package provider

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/kuberight/kuberight/internal/httpapi"
	"go.yaml.in/yaml/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var machineAutoscalerResource = schema.GroupVersionResource{Group: "autoscaling.openshift.io", Version: "v1beta1", Resource: "machineautoscalers"}

// clusterAutoscalerStatusName is the ConfigMap Cluster Autoscaler writes its
// per-node-group health and size limits to.
const clusterAutoscalerStatusName = "cluster-autoscaler-status"

// nodeGroupID names a node group the way node impact groups nodes.
type nodeGroupID struct{ Provider, Name string }

// autoscalerGroup is one Cluster Autoscaler node group from its status.
type autoscalerGroup struct {
	name             string
	minSize, maxSize int
}

// legacyGroup matches a node group in the text status written before
// Cluster Autoscaler 1.30.
var legacyGroup = regexp.MustCompile(`(?m)^\s*Name:\s*(\S+)\s*\n\s*Health:[^\n]*cloudProviderTarget=(\d+)\s*\(minSize=(\d+),\s*maxSize=(\d+)\)`)

// parseAutoscalerStatus reads the YAML status of Cluster Autoscaler 1.30 and
// later, or the earlier text status.
func parseAutoscalerStatus(status string) ([]autoscalerGroup, error) {
	var document struct {
		NodeGroups []struct {
			Name   string `yaml:"name"`
			Health struct {
				MinSize *int `yaml:"minSize"`
				MaxSize *int `yaml:"maxSize"`
			} `yaml:"health"`
		} `yaml:"nodeGroups"`
	}
	if err := yaml.Unmarshal([]byte(status), &document); err == nil && len(document.NodeGroups) > 0 {
		groups := make([]autoscalerGroup, 0, len(document.NodeGroups))
		for _, group := range document.NodeGroups {
			if group.Name == "" || group.Health.MinSize == nil || group.Health.MaxSize == nil {
				continue
			}
			groups = append(groups, autoscalerGroup{name: group.Name, minSize: *group.Health.MinSize, maxSize: *group.Health.MaxSize})
		}
		return groups, nil
	}
	matches := legacyGroup.FindAllStringSubmatch(status, -1)
	if len(matches) == 0 {
		return nil, fmt.Errorf("no node groups with size limits were found")
	}
	groups := make([]autoscalerGroup, 0, len(matches))
	for _, match := range matches {
		minimum, _ := strconv.Atoi(match[3])
		maximum, _ := strconv.Atoi(match[4])
		groups = append(groups, autoscalerGroup{name: match[1], minSize: minimum, maxSize: maximum})
	}
	return groups, nil
}

// matchAutoscalerGroup maps a Cluster Autoscaler node group name to the node
// group KubeRight identified. Cluster API names ("MachineSet/<namespace>/<name>"
// or "MachineDeployment/...") are exact; the cloud names follow each
// provider's naming convention, so those matches are inferred.
func matchAutoscalerGroup(name string, group nodeGroupID) (bool, string) {
	if kind, rest, found := strings.Cut(name, "/"); found && (kind == "MachineSet" || kind == "MachineDeployment") {
		parts := strings.Split(rest, "/")
		if (group.Provider == httpapi.NodeGroupOpenShift || group.Provider == httpapi.NodeGroupClusterAPI) && parts[len(parts)-1] == group.Name {
			return true, httpapi.MembershipVerified
		}
		return false, ""
	}
	last := name[strings.LastIndex(name, "/")+1:]
	switch group.Provider {
	case httpapi.NodeGroupEKS:
		// Managed node group Auto Scaling groups are named eks-<node group>-<id>.
		return strings.HasPrefix(last, "eks-"+group.Name+"-"), httpapi.MembershipInferred
	case httpapi.NodeGroupGKE:
		// Managed instance groups are named gke-<cluster>-<pool>-<id>-grp.
		return strings.HasPrefix(last, "gke-") && strings.Contains(last, "-"+group.Name+"-"), httpapi.MembershipInferred
	case httpapi.NodeGroupAKS:
		// Scale sets are named aks-<pool>-<id>-vmss.
		return strings.HasPrefix(strings.ToLower(last), "aks-"+strings.ToLower(group.Name)+"-"), httpapi.MembershipInferred
	}
	return false, ""
}

