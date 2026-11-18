package provider

import (
	"context"
	"errors"
	"testing"

	"github.com/kuberight/kuberight/internal/httpapi"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestNamespacePolicyReadsLimitRangesAndQuotas(t *testing.T) {
	client := kubernetesfake.NewSimpleClientset(
		&corev1.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: "defaults", Namespace: "shop"}, Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{
			{Type: corev1.LimitTypeContainer,
				Min:                  corev1.ResourceList{corev1.ResourceCPU: mustQuantity(t, "50m"), corev1.ResourceMemory: mustQuantity(t, "64Mi")},
				Max:                  corev1.ResourceList{corev1.ResourceCPU: mustQuantity(t, "4"), corev1.ResourceMemory: mustQuantity(t, "8Gi")},
				MaxLimitRequestRatio: corev1.ResourceList{corev1.ResourceCPU: mustQuantity(t, "4"), corev1.ResourceMemory: mustQuantity(t, "1.5")}},
			{Type: corev1.LimitTypePod, Max: corev1.ResourceList{corev1.ResourceCPU: mustQuantity(t, "8")}},
			{Type: corev1.LimitTypePersistentVolumeClaim},
		}}},
		&corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "team", Namespace: "shop"}, Status: corev1.ResourceQuotaStatus{
			Hard: corev1.ResourceList{corev1.ResourceCPU: mustQuantity(t, "10"), corev1.ResourceLimitsMemory: mustQuantity(t, "40Gi")},
			Used: corev1.ResourceList{corev1.ResourceCPU: mustQuantity(t, "7500m"), corev1.ResourceLimitsMemory: mustQuantity(t, "20Gi")},
		}},
		&corev1.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: "elsewhere", Namespace: "other"}},
	)
	policy, err := testProvider(client).NamespacePolicy(context.Background(), "shop")
	if err != nil {
		t.Fatal(err)
	}
	if len(policy.LimitRanges) != 2 || len(policy.Quotas) != 1 {
		t.Fatalf("policy %+v", policy)
	}
	container := policy.LimitRanges[0]
	if container.Type != "Container" || container.MinCPUMilli != 50 || container.MaxMemoryBytes != 8<<30 || container.MaxCPULimitRequestRatio != 4 || container.MaxMemoryLimitRequestRatio != 1.5 {
		t.Fatalf("container limits %+v", container)
	}
	if pod := policy.LimitRanges[1]; pod.Type != "Pod" || pod.MaxCPUMilli != 8000 || pod.MinMemoryBytes != 0 {
		t.Fatalf("pod limits %+v", pod)
	}
	quota := policy.Quotas[0]
	if quota.Hard.RequestsCPUMilli != 10_000 || quota.Used.RequestsCPUMilli != 7500 || quota.Hard.RequestsMemoryBytes != -1 || quota.Hard.LimitsMemoryBytes != 40<<30 || quota.Hard.LimitsCPUMilli != -1 {
		t.Fatalf("quota %+v", quota)
	}

	scoped := testProvider(client)
	scoped.namespace = "shop"
	if _, err := scoped.NamespacePolicy(context.Background(), "other"); !errors.Is(err, httpapi.ErrWorkloadOutOfScope) {
		t.Fatalf("scope err=%v", err)
	}
	for _, resource := range []string{"limitranges", "resourcequotas"} {
		denied := kubernetesfake.NewSimpleClientset()
		denied.PrependReactor("list", resource, func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("forbidden")
		})
		if _, err := testProvider(denied).NamespacePolicy(context.Background(), "shop"); err == nil {
			t.Fatalf("%s failure must be reported", resource)
		}
	}
}

func TestWorkloadReadsInitContainersSidecarsOverheadAndHPATarget(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	deployment := checkoutDeployment(t, 2)
	deployment.Spec.Template.Spec.InitContainers = []corev1.Container{
		{Name: "migrate", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: mustQuantity(t, "500m")}}},
		{Name: "mesh", RestartPolicy: &always, Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: mustQuantity(t, "64Mi")}}},
	}
	deployment.Spec.Template.Spec.Overhead = corev1.ResourceList{corev1.ResourceCPU: mustQuantity(t, "10m"), corev1.ResourceMemory: mustQuantity(t, "20Mi")}
	target := int32(65)
	minimum := int32(2)
	client := kubernetesfake.NewSimpleClientset(deployment, &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "checkout"}, MinReplicas: &minimum, MaxReplicas: 9,
			Metrics: []autoscalingv2.MetricSpec{{Type: autoscalingv2.ContainerResourceMetricSourceType, ContainerResource: &autoscalingv2.ContainerResourceMetricSource{
				Name: corev1.ResourceCPU, Container: "api", Target: autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: &target},
			}}},
		},
		Status: autoscalingv2.HorizontalPodAutoscalerStatus{CurrentReplicas: 4},
	})
	provider := withDynamicObjects(testProvider(client))
	spec, err := provider.Workload(context.Background(), "shop", "Deployment", "checkout")
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.InitContainers) != 2 || spec.InitContainers[0].Sidecar || !spec.InitContainers[1].Sidecar || spec.InitContainers[0].CPURequestMilli != 500 {
		t.Fatalf("init containers %+v", spec.InitContainers)
	}
	if spec.OverheadCPUMilli != 10 || spec.OverheadMemoryBytes != 20<<20 {
		t.Fatalf("overhead %d %d", spec.OverheadCPUMilli, spec.OverheadMemoryBytes)
	}
	if hpa := spec.HPA; hpa == nil || hpa.TargetCPUUtilization != 65 || hpa.Container != "api" || hpa.MinReplicas != 2 || hpa.MaxReplicas != 9 || hpa.CurrentReplicas != 4 {
		t.Fatalf("hpa %+v", spec.HPA)
	}
	if _, found := spec.Container("mesh"); !found || spec.IsInitContainer("mesh") || !spec.IsInitContainer("migrate") {
		t.Fatal("sidecars are sizable containers; run-to-completion init containers are not")
	}
	_ = appsv1.Deployment{}
}
