package provider

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func controlledBy(apiVersion, kind, name string, uid types.UID) metav1.OwnerReference {
	controller := true
	return metav1.OwnerReference{APIVersion: apiVersion, Kind: kind, Name: name, UID: uid, Controller: &controller}
}

func TestPodOwnersResolveControllerChainsWithoutNameGuessing(t *testing.T) {
	client := fake.NewSimpleClientset(
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "api-v2-7d9", Namespace: "shop", UID: "rs-api-v2", OwnerReferences: []metav1.OwnerReference{controlledBy("apps/v1", "Deployment", "api-v2", "deployment-api-v2")}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "api-v2-7d9-x2", Namespace: "shop", OwnerReferences: []metav1.OwnerReference{controlledBy("apps/v1", "ReplicaSet", "api-v2-7d9", "rs-api-v2")}}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "nightly-291", Namespace: "shop", UID: "job-nightly", OwnerReferences: []metav1.OwnerReference{controlledBy("batch/v1", "CronJob", "nightly", "cronjob-nightly")}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "nightly-291-pod", Namespace: "shop", OwnerReferences: []metav1.OwnerReference{controlledBy("batch/v1", "Job", "nightly-291", "job-nightly")}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "database-0", Namespace: "shop", OwnerReferences: []metav1.OwnerReference{controlledBy("apps/v1", "StatefulSet", "database", "stateful-database")}}},
		// The name now points at a different UID. Falling back to the name would
		// attribute metrics from a deleted controller to its replacement.
		&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "reused", Namespace: "shop", UID: "new-rs"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "orphaned-pod", Namespace: "shop", OwnerReferences: []metav1.OwnerReference{controlledBy("apps/v1", "ReplicaSet", "reused", "old-rs")}}},
	)
	owners, err := testProvider(client).PodOwners(context.Background(), []string{"shop", "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if len(owners) != 3 {
		t.Fatalf("owners=%#v, want three exact matches", owners)
	}
	want := map[string]string{
		"api-v2-7d9-x2":   "Deployment/api-v2",
		"database-0":      "StatefulSet/database",
		"nightly-291-pod": "CronJob/nightly",
	}
	for _, owner := range owners {
		if got := owner.Kind + "/" + owner.Workload; got != want[owner.Pod] || owner.Namespace != "shop" {
			t.Fatalf("owner=%#v want=%q", owner, want[owner.Pod])
		}
	}
}

func TestPodOwnersEnforceConfiguredNamespace(t *testing.T) {
	provider := testProvider(fake.NewSimpleClientset())
	provider.namespace = "shop"
	if _, err := provider.PodOwners(context.Background(), []string{"billing"}); err == nil {
		t.Fatal("out-of-scope pod owner lookup succeeded")
	}
}
