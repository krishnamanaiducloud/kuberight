package gitops

import (
	"errors"
	"testing"
)

var valuesTarget = Resources{CPURequestMilli: 250, MemoryRequestBytes: 384 << 20, MemoryLimitBytes: 512 << 20}

func TestUpdateValuesEditsAnExistingBlockInPlace(t *testing.T) {
	values := `# Production values
replicaCount: 3
api:
  image: ghcr.io/acme/api:1.4.2
  resources:
    requests:
      cpu: "1"     # sized for launch
      memory: 1Gi
    limits:
      memory: 2Gi
  service:
    port: 8080
`
	want := `# Production values
replicaCount: 3
api:
  image: ghcr.io/acme/api:1.4.2
  resources:
    requests:
      cpu: "250m"     # sized for launch
      memory: 384Mi
    limits:
      memory: 512Mi
  service:
    port: 8080
`
	got, err := UpdateValues([]byte(values), "api.resources", valuesTarget)
	if err != nil || string(got) != want {
		t.Fatalf("err=%v\n%s", err, got)
	}
	if _, err := UpdateValues([]byte(want), "api.resources", valuesTarget); !errors.Is(err, ErrNoChange) {
		t.Fatalf("unchanged values: %v", err)
	}
}

func TestUpdateValuesReplacesEmptyAndNullResources(t *testing.T) {
	for name, line := range map[string]string{
		"empty mapping": "resources: {}  # set per environment",
		"null":          "resources: null  # set per environment",
		"tilde":         "resources: ~  # set per environment",
		"bare":          "resources:  # set per environment",
	} {
		values := "nameOverride: \"\"\n" + line + "\nnodeSelector: {}\n"
		want := "nameOverride: \"\"\nresources:  # set per environment\n  requests:\n    cpu: 250m\n    memory: 384Mi\n  limits:\n    memory: 512Mi\nnodeSelector: {}\n"
		got, err := UpdateValues([]byte(values), "resources", valuesTarget)
		if err != nil || string(got) != want {
			t.Fatalf("%s: err=%v\n%s", name, err, got)
		}
	}
}

func TestUpdateValuesAddsAMissingKeyToItsMapping(t *testing.T) {
	values := "worker:\n  replicas: 2\n  image: worker:1\nglobal:\n  env: prod\n"
	want := "worker:\n  replicas: 2\n  image: worker:1\n  resources:\n    requests:\n      cpu: 250m\n      memory: 384Mi\n    limits:\n      memory: 512Mi\nglobal:\n  env: prod\n"
	got, err := UpdateValues([]byte(values), "worker.resources", valuesTarget)
	if err != nil || string(got) != want {
		t.Fatalf("err=%v\n%s", err, got)
	}
}

func TestUpdateValuesRejectsWhatItCannotEditSafely(t *testing.T) {
	for name, test := range map[string]struct {
		values, key string
		want        error
	}{
		"missing parent":   {"api:\n  image: a\n", "worker.resources", ErrTargetNotFound},
		"empty document":   {"", "resources", ErrTargetNotFound},
		"flow resources":   {"resources: {requests: {cpu: 1}}\n", "resources", ErrUnsupportedLayout},
		"scalar resources": {"resources: small\n", "resources", ErrUnsupportedLayout},
		"flow parent":      {"api: {image: a}\n", "api.resources", ErrUnsupportedLayout},
	} {
		if _, err := UpdateValues([]byte(test.values), test.key, valuesTarget); !errors.Is(err, test.want) {
			t.Fatalf("%s: err=%v", name, err)
		}
	}
	for _, key := range []string{"", ".resources", "api..resources", "api.resources[0]", "a b"} {
		if err := ValidateValuesKey(key); err == nil {
			t.Fatalf("key %q was accepted", key)
		}
	}
}
