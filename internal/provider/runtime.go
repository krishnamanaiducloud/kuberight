package provider

import (
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/kuberight/kuberight/internal/optimizer"
	corev1 "k8s.io/api/core/v1"
)

var (
	// javaOptionVariables are read only when their value is a literal; values
	// from Secrets or ConfigMaps are not resolved.
	javaOptionVariables = []string{"JAVA_TOOL_OPTIONS", "JDK_JAVA_OPTIONS", "JAVA_OPTS", "_JAVA_OPTIONS"}
	maxHeapFlag         = regexp.MustCompile(`^-Xmx([0-9]{1,12})([kKmMgGtT]?)$`)
	javaCommand         = regexp.MustCompile(`(^|[\s/])java(\s|$)`)
	maxRAMFlag          = regexp.MustCompile(`^-XX:MaxRAMPercentage=([0-9]{1,3}(?:\.[0-9]{1,4})?)$`)
	// jvmImages are image repositories that ship a JVM. Detection from the
	// image alone is reported as such and only makes sizing more cautious.
	jvmImages = regexp.MustCompile(`(?i)(^|/)(openjdk|eclipse-temurin|temurin|amazoncorretto|corretto|zulu-openjdk|zulu|ibm-semeru-runtimes|semeru|graalvm-ce|graalvm|liberica-openjdk[a-z-]*|liberica|sapmachine|jre|jdk|tomcat|jetty|wildfly|keycloak|elasticsearch|logstash|kafka|cp-kafka|cp-zookeeper|zookeeper|cassandra|solr|sonarqube|jenkins|nexus3)(:|@|$)`)
)

func heapBytes(value, unit string) int64 {
	amount, err := strconv.ParseInt(value, 10, 64)
	if err != nil || amount <= 0 {
		return 0
	}
	multiplier := map[string]int64{"": 1, "k": 1 << 10, "m": 1 << 20, "g": 1 << 30, "t": 1 << 40}[strings.ToLower(unit)]
	if amount > math.MaxInt64/multiplier {
		return 0
	}
	return amount * multiplier
}

// detectRuntime recognizes JVM containers and their heap configuration from
// literal JVM option variables, the command and arguments, and the image.
// The last -Xmx wins, as it does for the JVM.
func detectRuntime(container corev1.Container) *optimizer.RuntimeContext {
	options := make([]string, 0, 4)
	source := ""
	for _, variable := range container.Env {
		for _, name := range javaOptionVariables {
			if variable.Name == name && variable.ValueFrom == nil && variable.Value != "" {
				options, source = append(options, variable.Value), "env"
			}
		}
	}
	commandLine := strings.Join(append(append([]string{}, container.Command...), container.Args...), " ")
	if javaCommand.MatchString(commandLine) || strings.Contains(commandLine, "-Xmx") || strings.Contains(commandLine, "-XX:MaxRAMPercentage=") {
		options = append(options, commandLine)
		if source == "" {
			source = "command"
		}
	}
	runtime := optimizer.RuntimeContext{Kind: optimizer.RuntimeJVM}
	for _, option := range options {
		for _, token := range strings.Fields(option) {
			if match := maxHeapFlag.FindStringSubmatch(token); match != nil {
				runtime.MaxHeapBytes = heapBytes(match[1], match[2])
			}
			if match := maxRAMFlag.FindStringSubmatch(token); match != nil {
				if value, err := strconv.ParseFloat(match[1], 64); err == nil && value > 0 && value <= 100 {
					runtime.MaxRAMPercentage = value
				}
			}
		}
	}
	switch {
	case runtime.MaxHeapBytes > 0 || runtime.MaxRAMPercentage > 0:
		runtime.Source = source
	case source == "env":
		runtime.Source = "env"
	case javaCommand.MatchString(commandLine):
		runtime.Source = "command"
	case jvmImages.MatchString(imageRepository(container.Image)):
		runtime.Source = "image"
	default:
		return nil
	}
	return &runtime
}

// imageRepository drops the registry host, tag, and digest.
func imageRepository(image string) string {
	if at := strings.IndexByte(image, '@'); at >= 0 {
		image = image[:at]
	}
	if slash := strings.LastIndexByte(image, '/'); slash >= 0 {
		if colon := strings.LastIndexByte(image[slash:], ':'); colon >= 0 {
			image = image[:slash+colon]
		}
	} else if colon := strings.LastIndexByte(image, ':'); colon >= 0 {
		image = image[:colon]
	}
	return image
}
