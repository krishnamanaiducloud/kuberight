package provider

import (
	"context"
	"fmt"
	"sort"

	"github.com/kuberight/kuberight/internal/httpapi"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func controllerOwner(references []metav1.OwnerReference) (metav1.OwnerReference, bool) {
	for _, reference := range references {
		if reference.Controller != nil && *reference.Controller && reference.Name != "" {
			return reference, true
		}
	}
	return metav1.OwnerReference{}, false
}

func ownerObject[T any](uid types.UID, name string, byUID map[types.UID]*T, byName map[string]*T) *T {
	if uid != "" {
		return byUID[uid]
	}
	return byName[name]
}

// PodOwners resolves each pod through Kubernetes owner references. A
// ReplicaSet is followed to its Deployment and a Job to its CronJob; unknown,
// missing, or recreated owners are omitted instead of guessed from pod names.
func (provider *KubernetesProvider) PodOwners(ctx context.Context, namespaces []string) ([]httpapi.PodOwner, error) {
	unique := make(map[string]struct{}, len(namespaces))
	orderedNamespaces := make([]string, 0, len(namespaces))
	for _, namespace := range namespaces {
		if namespace == "" {
			continue
		}
		if provider.namespace != "" && namespace != provider.namespace {
			return nil, fmt.Errorf("pod owner namespace %q is outside the configured inventory scope", namespace)
		}
		if _, seen := unique[namespace]; !seen {
			unique[namespace] = struct{}{}
			orderedNamespaces = append(orderedNamespaces, namespace)
		}
	}
	sort.Strings(orderedNamespaces)
	owners := make([]httpapi.PodOwner, 0)
	for _, namespace := range orderedNamespaces {
		pods, err := listAll(ctx, func(ctx context.Context, options metav1.ListOptions) ([]corev1.Pod, string, error) {
			result, listErr := provider.client.CoreV1().Pods(namespace).List(ctx, options)
			if listErr != nil {
				return nil, "", listErr
			}
			return result.Items, result.Continue, nil
		})
		if err != nil {
			return nil, fmt.Errorf("list pods for ownership: %s: %w", safeKubernetesError(err), errInventoryUnavailable)
		}
		replicaSets, err := listAll(ctx, func(ctx context.Context, options metav1.ListOptions) ([]appsv1.ReplicaSet, string, error) {
			result, listErr := provider.client.AppsV1().ReplicaSets(namespace).List(ctx, options)
			if listErr != nil {
				return nil, "", listErr
			}
			return result.Items, result.Continue, nil
		})
		if err != nil {
			return nil, fmt.Errorf("list ReplicaSets for ownership: %s: %w", safeKubernetesError(err), errInventoryUnavailable)
		}
		jobs, err := listAll(ctx, func(ctx context.Context, options metav1.ListOptions) ([]batchv1.Job, string, error) {
			result, listErr := provider.client.BatchV1().Jobs(namespace).List(ctx, options)
			if listErr != nil {
				return nil, "", listErr
			}
			return result.Items, result.Continue, nil
		})
		if err != nil {
			return nil, fmt.Errorf("list Jobs for ownership: %s: %w", safeKubernetesError(err), errInventoryUnavailable)
		}

		replicaSetsByUID, replicaSetsByName := make(map[types.UID]*appsv1.ReplicaSet, len(replicaSets)), make(map[string]*appsv1.ReplicaSet, len(replicaSets))
		for index := range replicaSets {
			replicaSet := &replicaSets[index]
			replicaSetsByUID[replicaSet.UID], replicaSetsByName[replicaSet.Name] = replicaSet, replicaSet
		}
		jobsByUID, jobsByName := make(map[types.UID]*batchv1.Job, len(jobs)), make(map[string]*batchv1.Job, len(jobs))
		for index := range jobs {
			job := &jobs[index]
			jobsByUID[job.UID], jobsByName[job.Name] = job, job
		}

		for index := range pods {
			pod := &pods[index]
			owner, found := controllerOwner(pod.OwnerReferences)
			if !found {
				continue
			}
			kind, workload := owner.Kind, owner.Name
			switch owner.Kind {
			case "ReplicaSet":
				replicaSet := ownerObject(owner.UID, owner.Name, replicaSetsByUID, replicaSetsByName)
				if replicaSet == nil {
					continue
				}
				if parent, controlled := controllerOwner(replicaSet.OwnerReferences); controlled {
					if parent.Kind != "Deployment" {
						continue
					}
					kind, workload = "Deployment", parent.Name
				}
			case "Job":
				job := ownerObject(owner.UID, owner.Name, jobsByUID, jobsByName)
				if job == nil {
					continue
				}
				if parent, controlled := controllerOwner(job.OwnerReferences); controlled {
					if parent.Kind != "CronJob" {
						continue
					}
					kind, workload = "CronJob", parent.Name
				}
			case "StatefulSet", "DaemonSet":
			default:
				continue
			}
			if len(owners) == maximumInventoryItems {
				return nil, fmt.Errorf("pod owner inventory exceeds the limit of %d items", maximumInventoryItems)
			}
			owners = append(owners, httpapi.PodOwner{Namespace: namespace, Pod: pod.Name, Kind: kind, Workload: workload})
		}
	}
	sort.Slice(owners, func(left, right int) bool {
		if owners[left].Namespace != owners[right].Namespace {
			return owners[left].Namespace < owners[right].Namespace
		}
		return owners[left].Pod < owners[right].Pod
	})
	return owners, nil
}
