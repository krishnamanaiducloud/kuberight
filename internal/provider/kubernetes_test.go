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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/dynamic/fake"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestWorkloadInventoryUsesRealResourceValues(t *testing.T) {
	replicas := int32(3)
	client := kubernetesfake.NewSimpleClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "shop"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "api",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: mustQuantity(t, "250m"), corev1.ResourceMemory: mustQuantity(t, "128Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: mustQuantity(t, "1"), corev1.ResourceMemory: mustQuantity(t, "512Mi")},
				},
			}}}},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 2},
	})
	provider := testProvider(client)
	result, err := provider.List(context.Background(), "workloads", httpapi.CollectionQuery{Page: 1, PageSize: 20, Namespace: "shop"})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	records, ok := result.Items.([]map[string]any)
	if !ok || len(records) != 1 {
		t.Fatalf("records = %#v", result.Items)
	}
	if got := records[0]["cpuRequestMilli"]; got != int64(250) {
		t.Fatalf("cpuRequestMilli = %v, want 250", got)
	}
	if got := records[0]["cluster"]; got != "test" {
		t.Fatalf("cluster = %v, want test", got)
	}
	if got := records[0]["health"]; got != "degraded" {
		t.Fatalf("health = %v, want degraded", got)
	}
}

func TestAutoscalerInventoryExplainsControlLoopRisk(t *testing.T) {
	minimum := int32(2)
	client := kubernetesfake.NewSimpleClientset(&autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop"},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "api"},
			MinReplicas:    &minimum,
			MaxReplicas:    10,
		},
	})
	provider := testProvider(client)
	result, err := provider.List(context.Background(), "autoscalers", httpapi.CollectionQuery{Page: 1, PageSize: 20, Namespace: "shop"})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	records := result.Items.([]map[string]any)
	if len(records) != 1 || records[0]["kind"] != "HorizontalPodAutoscaler" {
		t.Fatalf("records = %#v", records)
	}
	if records[0]["warning"] == "" {
		t.Fatal("expected HPA interaction warning")
	}
}

func TestUnavailableIntegrationIsExplicit(t *testing.T) {
	provider := testProvider(kubernetesfake.NewSimpleClientset())
	result, err := provider.List(context.Background(), "metrics/history", httpapi.CollectionQuery{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if result.Status.Available || result.Status.Reason == "" {
		t.Fatalf("status = %#v", result.Status)
	}
}

func TestListAllFollowsContinuationTokens(t *testing.T) {
	calls := 0
	items, err := listAll(context.Background(), func(_ context.Context, options metav1.ListOptions) ([]string, string, error) {
		calls++
		if options.Limit != inventoryListLimit {
			t.Fatalf("limit = %d, want %d", options.Limit, inventoryListLimit)
		}
		switch options.Continue {
		case "":
			return []string{"one"}, "next", nil
		case "next":
			return []string{"two"}, "", nil
		default:
			t.Fatalf("unexpected continuation token %q", options.Continue)
			return nil, "", nil
		}
	})
	if err != nil {
		t.Fatalf("listAll() error = %v", err)
	}
	if calls != 2 || len(items) != 2 || items[0] != "one" || items[1] != "two" {
		t.Fatalf("calls=%d items=%v", calls, items)
	}
}

func TestListAllRejectsRepeatedContinuationToken(t *testing.T) {
	_, err := listAll(context.Background(), func(_ context.Context, _ metav1.ListOptions) ([]string, string, error) {
		return nil, "repeated", nil
	})
	if err == nil {
		t.Fatal("listAll() accepted a repeated continuation token")
	}
}

func TestListAllEnforcesInventoryBound(t *testing.T) {
	_, err := listAll(context.Background(), func(_ context.Context, _ metav1.ListOptions) ([]int, string, error) {
		return make([]int, maximumInventoryItems+1), "", nil
	})
	if err == nil {
		t.Fatal("listAll() accepted inventory above the configured bound")
	}
}

func TestWorkloadInventoryReportsAllSourcesUnavailable(t *testing.T) {
	client := kubernetesfake.NewSimpleClientset()
	client.Fake.PrependReactor("list", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(action.GetResource().GroupResource(), "", errors.New("sensitive dependency detail"))
	})
	provider := testProvider(client)
	result, err := provider.List(context.Background(), "workloads", httpapi.CollectionQuery{Page: 1, PageSize: 20})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if result.Status.Available || result.Status.Reason == "" {
		t.Fatalf("status = %#v, want explicit unavailable state", result.Status)
	}
}

func TestSafeKubernetesErrorDoesNotExposeDependencyDetails(t *testing.T) {
	err := apierrors.NewForbidden(schema.GroupResource{Resource: "deployments"}, "private-name", errors.New("https://internal.example/token"))
	if got := safeKubernetesError(err); got != "permission denied" {
		t.Fatalf("safeKubernetesError() = %q", got)
	}
}

func testProvider(client *kubernetesfake.Clientset) *KubernetesProvider {
	scheme := runtime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{
		{Group: "keda.sh", Version: "v1alpha1", Resource: "scaledobjects"}:               "ScaledObjectList",
		{Group: "keda.sh", Version: "v1alpha1", Resource: "scaledjobs"}:                  "ScaledJobList",
		{Group: "autoscaling.k8s.io", Version: "v1", Resource: "verticalpodautoscalers"}: "VerticalPodAutoscalerList",
	}
	return &KubernetesProvider{
		clusterID: "test", client: client, dynamic: fake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds),
		discover: func(context.Context) (*version.Info, error) { return &version.Info{GitVersion: "v1.35.0"}, nil },
	}
}

func mustQuantity(t *testing.T, value string) resource.Quantity {
	t.Helper()
	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		t.Fatalf("ParseQuantity(%q): %v", value, err)
	}
	return quantity
}
