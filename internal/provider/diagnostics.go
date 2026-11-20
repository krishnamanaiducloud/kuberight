package provider

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kuberight/kuberight/internal/httpapi"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	severityCritical = "critical"
	severityWarning  = "warning"
	// repeatedRestarts is the restart count from which a container is
	// reported even when it is running now.
	repeatedRestarts = 5
	// readinessGrace keeps a starting pod from being reported as failing
	// its readiness probe.
	readinessGrace = 5 * time.Minute
)

// waitingCategories are container waiting reasons that mean the container
// cannot run, with the investigation each one calls for.
var waitingCategories = map[string]string{
	"CrashLoopBackOff":           "the container keeps exiting; read its previous logs (kubectl logs --previous) and the last termination below",
	"ImagePullBackOff":           "the image cannot be pulled; check the image name, tag, registry access, and pull secrets",
	"ErrImagePull":               "the image cannot be pulled; check the image name, tag, registry access, and pull secrets",
	"InvalidImageName":           "the image reference is malformed",
	"CreateContainerConfigError": "a referenced ConfigMap, Secret, or key is missing, or the container configuration is invalid",
	"CreateContainerError":       "the runtime could not create the container; see the pod events",
	"RunContainerError":          "the runtime could not start the container; see the pod events",
}

// diagnosticOwner names the workload that owns a pod: its controller, with
// a Deployment's ReplicaSet resolved to the Deployment from the pod's
// pod-template-hash label. A static pod's mirror is owned by its Node and is
// reported as the pod itself, so it is never mistaken for a node problem.
func diagnosticOwner(pod corev1.Pod) (kind, name string) {
	for _, owner := range pod.OwnerReferences {
		if owner.Controller == nil || !*owner.Controller {
			continue
		}
		if owner.Kind == "Node" {
			break
		}
		if owner.Kind == "ReplicaSet" {
			if hash := pod.Labels["pod-template-hash"]; hash != "" && strings.HasSuffix(owner.Name, "-"+hash) {
				return "Deployment", strings.TrimSuffix(owner.Name, "-"+hash)
			}
		}
		return owner.Kind, owner.Name
	}
	return "Pod", pod.Name
}

func finding(pod corev1.Pod, container, category, severity, message string, since time.Time) map[string]any {
	kind, workload := diagnosticOwner(pod)
	rank := 1
	if severity == severityCritical {
		rank = 0
	}
	record := map[string]any{
		"id":       fmt.Sprintf("%s/%s/%s/%s", pod.Namespace, pod.Name, container, category),
		"name":     pod.Name,
		"rank":     rank,
		"severity": severity, "category": category, "evidence": "observed",
		"namespace": pod.Namespace, "kind": kind, "workload": workload, "pod": pod.Name, "container": container,
		"message": message,
	}
	if !since.IsZero() {
		record["since"] = since.UTC()
	}
	return record
}

func memoryLimit(pod corev1.Pod, container string) string {
	for _, item := range pod.Spec.Containers {
		if item.Name == container {
			if limit, ok := item.Resources.Limits[corev1.ResourceMemory]; ok {
				return limit.String()
			}
			return "none"
		}
	}
	return "unknown"
}

// podDiagnostics reports what the pod's own status shows now. Kubernetes
// keeps only the current state and the last termination of each container,
// so older causes are not claimed.
func podDiagnostics(pod corev1.Pod, now time.Time) []map[string]any {
	var findings []map[string]any
	if pod.Status.Reason == "Evicted" {
		var since time.Time
		if pod.Status.StartTime != nil {
			since = pod.Status.StartTime.Time
		}
		findings = append(findings, finding(pod, "", "Evicted", severityWarning, "the pod was evicted: "+pod.Status.Message, since))
		return findings
	}
	if pod.Status.Phase == corev1.PodPending {
		for _, condition := range pod.Status.Conditions {
			if condition.Type != corev1.PodScheduled || condition.Status != corev1.ConditionFalse {
				continue
			}
			category := "FailedScheduling"
			switch {
			case strings.Contains(condition.Message, "Insufficient cpu") && strings.Contains(condition.Message, "Insufficient memory"):
				category = "InsufficientCPUAndMemory"
			case strings.Contains(condition.Message, "Insufficient cpu"):
				category = "InsufficientCPU"
			case strings.Contains(condition.Message, "Insufficient memory"):
				category = "InsufficientMemory"
			}
			findings = append(findings, finding(pod, "", category, severityCritical,
				"the pod cannot be scheduled: "+condition.Message, condition.LastTransitionTime.Time))
		}
	}
	for _, statuses := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for _, status := range statuses {
			if waiting := status.State.Waiting; waiting != nil {
				if advice, known := waitingCategories[waiting.Reason]; known {
					message := advice
					if waiting.Message != "" {
						message = waiting.Message + "; " + advice
					}
					findings = append(findings, finding(pod, status.Name, waiting.Reason, severityCritical, message, time.Time{}))
				}
			}
			if terminated := status.LastTerminationState.Terminated; terminated != nil && terminated.Reason == "OOMKilled" {
				record := finding(pod, status.Name, "OOMKilled", severityCritical, fmt.Sprintf(
					"the container was killed for exceeding its memory limit (%s), exit code %d; compare its memory peak with the limit before changing it",
					memoryLimit(pod, status.Name), terminated.ExitCode), terminated.FinishedAt.Time)
				record["restarts"] = status.RestartCount
				findings = append(findings, record)
			} else if status.RestartCount >= repeatedRestarts {
				reason := "unknown"
				if terminated := status.LastTerminationState.Terminated; terminated != nil {
					reason = fmt.Sprintf("%s, exit code %d", terminated.Reason, terminated.ExitCode)
				}
				record := finding(pod, status.Name, "RepeatedRestarts", severityWarning, fmt.Sprintf(
					"the container restarted %d times; last termination: %s", status.RestartCount, reason), time.Time{})
				record["restarts"] = status.RestartCount
				findings = append(findings, record)
			}
		}
	}
	if pod.Status.Phase == corev1.PodRunning {
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionFalse && now.Sub(condition.LastTransitionTime.Time) > readinessGrace {
				findings = append(findings, finding(pod, "", "NotReady", severityWarning,
					"the pod has been running but not ready since "+condition.LastTransitionTime.UTC().Format(time.RFC3339)+"; its readiness probe or a container keeps failing", condition.LastTransitionTime.Time))
			}
		}
	}
	return findings
}

