package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kuberight/kuberight/internal/config"
	"github.com/kuberight/kuberight/internal/optimizer"
	"github.com/kuberight/kuberight/internal/repository"
	"github.com/kuberight/kuberight/internal/usage"
)

// FleetScanExecutor discovers every workload in a durable scan scope and
// creates one live, provenance-bound recommendation per long-running
// container. Individual workload failures are counted without hiding the
// successful results; discovery or queue failures fail the scan itself.
type FleetScanExecutor struct{ server *apiServer }

func NewFleetScanExecutor(cfg config.Config, dependencies Dependencies) (*FleetScanExecutor, error) {
	server, err := newAPIServer(cfg, dependencies)
	if err != nil {
		return nil, err
	}
	if server.provider == nil {
		return nil, errors.New("fleet scans require Kubernetes inventory")
	}
	if server.usage == nil {
		return nil, errors.New("fleet scans require a usage metric source")
	}
	if _, ok := server.provider.(WorkloadResolver); !ok {
		return nil, errors.New("fleet scans require workload resolution")
	}
	return &FleetScanExecutor{server: server}, nil
}

type fleetSubject struct{ Namespace, Kind, Workload string }

func (executor *FleetScanExecutor) Execute(ctx context.Context, scan repository.FleetScan, report func(repository.FleetScanProgress) error) error {
	subjects, err := executor.discover(ctx, scan.Scope)
	if err != nil {
		return fmt.Errorf("discover fleet workloads: %w", err)
	}
	resolver := executor.server.provider.(WorkloadResolver)
	type target struct {
		Spec      WorkloadSpec
		Container ContainerSpec
	}
	targets := make([]target, 0)
	failed := 0
	failureDetails := make([]repository.FleetTargetFailure, 0)
	for _, subject := range subjects {
		spec, resolveErr := resolver.Workload(ctx, subject.Namespace, subject.Kind, subject.Workload)
		if resolveErr != nil {
			failed++
			failureDetails = appendFleetFailure(failureDetails, repository.FleetTargetFailure{Cluster: scan.Scope.Cluster, Namespace: subject.Namespace, Kind: subject.Kind, Workload: subject.Workload, Stage: "resolve", Error: fleetFailureMessage(resolveErr)})
			continue
		}
		for _, container := range spec.Containers {
			targets = append(targets, target{Spec: spec, Container: container})
		}
		for _, container := range spec.InitContainers {
			if container.Sidecar {
				targets = append(targets, target{Spec: spec, Container: container.ContainerSpec})
			}
		}
	}
	progress := repository.FleetScanProgress{Expected: len(targets) + failed, Processed: failed, Failed: failed, FailureDetails: failureDetails}
	if err := report(progress); err != nil {
		return err
	}
	for _, target := range targets {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := executor.analyze(ctx, scan, target.Spec, target.Container); err != nil {
			progress.Failed++
			progress.FailureDetails = appendFleetFailure(progress.FailureDetails, repository.FleetTargetFailure{Cluster: target.Spec.Cluster, Namespace: target.Spec.Namespace, Kind: target.Spec.Kind, Workload: target.Spec.Name, Container: target.Container.Name, Stage: "analyze", Error: fleetFailureMessage(err)})
		} else {
			progress.Succeeded++
		}
		progress.Processed++
		if err := report(progress); err != nil {
			return err
		}
	}
	if progress.Expected > 0 && progress.Succeeded == 0 && progress.Failed == progress.Expected {
		message := "all fleet scan targets failed"
		if len(progress.FailureDetails) > 0 {
			message += ": " + progress.FailureDetails[0].Error
		}
		return errors.New(message)
	}
	return nil
}

func appendFleetFailure(details []repository.FleetTargetFailure, detail repository.FleetTargetFailure) []repository.FleetTargetFailure {
	if len(details) >= repository.MaxFleetFailureDetails {
		return details
	}
	return append(details, detail)
}

func fleetFailureMessage(err error) string {
	message := "target failed"
	if err != nil && strings.TrimSpace(err.Error()) != "" {
		message = strings.ToValidUTF8(err.Error(), "\uFFFD")
	}
	return truncate(message, 1024)
}

