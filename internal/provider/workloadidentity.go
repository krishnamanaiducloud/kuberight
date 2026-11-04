package provider

import (
	"context"
	"fmt"
	"sort"

	"github.com/kuberight/kuberight/internal/httpapi"
	"github.com/kuberight/kuberight/internal/usage"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func exactController(references []metav1.OwnerReference, kind, name string, uid types.UID) bool {
	owner, ok := controllerOwner(references)
	return ok && owner.Kind == kind && owner.Name == name && owner.UID != "" && owner.UID == uid
}

func podIdentity(pod corev1.Pod) usage.PodIdentity {
	containers := make([]string, 0, len(pod.Spec.Containers)+len(pod.Spec.InitContainers))
	for _, container := range pod.Spec.Containers {
		containers = append(containers, container.Name)
	}
	for _, container := range pod.Spec.InitContainers {
		if container.RestartPolicy != nil && *container.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			containers = append(containers, container.Name)
		}
	}
	sort.Strings(containers)
	var deletedAt *metav1.Time
	if pod.DeletionTimestamp != nil {
		copy := pod.DeletionTimestamp.DeepCopy()
		deletedAt = copy
	}
	identity := usage.PodIdentity{
		Name: pod.Name, UID: string(pod.UID), CreatedAt: pod.CreationTimestamp.Time.UTC(), Phase: string(pod.Status.Phase), Containers: containers,
	}
	if deletedAt != nil {
		at := deletedAt.Time.UTC()
		identity.DeletedAt = &at
	}
	return identity
}

// workloadMetricIdentity follows immutable owner-reference UIDs from pods to
// the requested workload. A missing UID or API failure becomes a resolution
// issue; it is never replaced with a pod-name guess.
func (provider *KubernetesProvider) workloadMetricIdentity(ctx context.Context, spec httpapi.WorkloadSpec, metadata metav1.ObjectMeta) *usage.WorkloadIdentity {
	identity := &usage.WorkloadIdentity{
		UID: string(metadata.UID), Generation: metadata.Generation, CreatedAt: metadata.CreationTimestamp.Time.UTC(),
		ExpectedReplicas: spec.Replicas, Pods: []usage.PodIdentity{}, ResolutionIssues: []string{},
	}
	if metadata.UID == "" {
		identity.ResolutionIssues = append(identity.ResolutionIssues, "the workload API object has no UID")
	}
	if metadata.CreationTimestamp.IsZero() {
		identity.ResolutionIssues = append(identity.ResolutionIssues, "the workload API object has no creation timestamp")
	}
	pods, err := listAll(ctx, func(ctx context.Context, options metav1.ListOptions) ([]corev1.Pod, string, error) {
		result, listErr := provider.client.CoreV1().Pods(spec.Namespace).List(ctx, options)
		if listErr != nil {
			return nil, "", listErr
		}
		return result.Items, result.Continue, nil
	})
	if err != nil {
		identity.ResolutionIssues = append(identity.ResolutionIssues, "pod UID inventory is unavailable: "+safeKubernetesError(err))
		return identity
	}

	allowedOwners := map[types.UID]struct{}{}
	switch spec.Kind {
	case "Deployment":
		replicaSets, listErr := listAll(ctx, func(ctx context.Context, options metav1.ListOptions) ([]appsv1.ReplicaSet, string, error) {
			result, err := provider.client.AppsV1().ReplicaSets(spec.Namespace).List(ctx, options)
			if err != nil {
				return nil, "", err
			}
			return result.Items, result.Continue, nil
		})
		if listErr != nil {
			identity.ResolutionIssues = append(identity.ResolutionIssues, "ReplicaSet UID inventory is unavailable: "+safeKubernetesError(listErr))
			return identity
		}
		for _, replicaSet := range replicaSets {
			if replicaSet.UID != "" && exactController(replicaSet.OwnerReferences, "Deployment", spec.Name, metadata.UID) {
				allowedOwners[replicaSet.UID] = struct{}{}
			}
		}
	case "CronJob":
		jobs, listErr := listAll(ctx, func(ctx context.Context, options metav1.ListOptions) ([]batchv1.Job, string, error) {
			result, err := provider.client.BatchV1().Jobs(spec.Namespace).List(ctx, options)
			if err != nil {
				return nil, "", err
			}
			return result.Items, result.Continue, nil
		})
		if listErr != nil {
			identity.ResolutionIssues = append(identity.ResolutionIssues, "Job UID inventory is unavailable: "+safeKubernetesError(listErr))
			return identity
		}
		for _, job := range jobs {
			if job.UID != "" && exactController(job.OwnerReferences, "CronJob", spec.Name, metadata.UID) {
				allowedOwners[job.UID] = struct{}{}
			}
		}
	default:
		if metadata.UID != "" {
			allowedOwners[metadata.UID] = struct{}{}
		}
	}

	podOwnerKind := spec.Kind
	if spec.Kind == "Deployment" {
		podOwnerKind = "ReplicaSet"
	} else if spec.Kind == "CronJob" {
		podOwnerKind = "Job"
	}
	ambiguous := 0
	for _, pod := range pods {
		owner, ok := controllerOwner(pod.OwnerReferences)
		if !ok || owner.Kind != podOwnerKind {
			continue
		}
		if owner.UID == "" {
			// Count only a plausible same-name direct owner. Indirect owners
			// cannot be linked safely without their UID and are simply omitted.
			if podOwnerKind == spec.Kind && owner.Name == spec.Name {
				ambiguous++
			}
			continue
		}
		if _, ok := allowedOwners[owner.UID]; !ok {
			continue
		}
		if pod.UID == "" || pod.CreationTimestamp.IsZero() {
			ambiguous++
			continue
		}
		identity.Pods = append(identity.Pods, podIdentity(pod))
	}
	if ambiguous > 0 {
		identity.ResolutionIssues = append(identity.ResolutionIssues, fmt.Sprintf("%d plausible pods lack immutable UID metadata", ambiguous))
	}
	sort.Slice(identity.Pods, func(left, right int) bool {
		if identity.Pods[left].Name != identity.Pods[right].Name {
			return identity.Pods[left].Name < identity.Pods[right].Name
		}
		return identity.Pods[left].UID < identity.Pods[right].UID
	})
	return identity
}
