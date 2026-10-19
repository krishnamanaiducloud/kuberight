// Package provider contains read-only integrations used by the HTTP API.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kuberight/kuberight/internal/config"
	"github.com/kuberight/kuberight/internal/httpapi"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	inventoryListLimit    int64 = 500
	maximumInventoryItems       = 10_000
)

var errInventoryUnavailable = errors.New("Kubernetes inventory sources are unavailable")

// KubernetesProvider reads inventory from the local cluster. It never owns a
// mutating client and therefore cannot apply recommendations.
type KubernetesProvider struct {
	clusterID string
	namespace string
	client    kubernetes.Interface
	dynamic   dynamic.Interface
	discover  func(context.Context) (*version.Info, error)
}

func listAll[T any](ctx context.Context, fetch func(context.Context, metav1.ListOptions) ([]T, string, error)) ([]T, error) {
	items := make([]T, 0)
	continueToken := ""
	seenTokens := make(map[string]struct{})
	for {
		pageItems, nextToken, err := fetch(ctx, metav1.ListOptions{Limit: inventoryListLimit, Continue: continueToken})
		if err != nil {
			return nil, err
		}
		if len(pageItems) > maximumInventoryItems-len(items) {
			return nil, fmt.Errorf("inventory exceeds the per-resource limit of %d items", maximumInventoryItems)
		}
		items = append(items, pageItems...)
		if nextToken == "" {
			return items, nil
		}
		if _, seen := seenTokens[nextToken]; seen {
			return nil, errors.New("Kubernetes API returned a repeated continuation token")
		}
		seenTokens[nextToken] = struct{}{}
		continueToken = nextToken
	}
}

// NewKubernetesProvider uses an in-cluster service-account credential with
// explicit API rate limits. The credential is retained only by client-go and
// is never serialized or logged.
func NewKubernetesProvider(cfg config.Config) (*KubernetesProvider, error) {
	restConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("load in-cluster configuration: %w", err)
	}
	restConfig.QPS = cfg.KubernetesQPS
	restConfig.Burst = cfg.KubernetesBurst
	restConfig.UserAgent = "kuberight/" + httpapi.Version
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes client: %w", err)
	}
	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes dynamic client: %w", err)
	}
	discoveryClient := client.Discovery()
	return &KubernetesProvider{
		clusterID: cfg.ClusterID,
		namespace: cfg.InventoryNamespace,
		client:    client,
		dynamic:   dynamicClient,
		discover: func(ctx context.Context) (*version.Info, error) {
			payload, discoveryErr := discoveryClient.RESTClient().Get().AbsPath("/version").Do(ctx).Raw()
			if discoveryErr != nil {
				return nil, discoveryErr
			}
			serverVersion := &version.Info{}
			if unmarshalErr := json.Unmarshal(payload, serverVersion); unmarshalErr != nil {
				return nil, fmt.Errorf("decode Kubernetes version: %w", unmarshalErr)
			}
			return serverVersion, nil
		},
	}, nil
}

// Ready checks the Kubernetes API independently of HTTP process health.
func (provider *KubernetesProvider) Ready(ctx context.Context) error {
	_, err := provider.discover(ctx)
	return err
}

// List implements the versioned collection contract. Unsupported integrations
// return an explicit unavailable status, never an invented empty-success state.
func (provider *KubernetesProvider) List(ctx context.Context, resource string, query httpapi.CollectionQuery) (httpapi.CollectionResult, error) {
	var records []map[string]any
	var warnings []string
	var err error
	switch resource {
	case "clusters":
		records, warnings, err = provider.clusters(ctx)
	case "namespaces":
		records, warnings, err = provider.namespaces(ctx)
	case "workloads":
		records, warnings, err = provider.workloads(ctx, query.Namespace)
	case "autoscalers":
		records, warnings, err = provider.autoscalers(ctx, query.Namespace)
	default:
		return httpapi.CollectionResult{
			Items:  []map[string]any{},
			Status: httpapi.Availability{Available: false, Source: resource, Reason: "the required integration is not configured"},
		}, nil
	}
	if err != nil {
		if errors.Is(err, errInventoryUnavailable) {
			return httpapi.CollectionResult{
				Items: []map[string]any{},
				Status: httpapi.Availability{
					Available: false,
					Source:    "kubernetes-api",
					Reason:    "Kubernetes inventory APIs are unavailable or access was denied",
				},
			}, nil
		}
		return httpapi.CollectionResult{}, err
	}
	filtered := filterAndSort(records, query)
	total := len(filtered)
	filtered = page(filtered, query.Page, query.PageSize)
	return httpapi.CollectionResult{
		Items: filtered,
		Total: total,
		Status: httpapi.Availability{
			Available: true,
			Source:    "kubernetes-api",
			Reason:    strings.Join(warnings, "; "),
		},
	}, nil
}