// nodeGroupLimits reads node-group minimum and maximum sizes from OpenShift
// MachineAutoscalers and from Cluster Autoscaler's status ConfigMap in each
// namespace given. Unreadable sources produce warnings, not errors.
func (provider *KubernetesProvider) nodeGroupLimits(ctx context.Context, autoscalerNamespaces []string, nodes []httpapi.NodeCapacity) (map[string]httpapi.NodeGroupLimit, []string) {
	limits, warnings := map[string]httpapi.NodeGroupLimit{}, []string{}
	key := func(group nodeGroupID) string { return group.Provider + "\x00" + group.Name }
	groups, seen := []nodeGroupID{}, map[nodeGroupID]bool{}
	for _, node := range nodes {
		if id := (nodeGroupID{Provider: node.NodeGroupProvider, Name: node.NodeGroup}); !seen[id] {
			seen[id] = true
			groups = append(groups, id)
		}
	}
	autoscalers, err := listAll(ctx, func(ctx context.Context, options metav1.ListOptions) ([]unstructured.Unstructured, string, error) {
		result, listErr := provider.dynamic.Resource(machineAutoscalerResource).List(ctx, options)
		if listErr != nil {
			return nil, "", listErr
		}
		return result.Items, result.GetContinue(), nil
	})
	switch {
	case err == nil:
		for _, autoscaler := range autoscalers {
			kind, _, _ := unstructured.NestedString(autoscaler.Object, "spec", "scaleTargetRef", "kind")
			target, _, _ := unstructured.NestedString(autoscaler.Object, "spec", "scaleTargetRef", "name")
			minimum, minFound, _ := unstructured.NestedInt64(autoscaler.Object, "spec", "minReplicas")
			maximum, maxFound, _ := unstructured.NestedInt64(autoscaler.Object, "spec", "maxReplicas")
			if kind != "MachineSet" || target == "" || !minFound || !maxFound {
				continue
			}
			for _, group := range groups {
				if group.Provider == httpapi.NodeGroupOpenShift && group.Name == target {
					limits[key(group)] = httpapi.NodeGroupLimit{MinSize: int(minimum), MaxSize: int(maximum), Membership: httpapi.MembershipVerified,
						Source: fmt.Sprintf("MachineAutoscaler %s/%s", autoscaler.GetNamespace(), autoscaler.GetName())}
				}
			}
		}
	case apierrors.IsNotFound(err):
		// Not an OpenShift cluster.
	default:
		warnings = append(warnings, "OpenShift MachineAutoscalers could not be read: "+safeKubernetesError(err))
	}

	for _, namespace := range autoscalerNamespaces {
		status, err := provider.client.CoreV1().ConfigMaps(namespace).Get(ctx, clusterAutoscalerStatusName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("Cluster Autoscaler status in %s could not be read, so node-group sizes are unknown: %s", namespace, safeKubernetesError(err)))
			continue
		}
		parsed, err := parseAutoscalerStatus(status.Data["status"])
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("Cluster Autoscaler status in %s has no readable node-group sizes: %v", namespace, err))
			continue
		}
		// Several autoscaler groups, such as one per zone, can back one node
		// group; their limits add up.
		combined := map[string]httpapi.NodeGroupLimit{}
		names := map[string][]string{}
		for _, autoscaled := range parsed {
			// "eks-general-two-<id>" also starts with "eks-general-", so the
			// longest matching group name wins.
			best, bestMembership := nodeGroupID{}, ""
			for _, group := range groups {
				if matched, membership := matchAutoscalerGroup(autoscaled.name, group); matched && len(group.Name) > len(best.Name) {
					best, bestMembership = group, membership
				}
			}
			if best.Name == "" {
				continue
			}
			limit := combined[key(best)]
			limit.MinSize += autoscaled.minSize
			limit.MaxSize += autoscaled.maxSize
			if limit.Membership == "" || bestMembership == httpapi.MembershipInferred {
				limit.Membership = bestMembership
			}
			combined[key(best)] = limit
			names[key(best)] = append(names[key(best)], autoscaled.name)
		}
		for groupKey, limit := range combined {
			if _, exact := limits[groupKey]; exact {
				continue
			}
			sort.Strings(names[groupKey])
			limit.Source = fmt.Sprintf("Cluster Autoscaler status %s/%s: %s", namespace, clusterAutoscalerStatusName, strings.Join(names[groupKey], ", "))
			limits[groupKey] = limit
		}
	}
	return limits, warnings
}
