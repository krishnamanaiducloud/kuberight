package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/kuberight/kuberight/internal/httpapi"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	scaledObjectsResource = schema.GroupVersionResource{Group: "keda.sh", Version: "v1alpha1", Resource: "scaledobjects"}
	vpaResource           = schema.GroupVersionResource{Group: "autoscaling.k8s.io", Version: "v1", Resource: "verticalpodautoscalers"}
)

var _ httpapi.WorkloadResolver = (*KubernetesProvider)(nil)

// Workload reads one workload's live resource configuration and autoscaler
// context. It uses list calls with a name field selector so it needs no
// permissions beyond the inventory's existing list access.
func (provider *KubernetesProvider) Workload(ctx context.Context, namespace, kind, name string) (httpapi.WorkloadSpec, error) {
	if provider.namespace != "" && namespace != provider.namespace {
		return httpapi.WorkloadSpec{}, httpapi.ErrWorkloadOutOfScope
	}
	options := metav1.ListOptions{FieldSelector: fields.OneTermEqualSelector("metadata.name", name).String()}
	var (
		replicas           int32
		podSpec            corev1.PodSpec
		identity           metav1.ObjectMeta
		workloadObjectSpec any
		found              bool
		err                error
	)
	switch kind {
	case "Deployment":
		list, listErr := provider.client.AppsV1().Deployments(namespace).List(ctx, options)
		err = listErr
		if listErr == nil {
			for index := range list.Items {
				if item := &list.Items[index]; item.Name == name {
					replicas, podSpec, found = defaultReplicas(item.Spec.Replicas), item.Spec.Template.Spec, true
					identity, workloadObjectSpec = item.ObjectMeta, item.Spec
				}
			}
		}
	case "StatefulSet":
		list, listErr := provider.client.AppsV1().StatefulSets(namespace).List(ctx, options)
		err = listErr
		if listErr == nil {
			for index := range list.Items {
				if item := &list.Items[index]; item.Name == name {
					replicas, podSpec, found = defaultReplicas(item.Spec.Replicas), item.Spec.Template.Spec, true
					identity, workloadObjectSpec = item.ObjectMeta, item.Spec
				}
			}
		}
	case "DaemonSet":
		list, listErr := provider.client.AppsV1().DaemonSets(namespace).List(ctx, options)
		err = listErr
		if listErr == nil {
			for index := range list.Items {
				if item := &list.Items[index]; item.Name == name {
					replicas, podSpec, found = item.Status.DesiredNumberScheduled, item.Spec.Template.Spec, true
					identity, workloadObjectSpec = item.ObjectMeta, item.Spec
				}
			}
		}
	case "ReplicaSet":
		list, listErr := provider.client.AppsV1().ReplicaSets(namespace).List(ctx, options)
		err = listErr
		if listErr == nil {
			for index := range list.Items {
				if item := &list.Items[index]; item.Name == name {
					replicas, podSpec, found = defaultReplicas(item.Spec.Replicas), item.Spec.Template.Spec, true
					identity, workloadObjectSpec = item.ObjectMeta, item.Spec
				}
			}
		}
	case "Job":
		list, listErr := provider.client.BatchV1().Jobs(namespace).List(ctx, options)
		err = listErr
		if listErr == nil {
			for index := range list.Items {
				if item := &list.Items[index]; item.Name == name {
					replicas, podSpec, found = defaultReplicas(item.Spec.Parallelism), item.Spec.Template.Spec, true
					identity, workloadObjectSpec = item.ObjectMeta, item.Spec
				}
			}
		}
	case "CronJob":
		list, listErr := provider.client.BatchV1().CronJobs(namespace).List(ctx, options)
		err = listErr
		if listErr == nil {
			for index := range list.Items {
				if item := &list.Items[index]; item.Name == name {
					replicas, podSpec, found = defaultReplicas(item.Spec.JobTemplate.Spec.Parallelism), item.Spec.JobTemplate.Spec.Template.Spec, true
					identity, workloadObjectSpec = item.ObjectMeta, item.Spec
				}
			}
		}
	default:
		return httpapi.WorkloadSpec{}, fmt.Errorf("%w: unsupported workload kind", httpapi.ErrWorkloadNotFound)
	}
	if err != nil {
		return httpapi.WorkloadSpec{}, fmt.Errorf("read %s: %s: %w", kind, safeKubernetesError(err), errInventoryUnavailable)
	}
	if !found {
		return httpapi.WorkloadSpec{}, httpapi.ErrWorkloadNotFound
	}
	specFingerprint, err := fingerprintWorkloadSpec(workloadObjectSpec)
	if err != nil {
		return httpapi.WorkloadSpec{}, fmt.Errorf("fingerprint %s spec: %w", kind, errInventoryUnavailable)
	}

	spec := httpapi.WorkloadSpec{
		Cluster: provider.clusterID, Namespace: namespace, Kind: kind, Name: name,
		UID: string(identity.UID), Generation: identity.Generation, ResourceVersion: identity.ResourceVersion, SourceSpecFingerprint: specFingerprint, Replicas: replicas,
		Containers: make([]httpapi.ContainerSpec, 0, len(podSpec.Containers)), Warnings: []string{},
	}
	for _, container := range podSpec.Containers {
		spec.Containers = append(spec.Containers, containerSpec(container))
	}
	for _, container := range podSpec.InitContainers {
		sidecar := container.RestartPolicy != nil && *container.RestartPolicy == corev1.ContainerRestartPolicyAlways
		spec.InitContainers = append(spec.InitContainers, httpapi.InitContainerSpec{ContainerSpec: containerSpec(container), Sidecar: sidecar})
	}
	spec.OverheadCPUMilli = podSpec.Overhead.Cpu().MilliValue()
	spec.OverheadMemoryBytes = podSpec.Overhead.Memory().Value()
	provider.applyHPA(ctx, &spec)
	provider.applyKEDA(ctx, &spec)
	provider.applyVPA(ctx, &spec)
	spec.MetricIdentity = provider.workloadMetricIdentity(ctx, spec, identity)
	return spec, nil
}

