package automation

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestPatchBodyMergesOneContainerWithPreconditionAndRollbackRecord(t *testing.T) {
	change := Change{
		Namespace: "shop", Kind: "Deployment", Workload: "checkout", Container: "api", ResourceVersion: "4211", RecommendationID: "rec_1",
		Desired:  Resources{CPURequestMilli: 750, CPULimitMilli: 0, MemoryRequestBytes: 768 << 20, MemoryLimitBytes: 1 << 30},
		Previous: Resources{CPURequestMilli: 1000, CPULimitMilli: 2000, MemoryRequestBytes: 1 << 30, MemoryLimitBytes: 2 << 30},
	}
	body, err := PatchBody(change)
	if err != nil {
		t.Fatal(err)
	}
	var patch map[string]any
	if err := json.Unmarshal(body, &patch); err != nil {
		t.Fatal(err)
	}
	metadata := patch["metadata"].(map[string]any)
	if metadata["resourceVersion"] != "4211" {
		t.Fatalf("missing optimistic-concurrency precondition: %s", body)
	}
	annotations := metadata["annotations"].(map[string]any)
	var previous Resources
	if err := json.Unmarshal([]byte(annotations[PreviousAnnotation].(string)), &previous); err != nil || previous != change.Previous || annotations[AppliedAnnotation] != "rec_1" {
		t.Fatalf("rollback record = %v err=%v", annotations, err)
	}
	container := patch["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	resources := container["resources"].(map[string]any)
	limits := resources["limits"].(map[string]any)
	requests := resources["requests"].(map[string]any)
	if container["name"] != "api" || requests["cpu"] != "750m" || requests["memory"] != "768Mi" || limits["memory"] != "1024Mi" {
		t.Fatalf("resources = %v", resources)
	}
	// A removed CPU limit is an explicit null, which strategic merge deletes.
	if value, present := limits["cpu"]; !present || value != nil {
		t.Fatalf("CPU limit removal must be null, got %v (present=%v)", value, present)
	}
}

func TestPatchBodyRejectsUnsafeChanges(t *testing.T) {
	valid := Change{Namespace: "shop", Workload: "w", Container: "c", ResourceVersion: "1", RecommendationID: "rec_1", Desired: Resources{CPURequestMilli: 100, MemoryRequestBytes: 64 << 20}}
	cases := map[string]func(Change) Change{
		"no precondition":       func(change Change) Change { change.ResourceVersion = ""; return change },
		"no memory request":     func(change Change) Change { change.Desired.MemoryRequestBytes = 0; return change },
		"limit below request":   func(change Change) Change { change.Desired.CPULimitMilli = 50; return change },
		"no recommendation id":  func(change Change) Change { change.RecommendationID = ""; return change },
		"memory limit below":    func(change Change) Change { change.Desired.MemoryLimitBytes = 1; return change },
		"missing container":     func(change Change) Change { change.Container = ""; return change },
		"missing workload name": func(change Change) Change { change.Workload = ""; return change },
	}
	for name, mutate := range cases {
		if _, err := PatchBody(mutate(valid)); err == nil {
			t.Fatalf("%s: unsafe patch was rendered", name)
		}
	}
}

func TestGitOpsManagerDetection(t *testing.T) {
	if GitOpsManager(nil, map[string]string{"argocd.argoproj.io/tracking-id": "app:apps/Deployment:shop/web"}) != "Argo CD" ||
		GitOpsManager(map[string]string{"kustomize.toolkit.fluxcd.io/name": "apps"}, nil) != "Flux" ||
		GitOpsManager(map[string]string{"helm.toolkit.fluxcd.io/name": "web"}, nil) != "Flux" ||
		GitOpsManager(map[string]string{"app.kubernetes.io/managed-by": "Helm"}, nil) != "" {
		t.Fatal("GitOps ownership was misdetected")
	}
}

func TestChangeWindow(t *testing.T) {
	day := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	overnight, err := ParseWindow("22:00-04:30")
	if err != nil {
		t.Fatal(err)
	}
	for hour, open := range map[time.Duration]bool{23 * time.Hour: true, 2 * time.Hour: true, 4*time.Hour + 30*time.Minute: false, 12 * time.Hour: false} {
		if overnight.Open(day.Add(hour)) != open {
			t.Fatalf("overnight window at %s: want %v", hour, open)
		}
	}
	daytime, _ := ParseWindow("09:00-17:00")
	if !daytime.Open(day.Add(9*time.Hour)) || daytime.Open(day.Add(17*time.Hour)) || daytime.String() != "09:00-17:00 UTC" {
		t.Fatal("daytime window bounds are wrong")
	}
	if open, _ := ParseWindow(""); !open.Open(day) || open.String() != "" {
		t.Fatal("an empty window must always be open")
	}
	for _, invalid := range []string{"9-17", "09:00", "25:00-01:00", "10:00-10:00"} {
		if _, err := ParseWindow(invalid); err == nil || !strings.Contains(err.Error(), "window") {
			t.Fatalf("%q was accepted", invalid)
		}
	}
	if !Supported("Deployment") || !Supported("StatefulSet") || !Supported("DaemonSet") || Supported("CronJob") || Supported("Job") {
		t.Fatal("supported kinds are wrong")
	}
}
