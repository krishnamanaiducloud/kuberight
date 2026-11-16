package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kuberight/kuberight/internal/automation"
	"github.com/kuberight/kuberight/internal/config"
	"github.com/kuberight/kuberight/internal/repository"
	"github.com/kuberight/kuberight/internal/usage"
)

// earlyVerifier seeds an approved, live recommendation whose target values
// were first observed observedAgo before fixedNow.
func earlyVerifier(t *testing.T, events func(*usage.Events), observedAgo time.Duration) (testAPI, *Verifier, repository.RecommendationRecord) {
	t.Helper()
	source := verificationUsage()
	events(&source.events)
	dependencies := Dependencies{Provider: fakeInventory{spec: verificationSpec()}, Usage: source, Notifier: &recordingNotifier{}}
	api := newTestAPI(t, nil, dependencies)
	record := storeApproved(t, api, liveRecommendation(), fixedNow.Add(-5*time.Hour))
	first := fixedNow.Add(-observedAgo)
	if _, err := api.store.RecordVerification(context.Background(), repository.VerificationRun{
		RecommendationID: record.ID, Status: repository.VerificationPending, ConfigurationObserved: true,
		FirstObservedAt: &first, ObservedGeneration: 13, ObservedSpecFingerprint: strings.Repeat("b", 64),
		Attribution: repository.VerificationAttributionExactPodUID, Checks: []repository.VerificationCheck{}, Reasons: []string{"waiting"}, CheckedAt: first,
	}); err != nil {
		t.Fatal(err)
	}
	dependencies.Repository, dependencies.Access, dependencies.Now = api.store, api.access, func() time.Time { return fixedNow }
	cfg := fallbackConfig()
	cfg.VerificationInterval, cfg.VerificationEarlyInterval = 15*time.Minute, 2*time.Minute
	verifier, err := NewVerifier(cfg, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	return api, verifier, record
}

func TestEarlyVerificationFlagsSevereRegressionsWithinMinutes(t *testing.T) {
	for name, test := range map[string]struct {
		events  func(*usage.Events)
		ago     time.Duration
		status  string
		reason  string
		checked int
	}{
		"OOM kill":          {func(events *usage.Events) { events.OOMKills = 1 }, 10 * time.Minute, VerificationRegressed, "early check after 10m0s: 1 OOM kill(s)", 1},
		"restart":           {func(events *usage.Events) { events.Restarts = 2 }, 8 * time.Minute, VerificationRegressed, "2 restart(s) occurred", 1},
		"severe throttling": {func(events *usage.Events) { events.ThrottlingRatio = 0.4 }, 10 * time.Minute, VerificationRegressed, "cPU throttling changed from", 1},
		"CPU pressure": {func(events *usage.Events) {
			events.PressureAvailable, events.CPUPressureRatio = true, 0.3
		}, 10 * time.Minute, VerificationRegressed, "pressure changed from 0.0% before analysis to 30.0%", 1},
		// Moderate throttling waits for the hourly verdict.
		"moderate throttling": {func(events *usage.Events) { events.ThrottlingRatio = 0.15 }, 10 * time.Minute, VerificationPending, "found no OOM kill, restart, or severe throttling or pressure", 1},
		"clean":               {func(*usage.Events) {}, 10 * time.Minute, VerificationPending, "found no OOM kill", 1},
		// Before five minutes a rollout may not have settled.
		"too early": {func(events *usage.Events) { events.OOMKills = 1 }, 3 * time.Minute, VerificationPending, "only 3m0s of exact post-change observation", 1},
		"no events": {func(events *usage.Events) { events.Available = false }, 10 * time.Minute, VerificationPending, "could not read restart and OOM evidence", 1},
		// After an hour the full pass, not the early pass, decides.
		"past the first hour": {func(events *usage.Events) { events.OOMKills = 1 }, 2 * time.Hour, VerificationPending, "waiting", 0},
	} {
		api, verifier, record := earlyVerifier(t, test.events, test.ago)
		if checked := verifier.PassRecent(context.Background()); checked != test.checked {
			t.Fatalf("%s: checked %d", name, checked)
		}
		outcome, err := api.store.GetVerificationOutcome(context.Background(), record.ID)
		if err != nil || outcome.Status != test.status || !strings.Contains(strings.Join(outcome.Reasons, "\n"), test.reason) {
			t.Fatalf("%s: outcome=%+v err=%v", name, outcome, err)
		}
	}
}

func TestVerificationFollowsALiveChangeAfterANewerRecommendation(t *testing.T) {
	// The change has been live for two hours, past the early checks, and
	// its container was OOM-killed. A newer recommendation for the same
	// container is now the latest one.
	supersede := func(t *testing.T, api testAPI) {
		t.Helper()
		if _, _, err := api.store.CreateRecommendation(context.Background(), liveRecommendation(), repository.CreateOptions{CreatedBy: "alice", Source: repository.SourcePrometheus}); err != nil {
			t.Fatal(err)
		}
	}
	api, verifier, record := earlyVerifier(t, func(events *usage.Events) { events.OOMKills = 1 }, 2*time.Hour)
	supersede(t, api)
	if latest, err := api.store.ListRecommendations(context.Background(), repository.ListOptions{Page: 1, PageSize: 10, ReviewStatus: repository.ReviewApproved, LatestOnly: true}); err != nil || len(latest.Items) != 0 {
		t.Fatalf("the approved change is still the latest: %+v err=%v", latest.Items, err)
	}
	if checked := verifier.Pass(context.Background()); checked != 1 {
		t.Fatalf("checked = %d", checked)
	}
	outcome, err := api.store.GetVerificationOutcome(context.Background(), record.ID)
	if err != nil || outcome.Status != VerificationRegressed || !outcome.CheckedAt.Equal(fixedNow) {
		t.Fatalf("the live change was no longer verified: %+v err=%v", outcome, err)
	}
	// A verdict ends the watch: the superseded change is not checked again.
	if checked := verifier.Pass(context.Background()); checked != 0 {
		t.Fatalf("a finished verification was repeated: %d", checked)
	}

	// While the change is the latest, it is checked once, not twice.
	api, verifier, _ = earlyVerifier(t, func(*usage.Events) {}, 2*time.Hour)
	if checked := verifier.Pass(context.Background()); checked != 1 {
		t.Fatalf("checked = %d", checked)
	}

	// A superseded approval whose values were never seen live is not followed.
	api, verifier, record = earlyVerifier(t, func(*usage.Events) {}, 2*time.Hour)
	if _, err := api.store.RecordVerification(context.Background(), repository.VerificationRun{
		RecommendationID: record.ID, Status: repository.VerificationNotApplied, Attribution: repository.VerificationAttributionNone,
		Checks: []repository.VerificationCheck{}, Reasons: []string{"not live"}, CheckedAt: fixedNow.Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	supersede(t, api)
	if checked := verifier.Pass(context.Background()); checked != 0 {
		t.Fatalf("an approval that is not live was followed: %d", checked)
	}
}

func TestEarlyChecksIgnoreEventsOutsideTheIdentity(t *testing.T) {
	api, verifier, record := earlyVerifier(t, func(events *usage.Events) {
		events.OOMKills = 1
		events.Replicas[0].UID = "someone-else"
	}, 10*time.Minute)
	verifier.PassRecent(context.Background())
	if outcome, _ := api.store.GetVerificationOutcome(context.Background(), record.ID); outcome.Status != VerificationPending {
		t.Fatalf("foreign evidence produced a verdict: %+v", outcome)
	}
}

func TestRollbacksIgnoreTheChangeWindowAndRunEarly(t *testing.T) {
	recommendation := automationRecommendation()
	spec := automationSpec()
	spec.Containers[0].CPURequestMilli, spec.Containers[0].CPULimitMilli = recommendation.CPURequestMilli, recommendation.CPULimitMilli
	spec.Containers[0].MemoryRequestBytes, spec.Containers[0].MemoryLimitBytes = recommendation.MemoryRequestBytes, recommendation.MemoryLimitBytes
	spec.Generation, spec.ResourceVersion = 13, "9100"
	closed := fixedNow.UTC().Add(time.Hour).Format("15:04") + "-" + fixedNow.UTC().Add(2*time.Hour).Format("15:04")
	harness := newAutomationHarness(t, config.ModeApply, spec, func(cfg *config.Config) { cfg.AutomationWindow = closed })
	record := storeApproved(t, harness.api, recommendation, fixedNow.Add(-3*time.Hour))
	harness.inventory.spec.Annotations = map[string]string{automation.AppliedAnnotation: record.ID}
	harness = newAutomationHarness(t, config.ModeApply, harness.inventory.spec, func(cfg *config.Config) { cfg.AutomationWindow = closed })
	record = storeApproved(t, harness.api, recommendation, fixedNow.Add(-3*time.Hour))
	harness.inventory.spec.Annotations[automation.AppliedAnnotation] = record.ID
	first := fixedNow.Add(-10 * time.Minute)
	if _, err := harness.api.store.RecordVerification(context.Background(), repository.VerificationRun{
		RecommendationID: record.ID, Status: repository.VerificationRegressed, ConfigurationObserved: true, FirstObservedAt: &first,
		ObservedGeneration: 13, ObservedSpecFingerprint: strings.Repeat("b", 64), EvidenceComplete: true, Attribution: repository.VerificationAttributionExactPodUID,
		Checks: []repository.VerificationCheck{}, Reasons: []string{"early check after 10m0s: 1 OOM kill(s) occurred"}, CheckedAt: fixedNow,
	}); err != nil {
		t.Fatal(err)
	}
	_, body := harness.api.do(t, http.MethodPost, "/api/v1/automation:run", "alice", "")
	if decision := decisionFor(t, body); decision["outcome"] != AutomationRolledBack || body["windowOpen"] != false || len(*harness.patches) != 1 || !(*harness.patches)[0].Rollback {
		t.Fatalf("a closed window deferred a rollback: body=%v patches=%v", body, *harness.patches)
	}
}

func TestRollbackPassEvaluatesOnlyRegressedChanges(t *testing.T) {
	harness := newAutomationHarness(t, config.ModeApply, automationSpec(), nil)
	// A fresh approved recommendation would be applied by a full pass, but a
	// rollback pass leaves it alone.
	storeApproved(t, harness.api, automationRecommendation(), fixedNow.Add(-time.Hour))
	cfg := fallbackConfig()
	cfg.OperationMode, cfg.AuthMode, cfg.APIToken = config.ModeApply, "kubernetes", ""
	automator, err := NewAutomator(cfg, Dependencies{Provider: harness.inventory, Repository: harness.api.store, Access: harness.api.access, Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatal(err)
	}
	report, err := automator.server.automationRun(context.Background(), false, true)
	if err != nil || report.Evaluated != 0 || len(report.Decisions) != 0 || len(*harness.patches) != 0 {
		t.Fatalf("rollback pass: report=%+v err=%v patches=%v", report, err, *harness.patches)
	}
	if report, err := automator.server.automationRun(context.Background(), false, false); err != nil || report.Applied != 1 {
		t.Fatalf("full pass: report=%+v err=%v", report, err)
	}
}