func (executor *FleetScanExecutor) discover(ctx context.Context, scope repository.FleetScope) ([]fleetSubject, error) {
	namespaces := scope.Namespaces
	if len(namespaces) == 0 {
		namespaces = []string{""}
	}
	kinds := make(map[string]struct{}, len(scope.Kinds))
	for _, kind := range scope.Kinds {
		kinds[strings.ToLower(kind)] = struct{}{}
	}
	filter := strings.ToLower(strings.TrimSpace(scope.Filter))
	seen := map[fleetSubject]struct{}{}
	for _, namespace := range namespaces {
		for page := 1; ; page++ {
			result, err := executor.server.provider.List(ctx, "workloads", CollectionQuery{Page: page, PageSize: 200, Namespace: namespace, Cluster: scope.Cluster})
			if err != nil {
				return nil, err
			}
			if !result.Status.Available {
				return nil, errors.New(result.Status.Reason)
			}
			items, err := workloadMaps(result.Items)
			if err != nil {
				return nil, err
			}
			for _, item := range items {
				cluster, _ := item["cluster"].(string)
				ns, _ := item["namespace"].(string)
				kind, _ := item["kind"].(string)
				name, _ := item["name"].(string)
				if ns == "" || kind == "" || name == "" {
					continue
				}
				if scope.Cluster != "" && cluster != "" && cluster != scope.Cluster {
					continue
				}
				if len(kinds) > 0 {
					if _, ok := kinds[strings.ToLower(kind)]; !ok {
						continue
					}
				}
				if filter != "" && !strings.Contains(strings.ToLower(ns+"/"+kind+"/"+name), filter) {
					continue
				}
				seen[fleetSubject{Namespace: ns, Kind: kind, Workload: name}] = struct{}{}
			}
			if page*200 >= result.Total || len(items) == 0 {
				break
			}
		}
	}
	subjects := make([]fleetSubject, 0, len(seen))
	for subject := range seen {
		subjects = append(subjects, subject)
	}
	sort.Slice(subjects, func(i, j int) bool {
		if subjects[i].Namespace != subjects[j].Namespace {
			return subjects[i].Namespace < subjects[j].Namespace
		}
		if subjects[i].Kind != subjects[j].Kind {
			return subjects[i].Kind < subjects[j].Kind
		}
		return subjects[i].Workload < subjects[j].Workload
	})
	return subjects, nil
}

func workloadMaps(value any) ([]map[string]any, error) {
	switch items := value.(type) {
	case []map[string]any:
		return items, nil
	case []any:
		result := make([]map[string]any, 0, len(items))
		for _, item := range items {
			record, ok := item.(map[string]any)
			if !ok {
				return nil, errors.New("workload inventory returned an invalid record")
			}
			result = append(result, record)
		}
		return result, nil
	default:
		return nil, errors.New("workload inventory returned an invalid collection")
	}
}

func (executor *FleetScanExecutor) effectivePolicy(ctx context.Context, scan repository.FleetScan, spec WorkloadSpec, container ContainerSpec) (*optimizer.Policy, error) {
	if scan.PolicyID != "" {
		record, err := executor.server.repository.GetPolicy(ctx, scan.PolicyID)
		if err != nil {
			return nil, err
		}
		if !record.Enabled {
			return nil, errors.New("selected policy is disabled")
		}
		subject := repository.PolicyScope{Cluster: spec.Cluster, Namespace: spec.Namespace, Kind: spec.Kind, Workload: spec.Name, Container: container.Name}
		if !record.Scope.Matches(subject) {
			return nil, errors.New("selected policy does not apply to the target container")
		}
		policy := record.Policy
		return &policy, nil
	}
	record, err := executor.server.repository.ResolvePolicy(ctx, repository.PolicyScope{Cluster: spec.Cluster, Namespace: spec.Namespace, Kind: spec.Kind, Workload: spec.Name, Container: container.Name})
	if err == nil {
		policy := record.Policy
		return &policy, nil
	}
	if !errors.Is(err, repository.ErrNotFound) {
		return nil, err
	}
	policy := optimizer.DefaultPolicy()
	return &policy, nil
}

// fleetDelayNotice is the execution delay after which a recommendation
// records that its scan ran late.
const fleetDelayNotice = 15 * time.Minute

