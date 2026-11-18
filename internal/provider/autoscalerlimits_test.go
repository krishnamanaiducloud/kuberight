package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/kuberight/kuberight/internal/httpapi"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
)

const yamlStatus = `time: 2026-11-14 10:00:00 +0000 UTC
autoscalerStatus: Running
nodeGroups:
- name: eks-general-58c2a1b4-7f2e-11ee
  health:
    status: Healthy
    nodeCounts:
      registered: {total: 4, ready: 4}
    cloudProviderTarget: 4
    minSize: 2
    maxSize: 10
- name: eks-general-two-9f8e7d6c
  health: {status: Healthy, cloudProviderTarget: 1, minSize: 1, maxSize: 3}
- name: https://www.googleapis.com/compute/v1/projects/p/zones/us-central1-a/instanceGroups/gke-prod-pool-1-9a8b7c6d-grp
  health: {status: Healthy, cloudProviderTarget: 2, minSize: 1, maxSize: 5}
- name: https://www.googleapis.com/compute/v1/projects/p/zones/us-central1-b/instanceGroups/gke-prod-pool-1-1f2e3d4c-grp
  health: {status: Healthy, cloudProviderTarget: 2, minSize: 1, maxSize: 5}
- name: MachineDeployment/default/md-0-7f9c
  health: {status: Healthy, cloudProviderTarget: 3, minSize: 3, maxSize: 6}
- name: unsized
  health: {status: Healthy, cloudProviderTarget: 1}
`

const legacyStatus = `Cluster-autoscaler status at 2026-11-14 10:00:00 +0000 UTC:
Cluster-wide:
  Health:      Healthy (ready=5 unready=0 notStarted=0 longNotStarted=0 registered=5 longUnregistered=0)

NodeGroups:
  Name:        aks-nodepool1-31415926-vmss
  Health:      Healthy (ready=3 unready=0 notStarted=0 longNotStarted=0 registered=3 longUnregistered=0 cloudProviderTarget=3 (minSize=1, maxSize=8))
  ScaleUp:     NoActivity (ready=3 cloudProviderTarget=3)
`

func groupKey(provider, name string) string { return provider + "\x00" + name }

func TestParseAutoscalerStatusReadsBothFormats(t *testing.T) {
	groups, err := parseAutoscalerStatus(yamlStatus)
	if err != nil || len(groups) != 5 || groups[0].name != "eks-general-58c2a1b4-7f2e-11ee" || groups[0].minSize != 2 || groups[0].maxSize != 10 {
		t.Fatalf("yaml groups=%+v err=%v", groups, err)
	}
	legacy, err := parseAutoscalerStatus(legacyStatus)
	if err != nil || len(legacy) != 1 || legacy[0].name != "aks-nodepool1-31415926-vmss" || legacy[0].minSize != 1 || legacy[0].maxSize != 8 {
		t.Fatalf("legacy groups=%+v err=%v", legacy, err)
	}
	if _, err := parseAutoscalerStatus("Cluster-autoscaler status: starting"); err == nil {
		t.Fatal("a status without node groups must be reported")
	}
}

func TestMatchAutoscalerGroupFollowsProviderNaming(t *testing.T) {
	for name, test := range map[string]struct {
		autoscaled string
		group      nodeGroupID
		matched    bool
		membership string
	}{
		"eks":                     {"eks-general-58c2a1b4", nodeGroupID{httpapi.NodeGroupEKS, "general"}, true, httpapi.MembershipInferred},
		"eks other group":         {"eks-batch-58c2a1b4", nodeGroupID{httpapi.NodeGroupEKS, "general"}, false, httpapi.MembershipInferred},
		"gke":                     {"zones/a/instanceGroups/gke-prod-pool-1-9a8b-grp", nodeGroupID{httpapi.NodeGroupGKE, "pool-1"}, true, httpapi.MembershipInferred},
		"aks":                     {"aks-NodePool1-31415926-vmss", nodeGroupID{httpapi.NodeGroupAKS, "nodepool1"}, true, httpapi.MembershipInferred},
		"cluster api":             {"MachineDeployment/default/md-0-7f9c", nodeGroupID{httpapi.NodeGroupClusterAPI, "md-0-7f9c"}, true, httpapi.MembershipVerified},
		"openshift":               {"MachineSet/openshift-machine-api/worker-a", nodeGroupID{httpapi.NodeGroupOpenShift, "worker-a"}, true, httpapi.MembershipVerified},
		"cluster api, wrong kind": {"MachineSet/default/md-0-7f9c", nodeGroupID{httpapi.NodeGroupEKS, "md-0-7f9c"}, false, ""},
		"no provider convention":  {"eks-general-1", nodeGroupID{httpapi.NodeGroupNone, "general"}, false, ""},
	} {
		matched, membership := matchAutoscalerGroup(test.autoscaled, test.group)
		if matched != test.matched || (matched && membership != test.membership) {
			t.Fatalf("%s: matched=%v membership=%q", name, matched, membership)
		}
	}
}

