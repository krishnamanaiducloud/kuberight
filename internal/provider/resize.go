package provider

import (
	"context"
	"fmt"
	"sort"
	"time"

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
	// changed is when KubeRight last wrote the workload, or zero.
	changed time.Time
}

// lastAutomatedWrite reads, from the managed fields, when KubeRight last
// patched a workload. The zero time means it is not recorded.
func lastAutomatedWrite(meta metav1.ObjectMeta) time.Time {
	var last time.Time
	for _, entry := range meta.ManagedFields {
		if entry.Manager == automation.FieldManager && entry.Time != nil && entry.Time.After(last) {
			last = entry.Time.Time
		}
	}
	return last
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
				return resizeTarget{item.UID, item.Spec.UpdateStrategy.Type == appsv1.OnDeleteStatefulSetStrategyType, item.Spec.Selector, item.Spec.Template.Spec, lastAutomatedWrite(item.ObjectMeta)}, true, nil
			}
		}
	case "DaemonSet":
		list, err := apps.DaemonSets(change.Namespace).List(ctx, options)
		if err != nil {
			return resizeTarget{}, false, err
		}
		for _, item := range list.Items {
			if item.Name == change.Workload {
				return resizeTarget{item.UID, item.Spec.UpdateStrategy.Type == appsv1.OnDeleteDaemonSetStrategyType, item.Spec.Selector, item.Spec.Template.Spec, lastAutomatedWrite(item.ObjectMeta)}, true, nil
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

// carriedSince is when a pod took the resources it carries: its last
// in-place resize by KubeRight, which the API server records in the pod's
// managed fields, or else its creation.
func carriedSince(pod corev1.Pod) time.Time {
	since := pod.CreationTimestamp.Time
	for _, entry := range pod.ManagedFields {
		if entry.Manager == automation.FieldManager && entry.Subresource == "resize" && entry.Time != nil && entry.Time.After(since) {
			since = entry.Time.Time
		}
	}
	return since
}

// carries reports whether the pod spec already holds the resources for the
// container.
func carries(pod corev1.Pod, container string, want automation.Resources) bool {
	for _, item := range pod.Spec.Containers {
		if item.Name == container {
			requests, limits := item.Resources.Requests, item.Resources.Limits
			return requests.Cpu().MilliValue() == want.CPURequestMilli && limits.Cpu().MilliValue() == want.CPULimitMilli &&
				requests.Memory().Value() == want.MemoryRequestBytes && limits.Memory().Value() == want.MemoryLimitBytes
		}
	}
	return false
}

// unsettled reports whether a pod that carries the new resources is no
// evidence that they work: it is not ready, the kubelet has not finished
// (or cannot finish) its resize, or the container restarted after the
// change was written.
func unsettled(pod corev1.Pod, container string, changed time.Time) bool {
	ready := false
	for _, condition := range pod.Status.Conditions {
		switch condition.Type {
		case corev1.PodReady:
			ready = condition.Status == corev1.ConditionTrue
		case corev1.PodResizePending, corev1.PodResizeInProgress:
			if condition.Status == corev1.ConditionTrue {
				return true
			}
		}
	}
	if !ready {
		return true
	}
	for _, status := range pod.Status.ContainerStatuses {
		if terminated := status.LastTerminationState.Terminated; status.Name == container && terminated != nil && !changed.IsZero() && terminated.FinishedAt.After(changed) {
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
//
// A change is resized in stages, so new values that harm a pod do not reach
// every replica at once. Each call resizes as many pods as already carry
// the new resources, and at least one: one pod, then one more, then two,
// four, and so on. It resizes nothing while a pod that carries them is
// unsettled: not ready, not done resizing, or restarted after the change was
// written. A pod that is not ready or is resizing for another reason is
// left for a later pass. A stage also waits until the pods resized before it
// have carried the new resources for change.StageGap. That time is read from
// the cluster, so it holds however many replicas of KubeRight run passes. A
// rollback restores values that ran before: it is not staged and reaches
// every running pod.
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
	var behind []string
	current, unhealthy := 0, ""
	waiting, unready := 0, ""
	var newest time.Time
	for _, pod := range pods.Items {
		if !ownedByUID(pod, target.uid) || pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
			continue
		}
		result.Pods++
		identity := podIdentity(pod)
		switch {
		case !carries(pod, change.Container, change.Desired):
			// A pod that is already unwell is no test of new values and
			// should not be given them; a rollback reaches it all the same.
			if !change.Rollback && identity.Unsettled(change.Container, time.Time{}) {
				if waiting++; unready == "" {
					unready = pod.Name
				}
				continue
			}
			behind = append(behind, pod.Name)
		case identity.Unsettled(change.Container, target.changed) && unhealthy == "":
			current, unhealthy = current+1, pod.Name
		default:
			current++
		}
		if since := carriedSince(pod); carries(pod, change.Container, change.Desired) && since.After(newest) {
			newest = since
		}
	}
	batch := len(behind)
	if !change.Rollback {
		switch {
		case unhealthy != "" && len(behind)+waiting > 0:
			result.Halted, result.Remaining = true, len(behind)+waiting
			result.Error = fmt.Sprintf("pod %s carries the new resources and is not ready, has not finished resizing, or restarted after the change; %d other running pods keep their resources", unhealthy, result.Remaining)
			return result, nil
		case current > 0 && len(behind)+waiting > 0 && time.Since(newest) < change.StageGap:
			carried := time.Since(newest)
			return automation.ResizeResult{Pods: result.Pods, Remaining: len(behind) + waiting,
				Reason: fmt.Sprintf("the pods resized last have carried the new resources for %s; the next stage follows in %s", carried.Round(time.Second), (change.StageGap - carried).Round(time.Second))}, nil
		case len(behind) == 0 && waiting > 0:
			result.Halted, result.Remaining = true, waiting
			result.Error = fmt.Sprintf("pod %s is not ready or is still resizing; pods left on their resources until they are ready: %d", unready, waiting)
			return result, nil
		}
		batch = max(1, current)
		// The highest ordinal goes first, as in a StatefulSet rolling
		// update: ordinal 0 is often the primary.
		sort.Slice(behind, func(left, right int) bool {
			if len(behind[left]) != len(behind[right]) {
				return len(behind[left]) > len(behind[right])
			}
			return behind[left] > behind[right]
		})
	}
	// A rejected pod does not use up the batch, so one pod the cluster
	// refuses to resize cannot block the others forever.
	for index, name := range behind {
		if result.Resized >= batch {
			break
		}
		_, err := provider.client.CoreV1().Pods(change.Namespace).Patch(ctx, name, types.StrategicMergePatchType, body, metav1.PatchOptions{FieldManager: automation.FieldManager}, "resize")
		switch {
		case err == nil:
			result.Resized++
			continue
		case apierrors.IsForbidden(err):
			// Without the permission every pod fails the same way.
			result.Failed, result.Error = len(behind)-result.Resized, "the pods/resize permission is not granted"
			return result, nil
		case apierrors.IsMethodNotSupported(err) || (apierrors.IsNotFound(err) && index == 0):
			// A cluster without the resize subresource answers 404 or 405
			// on the first pod.
			result.Failed, result.Error = len(behind)-result.Resized, "this cluster does not serve the pods/resize subresource"
			return result, nil
		}
		result.Failed++
		if result.Error == "" {
			result.Error = fmt.Sprintf("pod %s: %s", name, safeKubernetesError(err))
		}
	}
	result.Remaining = len(behind) - result.Resized - result.Failed + waiting
	return result, nil
}