func (provider *KubernetesProvider) clusters(ctx context.Context) ([]map[string]any, []string, error) {
	serverVersion, err := provider.discover(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("discover Kubernetes version: %w", err)
	}
	nodes, err := listAll(ctx, func(ctx context.Context, options metav1.ListOptions) ([]corev1.Node, string, error) {
		result, listErr := provider.client.CoreV1().Nodes().List(ctx, options)
		if listErr != nil {
			return nil, "", listErr
		}
		return result.Items, result.Continue, nil
	})
	if err != nil {
		return []map[string]any{{
			"id": provider.clusterID, "name": provider.clusterID, "health": "connected",
			"nodeInventoryAvailable": false, "kubernetesVersion": serverVersion.GitVersion,
			"updatedAt": time.Now().UTC(),
		}}, []string{"node inventory unavailable: " + safeKubernetesError(err)}, nil
	}
	ready, cpuMilli, memoryBytes := 0, int64(0), int64(0)
	for _, node := range nodes {
		cpuMilli += node.Status.Allocatable.Cpu().MilliValue()
		memoryBytes += node.Status.Allocatable.Memory().Value()
		for _, condition := range node.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == "True" {
				ready++
				break
			}
		}
	}
	return []map[string]any{{
		"id": provider.clusterID, "name": provider.clusterID, "health": health(ready == len(nodes)),
		"nodes": len(nodes), "readyNodes": ready, "allocatableCPUMilli": cpuMilli,
		"allocatableMemoryBytes": memoryBytes, "kubernetesVersion": serverVersion.GitVersion,
		"updatedAt": time.Now().UTC(),
	}}, nil, nil
}

