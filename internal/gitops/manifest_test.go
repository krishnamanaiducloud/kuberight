package gitops

import (
	"errors"
	"strings"
	"testing"
)

const manifest = `# Shop services
apiVersion: v1
kind: Service
metadata:
  name: api
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
  namespace: shop
spec:
  replicas: 3   # scaled by HPA
  template:
    spec:
      containers:
        - name: proxy
          image: envoy:1.36
          resources:
            requests: {cpu: 50m, memory: 64Mi}
        - name: app
          image: shop/api:2.4.1
          resources:
            requests:
              cpu: "1"        # sized in 2025
              memory: 512Mi
            limits:
              cpu: 2
              memory: '1Gi'
          ports:
            - containerPort: 8080
`

var api = Target{Namespace: "shop", Kind: "Deployment", Workload: "api", Container: "app"}

func TestUpdateManifestChangesOnlyTheRecommendedValues(t *testing.T) {
	updated, err := UpdateManifest([]byte(manifest), api, Resources{CPURequestMilli: 800, CPULimitMilli: 1600, MemoryRequestBytes: 410 << 20, MemoryLimitBytes: 820 << 20})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.NewReplacer(
		`cpu: "1"        # sized in 2025`, `cpu: "800m"        # sized in 2025`,
		"memory: 512Mi", "memory: 410Mi",
		"cpu: 2\n", "cpu: 1600m\n",
		"memory: '1Gi'", "memory: '820Mi'",
	).Replace(manifest)
	if string(updated) != want {
		t.Fatalf("diff beyond the four values:\n%s", updated)
	}
}

func TestUpdateManifestInsertsMissingValuesWithMatchingIndentation(t *testing.T) {
	source := `apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: web
spec:
  template:
    spec:
      containers:
      - name: app
        image: nginx
        resources:
          requests:
            cpu: 250m
      - name: sidecar
        image: busybox
`
	updated, err := UpdateManifest([]byte(source), Target{Namespace: "shop", Kind: "StatefulSet", Workload: "web", Container: "app"},
		Resources{CPURequestMilli: 225, MemoryRequestBytes: 64 << 20, MemoryLimitBytes: 128 << 20})
	if err != nil {
		t.Fatal(err)
	}
	want := `apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: web
spec:
  template:
    spec:
      containers:
      - name: app
        image: nginx
        resources:
          requests:
            cpu: 225m
            memory: 64Mi
          limits:
            memory: 128Mi
      - name: sidecar
        image: busybox
`
	if string(updated) != want {
		t.Fatalf("got:\n%s", updated)
	}
	// A container without any resources gets a complete block.
	updated, err = UpdateManifest([]byte(source), Target{Namespace: "shop", Kind: "StatefulSet", Workload: "web", Container: "sidecar"},
		Resources{CPURequestMilli: 2000, MemoryRequestBytes: 3 << 30})
	if err != nil || !strings.HasSuffix(string(updated), "      - name: sidecar\n        image: busybox\n        resources:\n          requests:\n            cpu: 2\n            memory: 3Gi\n") {
		t.Fatalf("err=%v got:\n%s", err, updated)
	}
}

func TestUpdateManifestRemovesTheCPULimit(t *testing.T) {
	updated, err := UpdateManifest([]byte(manifest), api, Resources{CPURequestMilli: 1000, MemoryRequestBytes: 512 << 20, RemoveCPULimit: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(updated), "cpu: 2\n") || !strings.Contains(string(updated), "limits:\n              memory: '1Gi'") {
		t.Fatalf("got:\n%s", updated)
	}
}

func TestUpdateManifestRefusesWhatItCannotEditSafely(t *testing.T) {
	for name, test := range map[string]struct {
		target Target
		want   error
	}{
		"other namespace":   {Target{Namespace: "payments", Kind: "Deployment", Workload: "api", Container: "app"}, ErrTargetNotFound},
		"other kind":        {Target{Namespace: "shop", Kind: "DaemonSet", Workload: "api", Container: "app"}, ErrTargetNotFound},
		"missing container": {Target{Namespace: "shop", Kind: "Deployment", Workload: "api", Container: "worker"}, ErrTargetNotFound},
	} {
		if _, err := UpdateManifest([]byte(manifest), test.target, Resources{CPURequestMilli: 100, MemoryRequestBytes: 128 << 20}); !errors.Is(err, test.want) {
			t.Errorf("%s: err=%v, want %v", name, err, test.want)
		}
	}
	if _, err := UpdateManifest([]byte(manifest), api, Resources{CPURequestMilli: 1000, MemoryRequestBytes: 512 << 20}); !errors.Is(err, ErrNoChange) {
		t.Errorf("unchanged values: err=%v", err)
	}
	if _, err := UpdateManifest([]byte("kind: [\n"), api, Resources{}); err == nil || errors.Is(err, ErrTargetNotFound) {
		t.Errorf("invalid YAML: err=%v", err)
	}
}

func TestQuantitiesUseCanonicalSuffixes(t *testing.T) {
	for value, want := range map[int64]string{250: "250m", 1000: "1", 1500: "1500m", 4000: "4"} {
		if got := FormatCPU(value); got != want {
			t.Errorf("FormatCPU(%d) = %s, want %s", value, got, want)
		}
	}
	for value, want := range map[int64]string{64 << 20: "64Mi", 3 << 30: "3Gi", 1536 << 20: "1536Mi", 1000: "1000", 2048: "2Ki"} {
		if got := FormatMemory(value); got != want {
			t.Errorf("FormatMemory(%d) = %s, want %s", value, got, want)
		}
	}
}

func TestUpdateManifestEditsFlowMappingsButNeverInsertsIntoThem(t *testing.T) {
	proxy := Target{Namespace: "shop", Kind: "Deployment", Workload: "api", Container: "proxy"}
	updated, err := UpdateManifest([]byte(manifest), proxy, Resources{CPURequestMilli: 25, MemoryRequestBytes: 32 << 20})
	if err != nil || !strings.Contains(string(updated), "requests: {cpu: 25m, memory: 32Mi}") {
		t.Fatalf("err=%v got:\n%s", err, updated)
	}
	partial := strings.Replace(manifest, "requests: {cpu: 50m, memory: 64Mi}", "requests: {cpu: 50m}", 1)
	if _, err := UpdateManifest([]byte(partial), proxy, Resources{CPURequestMilli: 25, MemoryRequestBytes: 32 << 20}); !errors.Is(err, ErrUnsupportedLayout) {
		t.Fatalf("insert into a flow mapping: err=%v", err)
	}
}