func containerSpec(container corev1.Container) httpapi.ContainerSpec {
	return httpapi.ContainerSpec{
		Name:               container.Name,
		CPURequestMilli:    container.Resources.Requests.Cpu().MilliValue(),
		CPULimitMilli:      container.Resources.Limits.Cpu().MilliValue(),
		MemoryRequestBytes: container.Resources.Requests.Memory().Value(),
		MemoryLimitBytes:   container.Resources.Limits.Memory().Value(),
	}
}

func fingerprintWorkloadSpec(spec any) (string, error) {
	payload, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func defaultReplicas(value *int32) int32 {
	if value == nil {
		return 1
	}
	return *value
}

func (provider *KubernetesProvider) applyHPA(ctx context.Context, spec *httpapi.WorkloadSpec) {
	hpas, err := listAll(ctx, func(ctx context.Context, options metav1.ListOptions) ([]autoscalingv2.HorizontalPodAutoscaler, string, error) {
		result, listErr := provider.client.AutoscalingV2().HorizontalPodAutoscalers(spec.Namespace).List(ctx, options)
		if listErr != nil {
			return nil, "", listErr
		}
		return result.Items, result.Continue, nil
	})
	if err != nil {
		spec.Warnings = append(spec.Warnings, "HPA state could not be verified: "+safeKubernetesError(err))
		return
	}
	for index := range hpas {
		hpa := &hpas[index]
		if hpa.Spec.ScaleTargetRef.Kind != spec.Kind || hpa.Spec.ScaleTargetRef.Name != spec.Name {
			continue
		}
		if hpa.Status.CurrentReplicas > 0 {
			spec.Replicas = hpa.Status.CurrentReplicas
		}
		details := &httpapi.HPASpec{Name: hpa.Name, MinReplicas: defaultReplicas(hpa.Spec.MinReplicas), MaxReplicas: hpa.Spec.MaxReplicas, CurrentReplicas: hpa.Status.CurrentReplicas}
		for _, metric := range hpa.Spec.Metrics {
			switch {
			case metric.Type == autoscalingv2.ResourceMetricSourceType && metric.Resource != nil &&
				metric.Resource.Name == corev1.ResourceCPU && metric.Resource.Target.Type == autoscalingv2.UtilizationMetricType:
				spec.HPAUtilizationBased = true
				if metric.Resource.Target.AverageUtilization != nil {
					details.TargetCPUUtilization = *metric.Resource.Target.AverageUtilization
				}
			case metric.Type == autoscalingv2.ContainerResourceMetricSourceType && metric.ContainerResource != nil &&
				metric.ContainerResource.Name == corev1.ResourceCPU && metric.ContainerResource.Target.Type == autoscalingv2.UtilizationMetricType:
				spec.HPAUtilizationBased = true
				if metric.ContainerResource.Target.AverageUtilization != nil {
					details.TargetCPUUtilization = *metric.ContainerResource.Target.AverageUtilization
					details.Container = metric.ContainerResource.Container
				}
			}
		}
		if details.TargetCPUUtilization > 0 {
			spec.HPA = details
		}
	}
}

func (provider *KubernetesProvider) listOptional(ctx context.Context, resource schema.GroupVersionResource, namespace, label string, spec *httpapi.WorkloadSpec) []unstructured.Unstructured {
	items, err := listAll(ctx, func(ctx context.Context, options metav1.ListOptions) ([]unstructured.Unstructured, string, error) {
		result, listErr := provider.dynamic.Resource(resource).Namespace(namespace).List(ctx, options)
		if listErr != nil {
			return nil, "", listErr
		}
		return result.Items, result.GetContinue(), nil
	})
	if err != nil {
		// An absent CRD means the autoscaler is not installed, which is a
		// verified absence. Any other failure leaves the state unknown.
		if !apierrors.IsNotFound(err) {
			spec.Warnings = append(spec.Warnings, label+" state could not be verified: "+safeKubernetesError(err))
		}
		return nil
	}
	return items
}

func (provider *KubernetesProvider) applyKEDA(ctx context.Context, spec *httpapi.WorkloadSpec) {
	for _, item := range provider.listOptional(ctx, scaledObjectsResource, spec.Namespace, "KEDA", spec) {
		targetName, _, _ := unstructured.NestedString(item.Object, "spec", "scaleTargetRef", "name")
		targetKind, _, _ := unstructured.NestedString(item.Object, "spec", "scaleTargetRef", "kind")
		if targetKind == "" {
			targetKind = "Deployment"
		}
		if targetName == spec.Name && targetKind == spec.Kind {
			spec.KEDAEnabled = true
			triggers, _, _ := unstructured.NestedSlice(item.Object, "spec", "triggers")
			for _, trigger := range triggers {
				if values, ok := trigger.(map[string]any); ok {
					if kind, ok := values["type"].(string); ok && kind != "" && len(spec.KEDATriggers) < 32 {
						spec.KEDATriggers = append(spec.KEDATriggers, kind)
					}
				}
			}
		}
	}
}

func (provider *KubernetesProvider) applyVPA(ctx context.Context, spec *httpapi.WorkloadSpec) {
	for _, item := range provider.listOptional(ctx, vpaResource, spec.Namespace, "VPA", spec) {
		targetName, _, _ := unstructured.NestedString(item.Object, "spec", "targetRef", "name")
		targetKind, _, _ := unstructured.NestedString(item.Object, "spec", "targetRef", "kind")
		if targetName != spec.Name || targetKind != spec.Kind {
			continue
		}
		if mode, _, _ := unstructured.NestedString(item.Object, "spec", "updatePolicy", "updateMode"); mode == "Off" {
			continue
		}
		policies, _, _ := unstructured.NestedSlice(item.Object, "spec", "resourcePolicy", "containerPolicies")
		for index := range spec.Containers {
			container := &spec.Containers[index]
			cpu, memory := vpaControl(policies, container.Name)
			container.VPAControlsCPU = container.VPAControlsCPU || cpu
			container.VPAControlsMemory = container.VPAControlsMemory || memory
		}
	}
}

// vpaControl applies VPA container-policy precedence: an exact container
// name wins over the "*" wildcard, mode Off disables control, and
// controlledResources defaults to both CPU and memory.
func vpaControl(policies []any, container string) (cpu bool, memory bool) {
	var selected map[string]any
	for _, candidate := range policies {
		policy, ok := candidate.(map[string]any)
		if !ok {
			continue
		}
		name, _ := policy["containerName"].(string)
		if name == container {
			selected = policy
			break
		}
		if name == "*" && selected == nil {
			selected = policy
		}
	}
	if selected == nil {
		return true, true
	}
	if mode, _ := selected["mode"].(string); mode == "Off" {
		return false, false
	}
	resources, ok := selected["controlledResources"].([]any)
	if !ok {
		return true, true
	}
	for _, resource := range resources {
		switch resource {
		case "cpu":
			cpu = true
		case "memory":
			memory = true
		}
	}
	return cpu, memory
}
