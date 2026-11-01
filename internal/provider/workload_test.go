package provider

import (
	"context"
	"errors"
	"testing"

	"github.com/kuberight/kuberight/internal/httpapi"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func checkoutDeployment(t *testing.T, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: "api", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: mustQuantity(t, "250m"), corev1.ResourceMemory: mustQuantity(t, "128Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: mustQuantity(t, "1"), corev1.ResourceMemory: mustQuantity(t, "512Mi")},
				}},
				{Name: "proxy"},
			}}},
		},
	}
}

func withDynamicObjects(provider *KubernetesProvider, objects ...runtime.Object) *KubernetesProvider {
	listKinds := map[schema.GroupVersionResource]string{
		scaledObjectsResource: "ScaledObjectList",
		vpaResource:           "VerticalPodAutoscalerList",
	}
	provider.dynamic = fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, objects...)
	return provider
}

func TestWorkloadResolvesResourcesAndAutoscalers(t *testing.T) {
	utilization := int32(70)
	client := kubernetesfake.NewSimpleClientset(
		checkoutDeployment(t, 2),
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "shop"}},
		&autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop"},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "checkout"},
				MaxReplicas:    10,
				Metrics: []autoscalingv2.MetricSpec{{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricSource{
					Name: corev1.ResourceCPU, Target: autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: &utilization},
				}}},
			},
			Status: autoscalingv2.HorizontalPodAutoscalerStatus{CurrentReplicas: 4},
		},
	)
	vpa := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "autoscaling.k8s.io/v1", "kind": "VerticalPodAutoscaler",
		"metadata": map[string]any{"name": "checkout", "namespace": "shop"},
		"spec": map[string]any{
			"targetRef":    map[string]any{"kind": "Deployment", "name": "checkout"},
			"updatePolicy": map[string]any{"updateMode": "Auto"},
			"resourcePolicy": map[string]any{"containerPolicies": []any{
				map[string]any{"containerName": "*", "controlledResources": []any{"memory"}},
				map[string]any{"containerName": "proxy", "mode": "Off"},
			}},
		},
	}}
	keda := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "keda.sh/v1alpha1", "kind": "ScaledObject",
		"metadata": map[string]any{"name": "checkout", "namespace": "shop"},
		"spec":     map[string]any{"scaleTargetRef": map[string]any{"name": "checkout"}},
	}}
	provider := withDynamicObjects(testProvider(client), vpa, keda)

	spec, err := provider.Workload(context.Background(), "shop", "Deployment", "checkout")
	if err != nil {
		t.Fatal(err)
	}
	api, ok := spec.Container("api")
	if !ok || api.CPURequestMilli != 250 || api.MemoryLimitBytes != 512<<20 {
		t.Fatalf("api container = %#v", api)
	}
	if spec.Replicas != 4 || !spec.HPAUtilizationBased || !spec.KEDAEnabled || spec.Cluster != "test" {
		t.Fatalf("spec = %#v", spec)
	}
	if api.VPAControlsCPU || !api.VPAControlsMemory {
		t.Fatalf("wildcard VPA policy must control only memory: %#v", api)
	}
	if proxy, _ := spec.Container("proxy"); proxy.VPAControlsCPU || proxy.VPAControlsMemory {
		t.Fatalf("mode Off must disable VPA control: %#v", proxy)
	}
	if len(spec.Warnings) != 0 {
		t.Fatalf("unexpected warnings %v", spec.Warnings)
	}
}

func TestWorkloadNotFoundAndScope(t *testing.T) {
	provider := withDynamicObjects(testProvider(kubernetesfake.NewSimpleClientset(checkoutDeployment(t, 1))))
	if _, err := provider.Workload(context.Background(), "shop", "Deployment", "missing"); !errors.Is(err, httpapi.ErrWorkloadNotFound) {
		t.Fatalf("got %v, want ErrWorkloadNotFound", err)
	}
	if _, err := provider.Workload(context.Background(), "shop", "StatefulSet", "checkout"); !errors.Is(err, httpapi.ErrWorkloadNotFound) {
		t.Fatalf("kind mismatch: got %v, want ErrWorkloadNotFound", err)
	}
	if _, err := provider.Workload(context.Background(), "shop", "Pod", "checkout"); !errors.Is(err, httpapi.ErrWorkloadNotFound) {
		t.Fatalf("unsupported kind: got %v, want ErrWorkloadNotFound", err)
	}
	provider.namespace = "billing"
	if _, err := provider.Workload(context.Background(), "shop", "Deployment", "checkout"); !errors.Is(err, httpapi.ErrWorkloadOutOfScope) {
		t.Fatalf("got %v, want ErrWorkloadOutOfScope", err)
	}
}

func TestWorkloadReportsUnverifiedAutoscalers(t *testing.T) {
	client := kubernetesfake.NewSimpleClientset(checkoutDeployment(t, 3))
	client.Fake.PrependReactor("list", "horizontalpodautoscalers", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(action.GetResource().GroupResource(), "", errors.New("denied"))
	})
	provider := withDynamicObjects(testProvider(client))
	spec, err := provider.Workload(context.Background(), "shop", "Deployment", "checkout")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Replicas != 3 || len(spec.Warnings) != 1 {
		t.Fatalf("spec = %#v", spec)
	}
}

func TestWorkloadPropagatesInventoryFailure(t *testing.T) {
	client := kubernetesfake.NewSimpleClientset()
	client.Fake.PrependReactor("list", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(action.GetResource().GroupResource(), "", errors.New("denied"))
	})
	_, err := withDynamicObjects(testProvider(client)).Workload(context.Background(), "shop", "Deployment", "checkout")
	if err == nil || errors.Is(err, httpapi.ErrWorkloadNotFound) {
		t.Fatalf("got %v, want an availability error", err)
	}
}

func TestVPAControlPrecedence(t *testing.T) {
	cpu, memory := vpaControl(nil, "app")
	if !cpu || !memory {
		t.Fatal("a VPA without container policies controls both resources")
	}
	policies := []any{
		map[string]any{"containerName": "*", "mode": "Off"},
		map[string]any{"containerName": "app", "controlledResources": []any{"cpu"}},
	}
	if cpu, memory = vpaControl(policies, "app"); !cpu || memory {
		t.Fatalf("exact policy must win: cpu=%v memory=%v", cpu, memory)
	}
	if cpu, memory = vpaControl(policies, "sidecar"); cpu || memory {
		t.Fatalf("wildcard Off must apply: cpu=%v memory=%v", cpu, memory)
	}
}
