package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kuberight/kuberight/internal/repository"
)

const verificationOutcomeColumns = `latest_run_id, recommendation_id, status, configuration_observed,
first_observed_at, observed_generation, observed_spec_fingerprint, post_change_window_seconds,
evidence_complete, attribution, checks, reasons, checked_at, regression_notified_at`

const verificationRunColumns = `id, recommendation_id, status, configuration_observed,
first_observed_at, observed_generation, observed_spec_fingerprint, post_change_window_seconds,
evidence_complete, attribution, checks, reasons, checked_at`

func (store *Store) RecordVerification(ctx context.Context, run repository.VerificationRun) (outcome repository.VerificationOutcome, err error) {
	if err := repository.ValidateVerificationRun(run); err != nil {
		return repository.VerificationOutcome{}, err
	}
	if err := ctx.Err(); err != nil {
		return repository.VerificationOutcome{}, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return repository.VerificationOutcome{}, fmt.Errorf("begin verification transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			rollbackContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = tx.Rollback(rollbackContext)
		}
	}()
	outcome, err = recordVerification(ctx, tx, run)
	if err != nil {
		return repository.VerificationOutcome{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return repository.VerificationOutcome{}, fmt.Errorf("commit verification transaction: %w", err)
	}
	committed = true
	return outcome, nil
}

func recordVerification(ctx context.Context, db queryer, run repository.VerificationRun) (repository.VerificationOutcome, error) {
	if run.ID == "" {
		id, err := repository.NewVerificationRunID()
		if err != nil {
			return repository.VerificationOutcome{}, errors.New("could not generate verification run identifier")
		}
		run.ID = id
	}
	// Lock an existing outcome before preserving its first exact observation.
	existing, err := scanVerificationOutcome(db.QueryRow(ctx, "SELECT "+verificationOutcomeColumns+" FROM recommendation_verification_outcomes WHERE recommendation_id = $1 FOR UPDATE", run.RecommendationID))
	if err == nil && existing.FirstObservedAt != nil && (run.FirstObservedAt == nil || existing.FirstObservedAt.Before(*run.FirstObservedAt)) {
		first := existing.FirstObservedAt.UTC()
		run.FirstObservedAt = &first
		run.ObservedGeneration = existing.ObservedGeneration
		run.ObservedSpecFingerprint = existing.ObservedSpecFingerprint
	} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return repository.VerificationOutcome{}, fmt.Errorf("lock verification outcome: %w", err)
	}
	checks, err := json.Marshal(run.Checks)
	if err != nil {
		return repository.VerificationOutcome{}, fmt.Errorf("encode verification checks: %w", err)
	}
	reasons, err := json.Marshal(run.Reasons)
	if err != nil {
		return repository.VerificationOutcome{}, fmt.Errorf("encode verification reasons: %w", err)
	}
	_, err = db.Exec(ctx, `INSERT INTO recommendation_verification_runs
(id, recommendation_id, status, configuration_observed, first_observed_at, observed_generation,
 observed_spec_fingerprint, post_change_window_seconds, evidence_complete, attribution, checks, reasons, checked_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		run.ID, run.RecommendationID, run.Status, run.ConfigurationObserved, run.FirstObservedAt, run.ObservedGeneration,
		run.ObservedSpecFingerprint, run.PostChangeWindowSeconds, run.EvidenceComplete, run.Attribution, checks, reasons, run.CheckedAt.UTC())
	if err != nil {
		return repository.VerificationOutcome{}, fmt.Errorf("append verification run: %w", err)
	}
	row := db.QueryRow(ctx, `INSERT INTO recommendation_verification_outcomes
(recommendation_id, latest_run_id, status, configuration_observed, first_observed_at, observed_generation,
 observed_spec_fingerprint, post_change_window_seconds, evidence_complete, attribution, checks, reasons, checked_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
ON CONFLICT (recommendation_id) DO UPDATE SET
 latest_run_id = EXCLUDED.latest_run_id,
 status = EXCLUDED.status,
 configuration_observed = EXCLUDED.configuration_observed,
 first_observed_at = CASE
   WHEN recommendation_verification_outcomes.first_observed_at IS NULL THEN EXCLUDED.first_observed_at
   WHEN EXCLUDED.first_observed_at IS NULL THEN recommendation_verification_outcomes.first_observed_at
   ELSE LEAST(recommendation_verification_outcomes.first_observed_at, EXCLUDED.first_observed_at)
 END,
 observed_generation = CASE
   WHEN recommendation_verification_outcomes.first_observed_at IS NULL THEN EXCLUDED.observed_generation
   WHEN EXCLUDED.first_observed_at IS NOT NULL AND EXCLUDED.first_observed_at < recommendation_verification_outcomes.first_observed_at THEN EXCLUDED.observed_generation
   ELSE recommendation_verification_outcomes.observed_generation
 END,
 observed_spec_fingerprint = CASE
   WHEN recommendation_verification_outcomes.first_observed_at IS NULL THEN EXCLUDED.observed_spec_fingerprint
   WHEN EXCLUDED.first_observed_at IS NOT NULL AND EXCLUDED.first_observed_at < recommendation_verification_outcomes.first_observed_at THEN EXCLUDED.observed_spec_fingerprint
   ELSE recommendation_verification_outcomes.observed_spec_fingerprint
 END,
 post_change_window_seconds = EXCLUDED.post_change_window_seconds,
 evidence_complete = EXCLUDED.evidence_complete,
 attribution = EXCLUDED.attribution,
 checks = EXCLUDED.checks,
 reasons = EXCLUDED.reasons,
 checked_at = EXCLUDED.checked_at
RETURNING `+verificationOutcomeColumns,
		run.RecommendationID, run.ID, run.Status, run.ConfigurationObserved, run.FirstObservedAt, run.ObservedGeneration,
		run.ObservedSpecFingerprint, run.PostChangeWindowSeconds, run.EvidenceComplete, run.Attribution, checks, reasons, run.CheckedAt.UTC())
	outcome, err := scanVerificationOutcome(row)
	if err != nil {
		return repository.VerificationOutcome{}, fmt.Errorf("store verification outcome: %w", err)
	}
	return outcome, nil
}

func scanVerificationOutcome(row pgx.Row) (repository.VerificationOutcome, error) {
	var outcome repository.VerificationOutcome
	var checks, reasons []byte
	if err := row.Scan(&outcome.ID, &outcome.RecommendationID, &outcome.Status, &outcome.ConfigurationObserved,
		&outcome.FirstObservedAt, &outcome.ObservedGeneration, &outcome.ObservedSpecFingerprint, &outcome.PostChangeWindowSeconds,
		&outcome.EvidenceComplete, &outcome.Attribution, &checks, &reasons, &outcome.CheckedAt, &outcome.RegressionNotifiedAt); err != nil {
		return repository.VerificationOutcome{}, err
	}
	if err := json.Unmarshal(checks, &outcome.Checks); err != nil {
		return repository.VerificationOutcome{}, fmt.Errorf("decode verification checks: %w", err)
	}
	if err := json.Unmarshal(reasons, &outcome.Reasons); err != nil {
		return repository.VerificationOutcome{}, fmt.Errorf("decode verification reasons: %w", err)
	}
	normalizeVerificationTimes(&outcome.VerificationRun, &outcome.RegressionNotifiedAt)
	return outcome, nil
}

func scanVerificationRun(row pgx.Row) (repository.VerificationRun, error) {
	var run repository.VerificationRun
	var checks, reasons []byte
	if err := row.Scan(&run.ID, &run.RecommendationID, &run.Status, &run.ConfigurationObserved,
		&run.FirstObservedAt, &run.ObservedGeneration, &run.ObservedSpecFingerprint, &run.PostChangeWindowSeconds,
		&run.EvidenceComplete, &run.Attribution, &checks, &reasons, &run.CheckedAt); err != nil {
		return repository.VerificationRun{}, err
	}
	if err := json.Unmarshal(checks, &run.Checks); err != nil {
		return repository.VerificationRun{}, fmt.Errorf("decode verification checks: %w", err)
	}
	if err := json.Unmarshal(reasons, &run.Reasons); err != nil {
		return repository.VerificationRun{}, fmt.Errorf("decode verification reasons: %w", err)
	}
	normalizeVerificationTimes(&run, nil)
	return run, nil
}

func normalizeVerificationTimes(run *repository.VerificationRun, notified **time.Time) {
	run.CheckedAt = run.CheckedAt.UTC()
	if run.FirstObservedAt != nil {
		value := run.FirstObservedAt.UTC()
		run.FirstObservedAt = &value
	}
	if notified != nil && *notified != nil {
		value := (*notified).UTC()
		*notified = &value
	}
}

func (store *Store) GetVerificationOutcome(ctx context.Context, recommendationID string) (repository.VerificationOutcome, error) {
	outcome, err := scanVerificationOutcome(store.pool.QueryRow(ctx, "SELECT "+verificationOutcomeColumns+" FROM recommendation_verification_outcomes WHERE recommendation_id = $1", recommendationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return repository.VerificationOutcome{}, repository.ErrNotFound
	}
	if err != nil {
		return repository.VerificationOutcome{}, fmt.Errorf("read verification outcome: %w", err)
	}
	return outcome, nil
}

func (store *Store) ListVerificationOutcomes(ctx context.Context, page, pageSize int) (repository.VerificationOutcomeListResult, error) {
	if page < 1 || pageSize < 1 || pageSize > 10_000 {
		return repository.VerificationOutcomeListResult{}, errors.New("page must be positive and pageSize must be between 1 and 10000")
	}
	rows, err := store.pool.Query(ctx, "SELECT "+verificationOutcomeColumns+", count(*) OVER() FROM recommendation_verification_outcomes ORDER BY checked_at DESC, recommendation_id DESC LIMIT $1 OFFSET $2", pageSize, (page-1)*pageSize)
	if err != nil {
		return repository.VerificationOutcomeListResult{}, fmt.Errorf("list verification outcomes: %w", err)
	}
	defer rows.Close()
	result := repository.VerificationOutcomeListResult{Items: []repository.VerificationOutcome{}, Page: page, PageSize: pageSize}
	for rows.Next() {
		var outcome repository.VerificationOutcome
		var checks, reasons []byte
		var total int64
		if err := rows.Scan(&outcome.ID, &outcome.RecommendationID, &outcome.Status, &outcome.ConfigurationObserved,
			&outcome.FirstObservedAt, &outcome.ObservedGeneration, &outcome.ObservedSpecFingerprint, &outcome.PostChangeWindowSeconds,
			&outcome.EvidenceComplete, &outcome.Attribution, &checks, &reasons, &outcome.CheckedAt, &outcome.RegressionNotifiedAt, &total); err != nil {
			return repository.VerificationOutcomeListResult{}, fmt.Errorf("scan verification outcome: %w", err)
		}
		if err := json.Unmarshal(checks, &outcome.Checks); err != nil {
			return repository.VerificationOutcomeListResult{}, fmt.Errorf("decode verification checks: %w", err)
		}
		if err := json.Unmarshal(reasons, &outcome.Reasons); err != nil {
			return repository.VerificationOutcomeListResult{}, fmt.Errorf("decode verification reasons: %w", err)
		}
		normalizeVerificationTimes(&outcome.VerificationRun, &outcome.RegressionNotifiedAt)
		result.Total = int(total)
		result.Items = append(result.Items, outcome)
	}
	if err := rows.Err(); err != nil {
		return repository.VerificationOutcomeListResult{}, fmt.Errorf("list verification outcomes: %w", err)
	}
	return result, nil
}

func (store *Store) ListVerificationRuns(ctx context.Context, recommendationID string, page, pageSize int) (repository.VerificationRunListResult, error) {
	if page < 1 || pageSize < 1 || pageSize > 10_000 {
		return repository.VerificationRunListResult{}, errors.New("page must be positive and pageSize must be between 1 and 10000")
	}
	rows, err := store.pool.Query(ctx, "SELECT "+verificationRunColumns+", count(*) OVER() FROM recommendation_verification_runs WHERE recommendation_id = $1 ORDER BY checked_at DESC, id DESC LIMIT $2 OFFSET $3", recommendationID, pageSize, (page-1)*pageSize)
	if err != nil {
		return repository.VerificationRunListResult{}, fmt.Errorf("list verification runs: %w", err)
	}
	defer rows.Close()
	result := repository.VerificationRunListResult{Items: []repository.VerificationRun{}, Page: page, PageSize: pageSize}
	for rows.Next() {
		var run repository.VerificationRun
		var checks, reasons []byte
		var total int64
		if err := rows.Scan(&run.ID, &run.RecommendationID, &run.Status, &run.ConfigurationObserved,
			&run.FirstObservedAt, &run.ObservedGeneration, &run.ObservedSpecFingerprint, &run.PostChangeWindowSeconds,
			&run.EvidenceComplete, &run.Attribution, &checks, &reasons, &run.CheckedAt, &total); err != nil {
			return repository.VerificationRunListResult{}, fmt.Errorf("scan verification run: %w", err)
		}
		if err := json.Unmarshal(checks, &run.Checks); err != nil {
			return repository.VerificationRunListResult{}, fmt.Errorf("decode verification checks: %w", err)
		}
		if err := json.Unmarshal(reasons, &run.Reasons); err != nil {
			return repository.VerificationRunListResult{}, fmt.Errorf("decode verification reasons: %w", err)
		}
		normalizeVerificationTimes(&run, nil)
		result.Total = int(total)
		result.Items = append(result.Items, run)
	}
	if err := rows.Err(); err != nil {
		return repository.VerificationRunListResult{}, fmt.Errorf("list verification runs: %w", err)
	}
	return result, nil
}

func (store *Store) ClaimRegressionNotification(ctx context.Context, recommendationID string, checkedAt, notifiedAt time.Time) (bool, error) {
	if notifiedAt.IsZero() {
		return false, errors.New("regression notification time is required")
	}
	tag, err := store.pool.Exec(ctx, `UPDATE recommendation_verification_outcomes
SET regression_notified_at = $3
WHERE recommendation_id = $1 AND status = 'regressed' AND checked_at = $2 AND regression_notified_at IS NULL`, recommendationID, checkedAt.UTC(), notifiedAt.UTC())
	if err != nil {
		return false, fmt.Errorf("claim regression notification: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