// diagnostics lists observed workload health problems and, with a
// cluster-wide inventory, nodes that are not ready.
func (provider *KubernetesProvider) diagnostics(ctx context.Context, requestedNamespace string) ([]map[string]any, []string, error) {
	records, _, warnings, err := provider.diagnosticsWithPods(ctx, requestedNamespace)
	return records, warnings, err
}

// HealthSummary counts pods, the workloads that own them, and findings by
// category, using the same rules as the diagnostics list. Workloads without
// pods (scaled to zero, or a CronJob between runs) are not counted.
func (provider *KubernetesProvider) HealthSummary(ctx context.Context) (httpapi.HealthSummary, error) {
	records, pods, warnings, err := provider.diagnosticsWithPods(ctx, "")
	if err != nil {
		return httpapi.HealthSummary{}, err
	}
	summary := httpapi.HealthSummary{Available: true, Pods: len(pods), Findings: map[string]int{}, Warnings: warnings}
	owners := map[string]bool{}
	for _, pod := range pods {
		kind, name := diagnosticOwner(pod)
		owners[pod.Namespace+"/"+kind+"/"+name] = true
	}
	attention := map[string]bool{}
	for _, record := range records {
		category, _ := record["category"].(string)
		summary.Findings[category]++
		if _, podFinding := record["pod"]; !podFinding {
			summary.NodesWithProblems++
			continue
		}
		attention[fmt.Sprint(record["namespace"], "/", record["kind"], "/", record["workload"])] = true
	}
	summary.WorkloadsWithPods = len(owners)
	summary.WorkloadsNeedingAttention = len(attention)
	summary.HealthyWorkloads = len(owners) - len(attention)
	return summary, nil
}

func (provider *KubernetesProvider) diagnosticsWithPods(ctx context.Context, requestedNamespace string) ([]map[string]any, []corev1.Pod, []string, error) {
	namespace := provider.effectiveNamespace(requestedNamespace)
	pods, err := listAll(ctx, func(ctx context.Context, options metav1.ListOptions) ([]corev1.Pod, string, error) {
		result, listErr := provider.client.CoreV1().Pods(namespace).List(ctx, options)
		if listErr != nil {
			return nil, "", listErr
		}
		return result.Items, result.Continue, nil
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%w: list pods: %s", errInventoryUnavailable, safeKubernetesError(err))
	}
	now := time.Now()
	records := []map[string]any{}
	for _, pod := range pods {
		for _, record := range podDiagnostics(pod, now) {
			record["cluster"] = provider.clusterID
			records = append(records, record)
		}
	}
	var warnings []string
	if provider.namespace == "" && requestedNamespace == "" {
		nodes, nodeErr := listAll(ctx, func(ctx context.Context, options metav1.ListOptions) ([]corev1.Node, string, error) {
			result, listErr := provider.client.CoreV1().Nodes().List(ctx, options)
			if listErr != nil {
				return nil, "", listErr
			}
			return result.Items, result.Continue, nil
		})
		if nodeErr != nil {
			warnings = append(warnings, "node conditions unavailable: "+safeKubernetesError(nodeErr))
		}
		for _, node := range nodes {
			for _, condition := range node.Status.Conditions {
				problem := ""
				switch {
				case condition.Type == corev1.NodeReady && condition.Status != corev1.ConditionTrue:
					problem = "NodeNotReady"
				case condition.Type == corev1.NodeMemoryPressure && condition.Status == corev1.ConditionTrue:
					problem = "NodeMemoryPressure"
				case condition.Type == corev1.NodeDiskPressure && condition.Status == corev1.ConditionTrue:
					problem = "NodeDiskPressure"
				case condition.Type == corev1.NodePIDPressure && condition.Status == corev1.ConditionTrue:
					problem = "NodePIDPressure"
				}
				if problem == "" {
					continue
				}
				severity := severityWarning
				if problem == "NodeNotReady" {
					severity = severityCritical
				}
				records = append(records, map[string]any{
					"id": "node/" + node.Name + "/" + problem, "name": node.Name, "rank": map[bool]int{true: 0, false: 1}[severity == severityCritical],
					"severity": severity, "category": problem, "evidence": "observed", "cluster": provider.clusterID,
					"kind": "Node", "workload": node.Name, "message": strings.TrimSpace(condition.Reason + ": " + condition.Message),
					"since": condition.LastTransitionTime.UTC(),
				})
			}
		}
	}
	return records, pods, warnings, nil
}