func TestCapacityReadsNodeGroupLimits(t *testing.T) {
	client := kubernetesfake.NewSimpleClientset(
		node("eks", true, map[string]string{"eks.amazonaws.com/nodegroup": "general"}),
		node("eks-two", true, map[string]string{"eks.amazonaws.com/nodegroup": "general-two"}),
		node("gke", true, map[string]string{"cloud.google.com/gke-nodepool": "pool-1"}),
		annotated(node("capi", true, nil), map[string]string{"cluster.x-k8s.io/owner-kind": "MachineSet", "cluster.x-k8s.io/owner-name": "md-0-7f9c"}),
		annotated(node("ocp", true, nil), map[string]string{openShiftMachineAnnotation: "openshift-machine-api/ocp-worker-a-x7k2p"}),
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "cluster-autoscaler", Namespace: "kube-system"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: clusterAutoscalerStatusName, Namespace: "kube-system"}, Data: map[string]string{"status": yamlStatus}},
	)
	autoscaler := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "autoscaling.openshift.io/v1beta1", "kind": "MachineAutoscaler",
		"metadata": map[string]any{"name": "worker-a", "namespace": "openshift-machine-api"},
		"spec":     map[string]any{"minReplicas": int64(1), "maxReplicas": int64(4), "scaleTargetRef": map[string]any{"kind": "MachineSet", "name": "ocp-worker-a"}},
	}}
	capacity, err := capacityProvider(client, machine("openshift-machine-api", "ocp-worker-a-x7k2p", "ocp-worker-a"), autoscaler).Capacity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	limits := capacity.GroupLimits
	eks := limits[groupKey(httpapi.NodeGroupEKS, "general")]
	if eks.MinSize != 2 || eks.MaxSize != 10 || eks.Membership != httpapi.MembershipInferred || !strings.Contains(eks.Source, "kube-system/cluster-autoscaler-status") {
		t.Fatalf("eks limit = %+v", eks)
	}
	// "eks-general-two-..." also starts with "eks-general-"; the longer name wins.
	if two := limits[groupKey(httpapi.NodeGroupEKS, "general-two")]; two.MinSize != 1 || two.MaxSize != 3 {
		t.Fatalf("eks general-two limit = %+v", two)
	}
	// Two zonal instance groups back one GKE node pool.
	if gke := limits[groupKey(httpapi.NodeGroupGKE, "pool-1")]; gke.MinSize != 2 || gke.MaxSize != 10 {
		t.Fatalf("gke limit = %+v", gke)
	}
	if capi := limits[groupKey(httpapi.NodeGroupClusterAPI, "md-0-7f9c")]; capi.MinSize != 3 || capi.MaxSize != 6 || capi.Membership != httpapi.MembershipVerified {
		t.Fatalf("cluster api limit = %+v", capi)
	}
	if ocp := limits[groupKey(httpapi.NodeGroupOpenShift, "ocp-worker-a")]; ocp.MinSize != 1 || ocp.MaxSize != 4 || ocp.Source != "MachineAutoscaler openshift-machine-api/worker-a" {
		t.Fatalf("openshift limit = %+v", ocp)
	}
	if len(limits) != 5 {
		t.Fatalf("limits = %+v", limits)
	}
}
