package provider

import (
	"context"
	"fmt"

	"github.com/kuberight/kuberight/internal/automation"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
)

// resizeTarget is the part of an OnDelete workload in-place resize needs.
type resizeTarget struct {
	uid      types.UID
	onDelete bool
	selector *metav1.LabelSelector
	template corev1.PodSpec
}

// resizeTarget reads the workload with the list permission the inventory
// already has. Deployments always roll out a template change, so only
// StatefulSets and DaemonSets can qualify.
func (provider *KubernetesProvider) resizeTarget(ctx context.Context, change automation.Change) (resizeTarget, bool, error) {
	options := metav1.ListOptions{FieldSelector: fields.OneTermEqualSelector("metadata.name", change.Workload).String()}
	apps := provider.client.AppsV1()
	switch change.Kind {
	case "StatefulSet":
		list, err := apps.StatefulSets(change.Namespace).List(ctx, options)
		if err != nil {
			return resizeTarget{}, false, err
		}
		for _, item := range list.Items {
			if item.Name == change.Workload {
				return resizeTarget{item.UID, item.Spec.UpdateStrategy.Type == appsv1.OnDeleteStatefulSetStrategyType, item.Spec.Selector, item.Spec.Template.Spec}, true, nil
			}
		}
	case "DaemonSet":
		list, err := apps.DaemonSets(change.Namespace).List(ctx, options)
		if err != nil {
			return resizeTarget{}, false, err
		}
		for _, item := range list.Items {
			if item.Name == change.Workload {
				return resizeTarget{item.UID, item.Spec.UpdateStrategy.Type == appsv1.OnDeleteDaemonSetStrategyType, item.Spec.Selector, item.Spec.Template.Spec}, true, nil
			}
		}
	}
	return resizeTarget{}, false, nil
}

// restartsOnResize reports whether the container's resize policy restarts
// it for a resource the change touches. Such a resize would restart every
// pod at once, which the owner of an OnDelete workload has chosen to control.
func restartsOnResize(container corev1.Container, change automation.Change) bool {
	cpu := change.Desired.CPURequestMilli != change.Previous.CPURequestMilli || change.Desired.CPULimitMilli != change.Previous.CPULimitMilli
	memory := change.Desired.MemoryRequestBytes != change.Previous.MemoryRequestBytes || change.Desired.MemoryLimitBytes != change.Previous.MemoryLimitBytes
	for _, policy := range container.ResizePolicy {
		if policy.RestartPolicy != corev1.RestartContainer {
			continue
		}
		if (policy.ResourceName == corev1.ResourceCPU && cpu) || (policy.ResourceName == corev1.ResourceMemory && memory) {
			return true
		}
	}
	return false
}

func ownedByUID(pod corev1.Pod, uid types.UID) bool {
	for _, owner := range pod.OwnerReferences {
		if owner.Controller != nil && *owner.Controller && owner.UID == uid {
			return true
		}
	}
	return false
}

// ResizeInPlace resizes the running pods of an OnDelete StatefulSet or
// DaemonSet through the pods/resize subresource, after PatchResources has
// updated the template. It never deletes or restarts a pod: where in-place
// resize does not apply or the cluster rejects it, the pods keep their
// resources until their owner recreates them from the patched template.
func (provider *KubernetesProvider) ResizeInPlace(ctx context.Context, change automation.Change) (automation.ResizeResult, error) {
	skip := func(reason string) (automation.ResizeResult, error) {
		return automation.ResizeResult{Reason: reason}, nil
	}
	target, found, err := provider.resizeTarget(ctx, change)
	switch {
	case err != nil:
		return automation.ResizeResult{}, fmt.Errorf("read %s %s/%s: %s", change.Kind, change.Namespace, change.Workload, safeKubernetesError(err))
	case !found || !target.onDelete:
		return skip("the workload rolls the change out itself")
	}
	var container *corev1.Container
	for index := range target.template.Containers {
		if target.template.Containers[index].Name == change.Container {
			container = &target.template.Containers[index]
		}
	}
	switch {
	case container == nil:
		return skip("the container is not in the pod template")
	case restartsOnResize(*container, change):
		return skip("the container resize policy restarts it; pods take the new resources when recreated")
	case (change.Desired.CPULimitMilli > 0) != (change.Previous.CPULimitMilli > 0) || (change.Desired.MemoryLimitBytes > 0) != (change.Previous.MemoryLimitBytes > 0):
		return skip("in-place resize cannot add or remove a limit; pods take the new resources when recreated")
	}
	selector, err := metav1.LabelSelectorAsSelector(target.selector)
	if err != nil {
		return automation.ResizeResult{}, fmt.Errorf("read the selector of %s %s/%s: %w", change.Kind, change.Namespace, change.Workload, err)
	}
	pods, err := provider.client.CoreV1().Pods(change.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return automation.ResizeResult{}, fmt.Errorf("list pods of %s %s/%s: %s", change.Kind, change.Namespace, change.Workload, safeKubernetesError(err))
	}
	body, err := automation.ResizeBody(change)
	if err != nil {
		return automation.ResizeResult{}, err
	}
	result := automation.ResizeResult{Attempted: true}
	for _, pod := range pods.Items {
		if !ownedByUID(pod, target.uid) || pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
			continue
		}
		result.Pods++
		_, err := provider.client.CoreV1().Pods(change.Namespace).Patch(ctx, pod.Name, types.StrategicMergePatchType, body, metav1.PatchOptions{FieldManager: automation.FieldManager}, "resize")
		switch {
		case err == nil:
			result.Resized++
			continue
		case apierrors.IsForbidden(err):
			// Without the permission every pod fails the same way.
			result.Failed, result.Error = result.Pods-result.Resized, "the pods/resize permission is not granted"
			return result, nil
		case apierrors.IsMethodNotSupported(err) || (apierrors.IsNotFound(err) && result.Pods == 1):
			// A cluster without the resize subresource answers 404 or 405
			// on the first pod.
			result.Failed, result.Error = 1, "this cluster does not serve the pods/resize subresource"
			return result, nil
		}
		result.Failed++
		if result.Error == "" {
			result.Error = fmt.Sprintf("pod %s: %s", pod.Name, safeKubernetesError(err))
		}
	}
	return result, nil
}
