package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/kuberight/kuberight/internal/httpapi"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var nodePoolResource = schema.GroupVersionResource{Group: "karpenter.sh", Version: "v1", Resource: "nodepools"}

var _ httpapi.CapacityReader = (*KubernetesProvider)(nil)

// Capacity reads node allocatable resources and node autoscaler settings.
// Node listing requires cluster-scoped inventory.
func (provider *KubernetesProvider) Capacity(ctx context.Context) (httpapi.ClusterCapacity, error) {
	nodes, err := listAll(ctx, func(ctx context.Context, options metav1.ListOptions) ([]corev1.Node, string, error) {
		result, listErr := provider.client.CoreV1().Nodes().List(ctx, options)
		if listErr != nil {
			return nil, "", listErr
		}
		return result.Items, result.Continue, nil
	})
	if err != nil {
		return httpapi.ClusterCapacity{}, fmt.Errorf("list nodes: %s: %w", safeKubernetesError(err), errInventoryUnavailable)
	}
	capacity := httpapi.ClusterCapacity{Nodes: make([]httpapi.NodeCapacity, 0, len(nodes)), Autoscaling: httpapi.NodeAutoscaling{NodePools: []httpapi.NodePool{}, Warnings: []string{}}}
	for index := range nodes {
		node := &nodes[index]
		ready := false
		for _, condition := range node.Status.Conditions {
			if condition.Type == corev1.NodeReady && condition.Status == corev1.ConditionTrue {
				ready = true
			}
		}
		labels := node.Labels
		capacityType := labels["karpenter.sh/capacity-type"]
		if capacityType == "" {
			capacityType = labels["eks.amazonaws.com/capacityType"]
		}
		if capacityType == "" && labels["cloud.google.com/gke-spot"] == "true" {
			capacityType = "spot"
		}
		if capacityType == "" && labels["kubernetes.azure.com/scalesetpriority"] == "spot" {
			capacityType = "spot"
		}
		capacity.Nodes = append(capacity.Nodes, httpapi.NodeCapacity{
			Name: node.Name, CPUMilli: node.Status.Allocatable.Cpu().MilliValue(), MemoryBytes: node.Status.Allocatable.Memory().Value(),
			Schedulable:  ready && !node.Spec.Unschedulable && labels["kubernetes.io/os"] != "windows",
			InstanceType: labels["node.kubernetes.io/instance-type"], CapacityType: capacityType, NodePool: labels["karpenter.sh/nodepool"],
		})
		if capacity.Nodes[len(capacity.Nodes)-1].NodePool != "" {
			capacity.Autoscaling.Karpenter = true
		}
	}

	pools, err := listAll(ctx, func(ctx context.Context, options metav1.ListOptions) ([]unstructured.Unstructured, string, error) {
		result, listErr := provider.dynamic.Resource(nodePoolResource).List(ctx, options)
		if listErr != nil {
			return nil, "", listErr
		}
		return result.Items, result.GetContinue(), nil
	})
	switch {
	case err == nil:
		for _, pool := range pools {
			capacity.Autoscaling.Karpenter = true
			capacity.Autoscaling.NodePools = append(capacity.Autoscaling.NodePools, nodePoolSummary(pool))
		}
	case apierrors.IsNotFound(err):
		// Karpenter's CRDs are not installed.
	default:
		capacity.Autoscaling.Warnings = append(capacity.Autoscaling.Warnings, "Karpenter NodePools could not be read: "+safeKubernetesError(err))
	}

	deployments, err := listAll(ctx, func(ctx context.Context, options metav1.ListOptions) ([]appsv1.Deployment, string, error) {
		result, listErr := provider.client.AppsV1().Deployments("").List(ctx, options)
		if listErr != nil {
			return nil, "", listErr
		}
		return result.Items, result.Continue, nil
	})
	if err == nil {
		for index := range deployments {
			name := deployments[index].Name
			if strings.Contains(name, "cluster-autoscaler") || deployments[index].Labels["app.kubernetes.io/name"] == "cluster-autoscaler" {
				capacity.Autoscaling.ClusterAutoscaler = true
			}
		}
	} else {
		capacity.Autoscaling.Warnings = append(capacity.Autoscaling.Warnings, "Cluster Autoscaler detection was unavailable: "+safeKubernetesError(err))
	}
	return capacity, nil
}

func nodePoolSummary(pool unstructured.Unstructured) httpapi.NodePool {
	summary := httpapi.NodePool{Name: pool.GetName(), DisruptionBudgets: []string{}}
	summary.ConsolidationPolicy, _, _ = unstructured.NestedString(pool.Object, "spec", "disruption", "consolidationPolicy")
	if summary.ConsolidationPolicy == "" {
		summary.ConsolidationPolicy = "WhenEmptyOrUnderutilized"
	}
	summary.ConsolidateAfter, _, _ = unstructured.NestedString(pool.Object, "spec", "disruption", "consolidateAfter")
	budgets, _, _ := unstructured.NestedSlice(pool.Object, "spec", "disruption", "budgets")
	for _, candidate := range budgets {
		budget, ok := candidate.(map[string]any)
		if !ok {
			continue
		}
		text := fmt.Sprint(budget["nodes"]) + " nodes"
		if schedule, ok := budget["schedule"].(string); ok && schedule != "" {
			text += " during " + schedule
			if duration, ok := budget["duration"].(string); ok {
				text += " for " + duration
			}
		}
		summary.DisruptionBudgets = append(summary.DisruptionBudgets, text)
	}
	summary.CPULimit, _, _ = unstructured.NestedString(pool.Object, "spec", "limits", "cpu")
	summary.MemoryLimit, _, _ = unstructured.NestedString(pool.Object, "spec", "limits", "memory")
	return summary
}