// fleetExecutionDelay is how long after its scheduled time a scan target was
// analyzed.
func fleetExecutionDelay(scan repository.FleetScan, executedAt time.Time) time.Duration {
	if scan.ScheduledFor.IsZero() || executedAt.Before(scan.ScheduledFor) {
		return 0
	}
	return executedAt.Sub(scan.ScheduledFor)
}

func (executor *FleetScanExecutor) analyze(ctx context.Context, scan repository.FleetScan, spec WorkloadSpec, container ContainerSpec) error {
	if spec.Cluster == "" {
		spec.Cluster = executor.server.config.ClusterID
	}
	if spec.UID == "" {
		return errors.New("workload UID is required for a fleet recommendation")
	}
	policy, err := executor.effectivePolicy(ctx, scan, spec, container)
	if err != nil {
		return err
	}
	subject := usage.Subject{Namespace: spec.Namespace, Kind: spec.Kind, Workload: spec.Name, Container: container.Name}
	// The observation window ends when this target is actually analyzed, not
	// when the scan was created or scheduled. A delayed or retried scan
	// therefore never stores advice from a stale window; the delay is
	// recorded instead.
	executedAt := executor.server.now().UTC().Truncate(time.Second)
	window := usage.Window{Duration: time.Duration(scan.WindowSeconds) * time.Second, Step: executor.server.config.RecommendationStep, End: executedAt}
	evidence, err := executor.server.collectEvidence(ctx, subject, spec, container, policy, window, "")
	if err != nil {
		return err
	}
	recommendation, err := optimizer.Recommend(evidence.input)
	if err != nil {
		return err
	}
	if assessment := recommendation.Assessment; assessment != nil && assessment.ValidForSeconds > 0 {
		expires := window.End.Add(time.Duration(assessment.ValidForSeconds) * time.Second)
		recommendation.Provenance.ExpiresAt = &expires
	}
	recommendation.Warnings = append(evidence.warnings, recommendation.Warnings...)
	recommendation.Warnings = append(recommendation.Warnings, evidence.informational...)
	if delay := fleetExecutionDelay(scan, executedAt); delay >= fleetDelayNotice {
		recommendation.Warnings = append(recommendation.Warnings, fmt.Sprintf("fleet scan scheduled for %s ran %s late; the observation window ends at execution time %s", scan.ScheduledFor.UTC().Format(time.RFC3339), delay.Truncate(time.Second), executedAt.Format(time.RFC3339)))
	}
	identity := sha256.Sum256([]byte(strings.Join([]string{"kuberight-fleet-target/v1", scan.ID, spec.Cluster, spec.UID, subject.Namespace, subject.Kind, subject.Workload, subject.Container}, "\x00")))
	scanFingerprint := scan.RequestFingerprint
	if scanFingerprint == "" {
		scanFingerprint = repository.FleetRequestFingerprint(repository.FleetScanRequest{Scope: scan.Scope, PolicyID: scan.PolicyID, WindowSeconds: scan.WindowSeconds, MaxAttempts: scan.MaxAttempts})
	}
	fingerprint := sha256.Sum256([]byte("kuberight-fleet-recommendation/v1\x00" + hex.EncodeToString(identity[:]) + "\x00" + scanFingerprint))
	idempotencyScope := "fleet-target\x00" + hex.EncodeToString(identity[:])
	return executor.server.repository.InTransaction(ctx, func(ctx context.Context, tx repository.Transaction) error {
		record, created, createErr := tx.CreateRecommendation(ctx, recommendation, repository.CreateOptions{IdempotencyKey: scan.ID, Scope: idempotencyScope, Fingerprint: hex.EncodeToString(fingerprint[:]), CreatedBy: "fleet-scheduler", Source: repository.SourcePrometheus})
		if createErr != nil {
			return createErr
		}
		action := "replay"
		if created {
			action = "create"
		}
		return tx.AppendAuditEvent(ctx, repository.AuditEvent{Timestamp: executor.server.now().UTC(), Actor: "fleet-scheduler", Action: action, Resource: "recommendations/" + record.ID, Namespace: subject.Namespace, Outcome: "success", RequestID: "fleet-scan/" + scan.ID})
	})
}
