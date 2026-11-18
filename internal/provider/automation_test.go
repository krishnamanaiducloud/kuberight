package provider

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/kuberight/kuberight/internal/automation"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func automationDeployment() *appsv1.Deployment {
	resources := func(cpu, memory string) corev1.ResourceList {
		return corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(memory)}
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop", ResourceVersion: "41"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "api", Image: "app:1", Resources: corev1.ResourceRequirements{Requests: resources("1500m", "3Gi"), Limits: resources("2500m", "4Gi")}},
			{Name: "sidecar", Image: "proxy:1", Resources: corev1.ResourceRequirements{Requests: resources("100m", "64Mi")}},
		}}}},
	}
}

func TestPatchResourcesChangesOnlyTheTargetContainer(t *testing.T) {
	client := kubernetesfake.NewSimpleClientset(automationDeployment())
	var patched []byte
	client.PrependReactor("patch", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		patch := action.(k8stesting.PatchAction)
		if patch.GetPatchType() != "application/strategic-merge-patch+json" {
			t.Fatalf("patch type = %s", patch.GetPatchType())
		}
		patched = patch.GetPatch()
		return false, nil, nil
	})
	provider := testProvider(client)
	change := automation.Change{
		Namespace: "shop", Kind: "Deployment", Workload: "checkout", Container: "api", ResourceVersion: "41", RecommendationID: "rec_1",
		Desired:  automation.Resources{CPURequestMilli: 750, MemoryRequestBytes: 2 << 30, MemoryLimitBytes: 3 << 30},
		Previous: automation.Resources{CPURequestMilli: 1500, CPULimitMilli: 2500, MemoryRequestBytes: 3 << 30, MemoryLimitBytes: 4 << 30},
	}
	if err := provider.PatchResources(context.Background(), change); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(patched, &body); err != nil || body["metadata"].(map[string]any)["resourceVersion"] != "41" {
		t.Fatalf("patch body = %s err=%v", patched, err)
	}
	live, err := client.AppsV1().Deployments("shop").Get(context.Background(), "checkout", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	api, sidecar := live.Spec.Template.Spec.Containers[0], live.Spec.Template.Spec.Containers[1]
	if api.Resources.Requests.Cpu().MilliValue() != 750 || api.Resources.Requests.Memory().Value() != 2<<30 || api.Resources.Limits.Memory().Value() != 3<<30 {
		t.Fatalf("api resources = %+v", api.Resources)
	}
	if _, present := api.Resources.Limits[corev1.ResourceCPU]; present {
		t.Fatalf("the CPU limit should have been removed: %+v", api.Resources.Limits)
	}
	if sidecar.Resources.Requests.Cpu().MilliValue() != 100 || api.Image != "app:1" {
		t.Fatalf("other fields changed: sidecar=%+v image=%s", sidecar.Resources, api.Image)
	}
	if live.Annotations[automation.AppliedAnnotation] != "rec_1" || live.Annotations[automation.PreviousAnnotation] == "" {
		t.Fatalf("annotations = %v", live.Annotations)
	}
}

func TestPatchResourcesReportsConflictsAndRejectsUnsupportedKinds(t *testing.T) {
	client := kubernetesfake.NewSimpleClientset(automationDeployment())
	client.PrependReactor("patch", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, "checkout", errors.New("the object has been modified"))
	})
	provider := testProvider(client)
	change := automation.Change{Namespace: "shop", Kind: "Deployment", Workload: "checkout", Container: "api", ResourceVersion: "40", RecommendationID: "rec_1",
		Desired: automation.Resources{CPURequestMilli: 750, MemoryRequestBytes: 2 << 30}}
	if err := provider.PatchResources(context.Background(), change); !errors.Is(err, ErrAutomationConflict) {
		t.Fatalf("err = %v, want a conflict", err)
	}
	change.Kind = "CronJob"
	if err := provider.PatchResources(context.Background(), change); err == nil {
		t.Fatal("a CronJob was patched")
	}
}

func TestNamespaceAutomationOptIn(t *testing.T) {
	client := kubernetesfake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop", Labels: map[string]string{automation.NamespaceLabel: "enabled"}}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "payments", Labels: map[string]string{automation.NamespaceLabel: "true"}}},
	)
	provider := testProvider(client)
	for namespace, want := range map[string]bool{"shop": true, "payments": false, "missing": false} {
		if got, err := provider.NamespaceAutomationEnabled(context.Background(), namespace); err != nil || got != want {
			t.Fatalf("%s: enabled=%v err=%v, want %v", namespace, got, err, want)
		}
	}
}
