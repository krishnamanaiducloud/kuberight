package provider

import (
	"context"
	"fmt"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// blocksEvictions reports whether a PodDisruptionBudget allows no voluntary
// eviction even when every pod it covers is healthy. The disruption
// controller's status is used once it has observed the current spec;
// otherwise only the spec forms that always block are recognized.
func blocksEvictions(budget *policyv1.PodDisruptionBudget) bool {
	status := budget.Status
	if status.ObservedGeneration >= budget.Generation && status.ExpectedPods > 0 {
		return status.DesiredHealthy >= status.ExpectedPods
	}
	zero := func(value *intstr.IntOrString) bool {
		return value != nil && ((value.Type == intstr.Int && value.IntVal == 0) || (value.Type == intstr.String && value.StrVal == "0%"))
	}
	all := budget.Spec.MinAvailable != nil && budget.Spec.MinAvailable.Type == intstr.String && budget.Spec.MinAvailable.StrVal == "100%"
	return zero(budget.Spec.MaxUnavailable) || all
}

// disruptionPins returns, by "namespace/kind/name", the nodes of Deployment
// and StatefulSet pods that a PodDisruptionBudget keeps from being evicted,
// and one description per blocking budget. Consolidation cannot drain those
// pods, so node-impact estimates keep them where they run.
func (provider *KubernetesProvider) disruptionPins(ctx context.Context) (pins map[string][]string, blockers, warnings []string) {
	pins, blockers, warnings = map[string][]string{}, []string{}, []string{}
	budgets, err := listAll(ctx, func(ctx context.Context, options metav1.ListOptions) ([]policyv1.PodDisruptionBudget, string, error) {
		result, listErr := provider.client.PolicyV1().PodDisruptionBudgets("").List(ctx, options)
		if listErr != nil {
			return nil, "", listErr
		}
		return result.Items, result.Continue, nil
	})
	if err != nil {
		return pins, blockers, []string{"PodDisruptionBudgets could not be read, so the pods they protect are assumed to be movable: " + safeKubernetesError(err)}
	}
	sort.Slice(budgets, func(left, right int) bool {
		return budgets[left].Namespace+"/"+budgets[left].Name < budgets[right].Namespace+"/"+budgets[right].Name
	})
	deploymentsByReplicaSet := map[string]map[string]string{}
	seen := map[string]bool{}
	for index := range budgets {
		budget := &budgets[index]
		if budget.Spec.Selector == nil || !blocksEvictions(budget) {
			continue
		}
		selector, err := metav1.LabelSelectorAsSelector(budget.Spec.Selector)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("PodDisruptionBudget %s/%s has an invalid selector and was ignored", budget.Namespace, budget.Name))
			continue
		}
		pods, err := listAll(ctx, func(ctx context.Context, options metav1.ListOptions) ([]corev1.Pod, string, error) {
			options.LabelSelector = selector.String()
			result, listErr := provider.client.CoreV1().Pods(budget.Namespace).List(ctx, options)
			if listErr != nil {
				return nil, "", listErr
			}
			return result.Items, result.Continue, nil
		})
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("Pods protected by PodDisruptionBudget %s/%s could not be read: %s", budget.Namespace, budget.Name, safeKubernetesError(err)))
			continue
		}
		owners, found := deploymentsByReplicaSet[budget.Namespace]
		if !found {
			owners = map[string]string{}
			replicaSets, listErr := listAll(ctx, func(ctx context.Context, options metav1.ListOptions) ([]appsv1.ReplicaSet, string, error) {
				result, err := provider.client.AppsV1().ReplicaSets(budget.Namespace).List(ctx, options)
				if err != nil {
					return nil, "", err
				}
				return result.Items, result.Continue, nil
			})
			if listErr != nil {
				warnings = append(warnings, fmt.Sprintf("ReplicaSets in %s could not be read, so Deployment pods protected by PodDisruptionBudgets there are assumed to be movable: %s", budget.Namespace, safeKubernetesError(listErr)))
			}
			for _, replicaSet := range replicaSets {
				if parent, controlled := controllerOwner(replicaSet.OwnerReferences); controlled && parent.Kind == "Deployment" {
					owners[replicaSet.Name] = parent.Name
				}
			}
			deploymentsByReplicaSet[budget.Namespace] = owners
		}
		protected := 0
		for podIndex := range pods {
			pod := &pods[podIndex]
			if pod.Spec.NodeName == "" || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed || pod.DeletionTimestamp != nil {
				continue
			}
			owner, controlled := controllerOwner(pod.OwnerReferences)
			if !controlled {
				continue
			}
			key := ""
			switch owner.Kind {
			case "StatefulSet":
				key = budget.Namespace + "/StatefulSet/" + owner.Name
			case "ReplicaSet":
				if deployment := owners[owner.Name]; deployment != "" {
					key = budget.Namespace + "/Deployment/" + deployment
				}
			}
			// A pod covered by two blocking budgets is pinned once.
			if key == "" || seen[budget.Namespace+"/"+pod.Name] {
				continue
			}
			seen[budget.Namespace+"/"+pod.Name] = true
			pins[key] = append(pins[key], pod.Spec.NodeName)
			protected++
		}
		blockers = append(blockers, fmt.Sprintf("PodDisruptionBudget %s/%s allows no voluntary evictions; %d pods stay on their nodes", budget.Namespace, budget.Name, protected))
	}
	return pins, blockers, warnings
}
