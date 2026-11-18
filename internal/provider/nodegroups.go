package provider

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/kuberight/kuberight/internal/httpapi"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// OpenShift links a node to its Machine with this annotation
// ("<namespace>/<machine>"), and a Machine to its MachineSet with the label.
const (
	openShiftMachineAnnotation = "machine.openshift.io/machine"
	openShiftMachineSetLabel   = "machine.openshift.io/cluster-api-machineset"
)

var machineResource = schema.GroupVersionResource{Group: "machine.openshift.io", Version: "v1beta1", Resource: "machines"}

// A MachineSet names its Machines "<machineset>-<5 random characters>".
var machineSuffix = regexp.MustCompile(`^(.+)-[a-z0-9]{5}$`)

// providerLabels are node-group labels written by each platform's node
// lifecycle controller, in precedence order. Karpenter comes first because a
// Karpenter node on EKS carries no managed node-group label.
var providerLabels = []struct{ label, provider string }{
	{"karpenter.sh/nodepool", httpapi.NodeGroupKarpenter},
	{"eks.amazonaws.com/nodegroup", httpapi.NodeGroupEKS},
	{"alpha.eksctl.io/nodegroup-name", httpapi.NodeGroupEKS},
	{"cloud.google.com/gke-nodepool", httpapi.NodeGroupGKE},
	{"kubernetes.azure.com/agentpool", httpapi.NodeGroupAKS},
}

// assignNodeGroups sets each node's group and how its membership is known.
// Verified membership comes from the platform's own node-group label, from
// the Cluster API owner annotations, or from the node's OpenShift Machine
// read through the API. Inferred membership comes from a Machine name prefix
// when Machines cannot be read, or from the node's shape when the node
// carries no group at all.
func (provider *KubernetesProvider) assignNodeGroups(ctx context.Context, nodes []corev1.Node, capacity []httpapi.NodeCapacity) []string {
	var warnings []string
	machineSets, machineErr := map[string]string{}, error(nil)
	machinesRead := map[string]bool{}
	for index := range nodes {
		node, group := &nodes[index], &capacity[index]
		if name, providerName, evidence := labelGroup(node.Labels, node.Annotations); name != "" {
			group.NodeGroup, group.NodeGroupProvider, group.NodeGroupMembership, group.NodeGroupEvidence = name, providerName, httpapi.MembershipVerified, evidence
			continue
		}
		if reference := node.Annotations[openShiftMachineAnnotation]; reference != "" {
			namespace, machine, ok := strings.Cut(reference, "/")
			if ok && namespace != "" && machine != "" {
				if !machinesRead[namespace] && machineErr == nil {
					machinesRead[namespace] = true
					machineErr = provider.readMachineSets(ctx, namespace, machineSets)
				}
				group.NodeGroupProvider = httpapi.NodeGroupOpenShift
				if set := machineSets[namespace+"/"+machine]; set != "" {
					group.NodeGroup, group.NodeGroupMembership = set, httpapi.MembershipVerified
					group.NodeGroupEvidence = fmt.Sprintf("Machine %s/%s, label %s", namespace, machine, openShiftMachineSetLabel)
					continue
				}
				if match := machineSuffix.FindStringSubmatch(machine); match != nil {
					group.NodeGroup, group.NodeGroupMembership = match[1], httpapi.MembershipInferred
					group.NodeGroupEvidence = fmt.Sprintf("Machine name %s; no MachineSet label was read", machine)
					continue
				}
			}
		}
		group.NodeGroup, group.NodeGroupProvider, group.NodeGroupMembership = shapeGroup(*group), httpapi.NodeGroupNone, httpapi.MembershipInferred
		group.NodeGroupEvidence = "no node-group label; grouped by node shape"
	}
	if machineErr != nil {
		warnings = append(warnings, "OpenShift Machines could not be read, so MachineSet membership is inferred from Machine names: "+safeKubernetesError(machineErr))
	}
	return warnings
}

// labelGroup returns a node group named by a provider label or by the
// Cluster API owner annotations.
func labelGroup(labels, annotations map[string]string) (name, providerName, evidence string) {
	for _, candidate := range providerLabels {
		if value := labels[candidate.label]; value != "" {
			return value, candidate.provider, "label " + candidate.label
		}
	}
	if annotations["cluster.x-k8s.io/owner-kind"] == "MachineSet" && annotations["cluster.x-k8s.io/owner-name"] != "" {
		return annotations["cluster.x-k8s.io/owner-name"], httpapi.NodeGroupClusterAPI, "annotation cluster.x-k8s.io/owner-name"
	}
	return "", "", ""
}

// readMachineSets maps "<namespace>/<machine>" to its MachineSet.
func (provider *KubernetesProvider) readMachineSets(ctx context.Context, namespace string, into map[string]string) error {
	machines, err := provider.dynamic.Resource(machineResource).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("the machine.openshift.io API is not served: %w", err)
		}
		return err
	}
	for _, machine := range machines.Items {
		if set := machine.GetLabels()[openShiftMachineSetLabel]; set != "" {
			into[namespace+"/"+machine.GetName()] = set
		}
	}
	return nil
}

// shapeGroup names an unlabeled node's group by instance type and capacity
// type, or by allocatable resources when the instance type is unknown.
func shapeGroup(node httpapi.NodeCapacity) string {
	shape := node.InstanceType
	if shape == "" {
		shape = fmt.Sprintf("%dm-%dMi", node.CPUMilli, node.MemoryBytes>>20)
	}
	if node.CapacityType != "" {
		shape += "-" + node.CapacityType
	}
	return "shape:" + shape
}
