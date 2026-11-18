package provider

import (
	"context"
	"errors"
	"fmt"

	"github.com/kuberight/kuberight/internal/automation"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
)

// ErrAutomationConflict means the workload changed after the change was
// evaluated; the patch was rejected by its resourceVersion precondition.
var ErrAutomationConflict = errors.New("the workload changed after evaluation")

// NamespaceAutomationEnabled reports whether a namespace opted in with the
// kuberight.io/automation=enabled label. It uses the read-only namespace
// list permission the inventory already has.
func (provider *KubernetesProvider) NamespaceAutomationEnabled(ctx context.Context, namespace string) (bool, error) {
	list, err := provider.client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{FieldSelector: fields.OneTermEqualSelector("metadata.name", namespace).String()})
	if err != nil {
		return false, fmt.Errorf("read namespace %s: %s", namespace, safeKubernetesError(err))
	}
	for _, item := range list.Items {
		if item.Name == namespace {
			return item.Labels[automation.NamespaceLabel] == "enabled", nil
		}
	}
	return false, nil
}

// PatchResources applies one automated container change with a strategic
// merge patch owned by the KubeRight field manager. The patch carries the
// evaluated resourceVersion, so a concurrent edit makes it fail instead of
// overwriting someone else's change.
func (provider *KubernetesProvider) PatchResources(ctx context.Context, change automation.Change) error {
	if !automation.Supported(change.Kind) {
		return fmt.Errorf("automated changes are not supported for %s", change.Kind)
	}
	body, err := automation.PatchBody(change)
	if err != nil {
		return err
	}
	options := metav1.PatchOptions{FieldManager: automation.FieldManager}
	apps := provider.client.AppsV1()
	switch change.Kind {
	case "Deployment":
		_, err = apps.Deployments(change.Namespace).Patch(ctx, change.Workload, types.StrategicMergePatchType, body, options)
	case "StatefulSet":
		_, err = apps.StatefulSets(change.Namespace).Patch(ctx, change.Workload, types.StrategicMergePatchType, body, options)
	case "DaemonSet":
		_, err = apps.DaemonSets(change.Namespace).Patch(ctx, change.Workload, types.StrategicMergePatchType, body, options)
	}
	switch {
	case apierrors.IsConflict(err):
		return ErrAutomationConflict
	case err != nil:
		return fmt.Errorf("patch %s %s/%s: %s", change.Kind, change.Namespace, change.Workload, safeKubernetesError(err))
	}
	return nil
}
