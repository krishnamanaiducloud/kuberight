package provider

import (
	"context"
	"fmt"

	"github.com/kuberight/kuberight/internal/httpapi"
	"github.com/kuberight/kuberight/internal/optimizer"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ httpapi.NamespacePolicyReader = (*KubernetesProvider)(nil)

// NamespacePolicy reads the namespace's LimitRanges and ResourceQuotas so
// recommendations can be checked against admission before anyone applies
// them. It needs list access to both resources.
func (provider *KubernetesProvider) NamespacePolicy(ctx context.Context, namespace string) (optimizer.NamespacePolicy, error) {
	if provider.namespace != "" && namespace != provider.namespace {
		return optimizer.NamespacePolicy{}, httpapi.ErrWorkloadOutOfScope
	}
	ranges, err := listAll(ctx, func(ctx context.Context, options metav1.ListOptions) ([]corev1.LimitRange, string, error) {
		result, listErr := provider.client.CoreV1().LimitRanges(namespace).List(ctx, options)
		if listErr != nil {
			return nil, "", listErr
		}
		return result.Items, result.Continue, nil
	})
	if err != nil {
		return optimizer.NamespacePolicy{}, fmt.Errorf("list LimitRanges: %s: %w", safeKubernetesError(err), errInventoryUnavailable)
	}
	quotas, err := listAll(ctx, func(ctx context.Context, options metav1.ListOptions) ([]corev1.ResourceQuota, string, error) {
		result, listErr := provider.client.CoreV1().ResourceQuotas(namespace).List(ctx, options)
		if listErr != nil {
			return nil, "", listErr
		}
		return result.Items, result.Continue, nil
	})
	if err != nil {
		return optimizer.NamespacePolicy{}, fmt.Errorf("list ResourceQuotas: %s: %w", safeKubernetesError(err), errInventoryUnavailable)
	}
	return namespacePolicy(ranges, quotas), nil
}

func namespacePolicy(ranges []corev1.LimitRange, quotas []corev1.ResourceQuota) optimizer.NamespacePolicy {
	policy := optimizer.NamespacePolicy{LimitRanges: []optimizer.LimitRange{}, Quotas: []optimizer.ResourceQuota{}}
	for _, item := range ranges {
		for _, limit := range item.Spec.Limits {
			if limit.Type != corev1.LimitTypeContainer && limit.Type != corev1.LimitTypePod {
				continue
			}
			if len(policy.LimitRanges) == 32 {
				break
			}
			policy.LimitRanges = append(policy.LimitRanges, optimizer.LimitRange{
				Name: item.Name, Type: string(limit.Type),
				MinCPUMilli: milli(limit.Min, corev1.ResourceCPU), MaxCPUMilli: milli(limit.Max, corev1.ResourceCPU),
				MinMemoryBytes: value(limit.Min, corev1.ResourceMemory), MaxMemoryBytes: value(limit.Max, corev1.ResourceMemory),
				MaxCPULimitRequestRatio:    ratio(limit.MaxLimitRequestRatio, corev1.ResourceCPU),
				MaxMemoryLimitRequestRatio: ratio(limit.MaxLimitRequestRatio, corev1.ResourceMemory),
			})
		}
	}
	for _, quota := range quotas {
		if len(policy.Quotas) == 32 {
			break
		}
		policy.Quotas = append(policy.Quotas, optimizer.ResourceQuota{
			Name: quota.Name, Hard: quotaAmounts(quota.Status.Hard), Used: quotaAmounts(quota.Status.Used),
		})
	}
	return policy
}

// quotaAmounts reads requests and limits; a bare "cpu" or "memory" quota is
// a requests quota. Absent resources are -1, meaning unconstrained.
func quotaAmounts(list corev1.ResourceList) optimizer.QuotaAmounts {
	pick := func(names ...corev1.ResourceName) (resource.Quantity, bool) {
		for _, name := range names {
			if quantity, ok := list[name]; ok {
				return quantity, true
			}
		}
		return resource.Quantity{}, false
	}
	amounts := optimizer.QuotaAmounts{RequestsCPUMilli: -1, RequestsMemoryBytes: -1, LimitsCPUMilli: -1, LimitsMemoryBytes: -1}
	if quantity, ok := pick(corev1.ResourceRequestsCPU, corev1.ResourceCPU); ok {
		amounts.RequestsCPUMilli = quantity.MilliValue()
	}
	if quantity, ok := pick(corev1.ResourceRequestsMemory, corev1.ResourceMemory); ok {
		amounts.RequestsMemoryBytes = quantity.Value()
	}
	if quantity, ok := pick(corev1.ResourceLimitsCPU); ok {
		amounts.LimitsCPUMilli = quantity.MilliValue()
	}
	if quantity, ok := pick(corev1.ResourceLimitsMemory); ok {
		amounts.LimitsMemoryBytes = quantity.Value()
	}
	return amounts
}

func milli(list corev1.ResourceList, name corev1.ResourceName) int64 {
	if quantity, ok := list[name]; ok {
		return quantity.MilliValue()
	}
	return 0
}

func value(list corev1.ResourceList, name corev1.ResourceName) int64 {
	if quantity, ok := list[name]; ok {
		return quantity.Value()
	}
	return 0
}

func ratio(list corev1.ResourceList, name corev1.ResourceName) float64 {
	if quantity, ok := list[name]; ok {
		return quantity.AsApproximateFloat64()
	}
	return 0
}
