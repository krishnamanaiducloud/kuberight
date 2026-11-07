package provider

import (
	"testing"

	"github.com/kuberight/kuberight/internal/optimizer"
	corev1 "k8s.io/api/core/v1"
)

func TestDetectRuntime(t *testing.T) {
	cases := map[string]struct {
		container corev1.Container
		want      *optimizer.RuntimeContext
	}{
		"heap from JAVA_TOOL_OPTIONS": {
			container: corev1.Container{Image: "registry.example/app:1.0", Env: []corev1.EnvVar{{Name: "JAVA_TOOL_OPTIONS", Value: "-XX:+UseG1GC -Xmx768m"}}},
			want:      &optimizer.RuntimeContext{Kind: "jvm", Source: "env", MaxHeapBytes: 768 << 20},
		},
		"last -Xmx wins on the command line": {
			container: corev1.Container{Command: []string{"java"}, Args: []string{"-Xmx1g", "-Xmx2g", "-jar", "app.jar"}},
			want:      &optimizer.RuntimeContext{Kind: "jvm", Source: "command", MaxHeapBytes: 2 << 30},
		},
		"RAM percentage": {
			container: corev1.Container{Env: []corev1.EnvVar{{Name: "JAVA_OPTS", Value: "-XX:MaxRAMPercentage=75.0"}}},
			want:      &optimizer.RuntimeContext{Kind: "jvm", Source: "env", MaxRAMPercentage: 75},
		},
		"JVM image without heap settings": {
			container: corev1.Container{Image: "docker.io/library/eclipse-temurin:21-jre@sha256:abc"},
			want:      &optimizer.RuntimeContext{Kind: "jvm", Source: "image"},
		},
		"secret-backed options are not resolved": {
			container: corev1.Container{Image: "app:1", Env: []corev1.EnvVar{{Name: "JAVA_TOOL_OPTIONS", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{Key: "opts"}}}}},
		},
		"javascript is not java": {
			container: corev1.Container{Image: "node:24", Command: []string{"/usr/bin/javascript-runner"}},
		},
		"image name containing jdk as a word fragment": {
			container: corev1.Container{Image: "example/jdk-tools-ui:2"},
		},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			got := detectRuntime(test.container)
			if (got == nil) != (test.want == nil) || (got != nil && *got != *test.want) {
				t.Fatalf("detectRuntime = %+v, want %+v", got, test.want)
			}
		})
	}
}
