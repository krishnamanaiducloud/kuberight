package config

import "testing"

func TestBasePathValidation(t *testing.T) {
	for basePath, valid := range map[string]bool{
		"": true, "/kuberight": true, "/tools/kube-right_v1.2~x": true,
		"kuberight": false, "/kuberight/": false, "//kuberight": false, "/a/../b": false,
		"/./kuberight": false, `/kube"right`: false, "/kube right": false, "/kube<x>": false,
	} {
		if err := ValidateBasePath(basePath); (err == nil) != valid {
			t.Errorf("ValidateBasePath(%q) = %v, valid=%v", basePath, err, valid)
		}
	}
}

func TestLoadTrimsTheBasePath(t *testing.T) {
	t.Setenv("KUBERIGHT_BASE_PATH", " /kuberight/ ")
	cfg, err := Load()
	if err != nil || cfg.BasePath != "/kuberight" {
		t.Fatalf("BasePath = %q, err = %v", cfg.BasePath, err)
	}
	t.Setenv("KUBERIGHT_BASE_PATH", "kuberight")
	if _, err := Load(); err == nil {
		t.Fatal("a base path without a leading slash was accepted")
	}
}