func (provider *KubernetesProvider) namespaces(ctx context.Context) ([]map[string]any, []string, error) {
	if provider.namespace != "" {
		return []map[string]any{namespaceRecord(provider.namespace, "scoped", time.Time{})}, []string{"namespace-scoped installation does not request cluster-wide namespace metadata"}, nil
	}
	namespaces, err := listAll(ctx, func(ctx context.Context, options metav1.ListOptions) ([]corev1.Namespace, string, error) {
		result, listErr := provider.client.CoreV1().Namespaces().List(ctx, options)
		if listErr != nil {
			return nil, "", listErr
		}
		return result.Items, result.Continue, nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("list namespaces: %w", err)
	}
	records := make([]map[string]any, 0, len(namespaces))
	for _, namespace := range namespaces {
		records = append(records, namespaceRecord(namespace.Name, string(namespace.Status.Phase), namespace.CreationTimestamp.Time))
	}
	return records, nil, nil
}

func namespaceRecord(name, phase string, created time.Time) map[string]any {
	record := map[string]any{"name": name, "status": strings.ToLower(phase)}
	if !created.IsZero() {
		record["createdAt"] = created.UTC()
	}
	return record
}

func (provider *KubernetesProvider) workloads(ctx context.Context, requestedNamespace string) ([]map[string]any, []string, error) {
	namespace := provider.effectiveNamespace(requestedNamespace)
	records := make([]map[string]any, 0)
	warnings := make([]string, 0)
	successfulSources := 0
	deployments, err := listAll(ctx, func(ctx context.Context, options metav1.ListOptions) ([]appsv1.Deployment, string, error) {
		result, listErr := provider.client.AppsV1().Deployments(namespace).List(ctx, options)
		if listErr != nil {
			return nil, "", listErr
		}
		return result.Items, result.Continue, nil
	})
	if err != nil {
		warnings = append(warnings, "deployments unavailable: "+safeKubernetesError(err))
	} else {
		successfulSources++
		for index := range deployments {
			item := &deployments[index]
			records = append(records, workloadRecord(provider.clusterID, "Deployment", item.Namespace, item.Name, int32Value(item.Spec.Replicas), item.Status.ReadyReplicas, item.Spec.Template.Spec.Containers))
		}
	}
	statefulSets, err := listAll(ctx, func(ctx context.Context, options metav1.ListOptions) ([]appsv1.StatefulSet, string, error) {
		result, listErr := provider.client.AppsV1().StatefulSets(namespace).List(ctx, options)
		if listErr != nil {
			return nil, "", listErr
		}
		return result.Items, result.Continue, nil
	})
	if err != nil {
		warnings = append(warnings, "statefulsets unavailable: "+safeKubernetesError(err))
	} else {
		successfulSources++
		for index := range statefulSets {
			item := &statefulSets[index]
			records = append(records, workloadRecord(provider.clusterID, "StatefulSet", item.Namespace, item.Name, int32Value(item.Spec.Replicas), item.Status.ReadyReplicas, item.Spec.Template.Spec.Containers))
		}
	}
	daemonSets, err := listAll(ctx, func(ctx context.Context, options metav1.ListOptions) ([]appsv1.DaemonSet, string, error) {
		result, listErr := provider.client.AppsV1().DaemonSets(namespace).List(ctx, options)
		if listErr != nil {
			return nil, "", listErr
		}
		return result.Items, result.Continue, nil
	})
	if err != nil {
		warnings = append(warnings, "daemonsets unavailable: "+safeKubernetesError(err))
	} else {
		successfulSources++
		for index := range daemonSets {
			item := &daemonSets[index]
			records = append(records, workloadRecord(provider.clusterID, "DaemonSet", item.Namespace, item.Name, item.Status.DesiredNumberScheduled, item.Status.NumberReady, item.Spec.Template.Spec.Containers))
		}
	}
	replicaSets, err := listAll(ctx, func(ctx context.Context, options metav1.ListOptions) ([]appsv1.ReplicaSet, string, error) {
		result, listErr := provider.client.AppsV1().ReplicaSets(namespace).List(ctx, options)
		if listErr != nil {
			return nil, "", listErr
		}
		return result.Items, result.Continue, nil
	})
	if err != nil {
		warnings = append(warnings, "replicasets unavailable: "+safeKubernetesError(err))
	} else {
		successfulSources++
		for index := range replicaSets {
			item := &replicaSets[index]
			if ownedByController(item.OwnerReferences) {
				continue
			}
			records = append(records, workloadRecord(provider.clusterID, "ReplicaSet", item.Namespace, item.Name, int32Value(item.Spec.Replicas), item.Status.ReadyReplicas, item.Spec.Template.Spec.Containers))
		}
	}
	jobs, err := listAll(ctx, func(ctx context.Context, options metav1.ListOptions) ([]batchv1.Job, string, error) {
		result, listErr := provider.client.BatchV1().Jobs(namespace).List(ctx, options)
		if listErr != nil {
			return nil, "", listErr
		}
		return result.Items, result.Continue, nil
	})
	if err != nil {
		warnings = append(warnings, "jobs unavailable: "+safeKubernetesError(err))
	} else {
		successfulSources++
		for index := range jobs {
			item := &jobs[index]
			records = append(records, workloadRecord(provider.clusterID, "Job", item.Namespace, item.Name, int32Value(item.Spec.Parallelism), item.Status.Active, item.Spec.Template.Spec.Containers))
		}
	}
	cronJobs, err := listAll(ctx, func(ctx context.Context, options metav1.ListOptions) ([]batchv1.CronJob, string, error) {
		result, listErr := provider.client.BatchV1().CronJobs(namespace).List(ctx, options)
		if listErr != nil {
			return nil, "", listErr
		}
		return result.Items, result.Continue, nil
	})
	if err != nil {
		warnings = append(warnings, "cronjobs unavailable: "+safeKubernetesError(err))
	} else {
		successfulSources++
		for index := range cronJobs {
			item := &cronJobs[index]
			record := workloadRecord(provider.clusterID, "CronJob", item.Namespace, item.Name, int32Value(item.Spec.JobTemplate.Spec.Parallelism), int32(len(item.Status.Active)), item.Spec.JobTemplate.Spec.Template.Spec.Containers)
			record["schedule"] = item.Spec.Schedule
			records = append(records, record)
		}
	}
	if successfulSources == 0 {
		return nil, nil, errInventoryUnavailable
	}
	return records, warnings, nil
}

func workloadRecord(clusterID, kind, namespace, name string, desired, ready int32, containers []corev1.Container) map[string]any {
	cpuRequests, cpuLimits, memoryRequests, memoryLimits := int64(0), int64(0), int64(0), int64(0)
	containerRecords := make([]map[string]any, 0, len(containers))
	for _, container := range containers {
		requestCPU := container.Resources.Requests.Cpu().MilliValue()
		requestMemory := container.Resources.Requests.Memory().Value()
		limitCPU := container.Resources.Limits.Cpu().MilliValue()
		limitMemory := container.Resources.Limits.Memory().Value()
		cpuRequests += requestCPU
		memoryRequests += requestMemory
		cpuLimits += limitCPU
		memoryLimits += limitMemory
		containerRecords = append(containerRecords, map[string]any{
			"name": container.Name, "cpuRequestMilli": requestCPU, "cpuLimitMilli": limitCPU,
			"memoryRequestBytes": requestMemory, "memoryLimitBytes": limitMemory,
		})
	}
	return map[string]any{
		"id": namespace + "/" + kind + "/" + name, "cluster": clusterID, "namespace": namespace,
		"kind": kind, "name": name, "desiredReplicas": desired, "readyReplicas": ready,
		"health": health(desired == 0 || ready >= desired), "cpuRequestMilli": cpuRequests,
		"cpuLimitMilli": cpuLimits, "memoryRequestBytes": memoryRequests,
		"memoryLimitBytes": memoryLimits, "containers": containerRecords,
	}
}

func (provider *KubernetesProvider) autoscalers(ctx context.Context, requestedNamespace string) ([]map[string]any, []string, error) {
	namespace := provider.effectiveNamespace(requestedNamespace)
	records := make([]map[string]any, 0)
	warnings := make([]string, 0)
	successfulSources := 0
	hpas, err := listAll(ctx, func(ctx context.Context, options metav1.ListOptions) ([]autoscalingv2.HorizontalPodAutoscaler, string, error) {
		result, listErr := provider.client.AutoscalingV2().HorizontalPodAutoscalers(namespace).List(ctx, options)
		if listErr != nil {
			return nil, "", listErr
		}
		return result.Items, result.Continue, nil
	})
	if err != nil {
		warnings = append(warnings, "HPA unavailable: "+safeKubernetesError(err))
	} else {
		successfulSources++
		for _, item := range hpas {
			records = append(records, map[string]any{
				"id": item.Namespace + "/HPA/" + item.Name, "namespace": item.Namespace,
				"kind": "HorizontalPodAutoscaler", "name": item.Name,
				"targetKind": item.Spec.ScaleTargetRef.Kind, "targetName": item.Spec.ScaleTargetRef.Name,
				"minReplicas": int32Value(item.Spec.MinReplicas), "maxReplicas": item.Spec.MaxReplicas,
				"currentReplicas": item.Status.CurrentReplicas, "desiredReplicas": item.Status.DesiredReplicas,
				"status": "detected", "warning": "CPU request changes can alter utilization-based HPA scaling behavior",
			})
		}
	}
	dynamicResources := []struct {
		gvr  schema.GroupVersionResource
		kind string
	}{
		{schema.GroupVersionResource{Group: "keda.sh", Version: "v1alpha1", Resource: "scaledobjects"}, "KEDAScaledObject"},
		{schema.GroupVersionResource{Group: "keda.sh", Version: "v1alpha1", Resource: "scaledjobs"}, "KEDAScaledJob"},
		{schema.GroupVersionResource{Group: "autoscaling.k8s.io", Version: "v1", Resource: "verticalpodautoscalers"}, "VerticalPodAutoscaler"},
	}
	for _, candidate := range dynamicResources {
		items, listErr := listAll(ctx, func(ctx context.Context, options metav1.ListOptions) ([]unstructured.Unstructured, string, error) {
			result, dynamicListErr := provider.dynamic.Resource(candidate.gvr).Namespace(namespace).List(ctx, options)
			if dynamicListErr != nil {
				return nil, "", dynamicListErr
			}
			return result.Items, result.GetContinue(), nil
		})
		if listErr != nil {
			// Optional API groups are expected to be absent on many clusters. Keep
			// the successful inventory and make the missing detector explainable.
			warnings = append(warnings, candidate.kind+" unavailable: "+safeKubernetesError(listErr))
			continue
		}
		successfulSources++
		for _, item := range items {
			targetKind, _, _ := unstructuredNestedString(item.Object, "spec", "scaleTargetRef", "kind")
			targetName, _, _ := unstructuredNestedString(item.Object, "spec", "scaleTargetRef", "name")
			records = append(records, map[string]any{
				"id":        item.GetNamespace() + "/" + candidate.kind + "/" + item.GetName(),
				"namespace": item.GetNamespace(), "kind": candidate.kind, "name": item.GetName(),
				"targetKind": targetKind, "targetName": targetName, "status": "detected",
				"warning": autoscalerWarning(candidate.kind),
			})
		}
	}
	if successfulSources == 0 {
		return nil, nil, errInventoryUnavailable
	}
	return records, warnings, nil
}

func unstructuredNestedString(object map[string]any, fields ...string) (string, bool, error) {
	current := any(object)
	for _, field := range fields {
		mapping, ok := current.(map[string]any)
		if !ok {
			return "", false, nil
		}
		current, ok = mapping[field]
		if !ok {
			return "", false, nil
		}
	}
	value, ok := current.(string)
	return value, ok, nil
}

func autoscalerWarning(kind string) string {
	if kind == "VerticalPodAutoscaler" {
		return "VPA-managed resources are excluded from automatic changes without an explicit policy"
	}
	return "request changes must be reviewed against this autoscaler's control loop"
}

func (provider *KubernetesProvider) effectiveNamespace(requested string) string {
	if provider.namespace != "" {
		return provider.namespace
	}
	return requested
}

func int32Value(value *int32) int32 {
	if value == nil {
		return 0
	}
	return *value
}

func ownedByController(references []metav1.OwnerReference) bool {
	for _, reference := range references {
		if reference.Controller != nil && *reference.Controller {
			return true
		}
	}
	return false
}

func safeKubernetesError(err error) string {
	switch {
	case apierrors.IsForbidden(err):
		return "permission denied"
	case apierrors.IsUnauthorized(err):
		return "authentication rejected"
	case apierrors.IsNotFound(err):
		return "API is not installed"
	case apierrors.IsTooManyRequests(err):
		return "Kubernetes API rate limit exceeded"
	case apierrors.IsTimeout(err), apierrors.IsServerTimeout(err), errors.Is(err, context.DeadlineExceeded):
		return "Kubernetes API request timed out"
	case errors.Is(err, context.Canceled):
		return "request canceled"
	case apierrors.IsServiceUnavailable(err):
		return "Kubernetes API is unavailable"
	default:
		return "Kubernetes API request failed"
	}
}

func health(ok bool) string {
	if ok {
		return "healthy"
	}
	return "degraded"
}

func filterAndSort(records []map[string]any, query httpapi.CollectionQuery) []map[string]any {
	filter := strings.ToLower(strings.TrimSpace(query.Filter))
	filtered := make([]map[string]any, 0, len(records))
	for _, record := range records {
		if query.Cluster != "" && fmt.Sprint(record["cluster"]) != query.Cluster {
			continue
		}
		if query.Namespace != "" && fmt.Sprint(record["namespace"]) != query.Namespace {
			continue
		}
		if filter != "" {
			matched := false
			for _, value := range record {
				if strings.Contains(strings.ToLower(fmt.Sprint(value)), filter) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		filtered = append(filtered, record)
	}
	sortField := query.Sort
	if sortField == "" {
		sortField = "name"
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		left, right := comparable(filtered[i][sortField]), comparable(filtered[j][sortField])
		if query.Order == "desc" {
			return left > right
		}
		return left < right
	})
	return filtered
}

func comparable(value any) string {
	switch item := value.(type) {
	case int:
		return fmt.Sprintf("%020d", item)
	case int32:
		return fmt.Sprintf("%020d", item)
	case int64:
		return fmt.Sprintf("%020d", item)
	case float64:
		return fmt.Sprintf("%030.9f", item)
	default:
		return strings.ToLower(fmt.Sprint(value))
	}
}

func page(records []map[string]any, pageNumber, pageSize int) []map[string]any {
	if pageNumber < 1 {
		pageNumber = 1
	}
	if pageSize < 1 {
		pageSize = 50
	}
	start64 := int64(pageNumber-1) * int64(pageSize)
	if start64 >= int64(len(records)) {
		return []map[string]any{}
	}
	start := int(start64)
	end := start + pageSize
	if end > len(records) {
		end = len(records)
	}
	return records[start:end]
}
