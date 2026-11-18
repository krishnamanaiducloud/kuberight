from __future__ import annotations

import json
import math
from dataclasses import replace
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any

import numpy as np
import pytest
from fastapi.testclient import TestClient

from ml import service


def _samples(length: int = 168, period: int = 24, base: float = 2.0) -> list[float]:
    return [base + 0.6 * math.sin(2 * math.pi * index / period) for index in range(length)]


@pytest.fixture
def settings(tmp_path: Path) -> service.Settings:
    return service.Settings(
        min_samples=48,
        max_samples=512,
        max_horizon=48,
        max_body_bytes=100_000,
        max_concurrent_forecasts=2,
        prediction_timeout_seconds=4,
        training_sample_limit=512,
        model_dir=tmp_path / "models",
    )


@pytest.fixture
def client(settings: service.Settings) -> TestClient:
    return TestClient(service.create_app(settings))


def test_health_readiness_metrics_and_security_headers(client: TestClient) -> None:
    health = client.get("/healthz", headers={"X-Request-ID": "test-request-1"})
    assert health.status_code == 200
    assert health.json()["status"] == "ok"
    assert health.json()["version"] == "0.1.0-rc.4"
    assert health.headers["x-request-id"] == "test-request-1"
    assert health.headers["x-content-type-options"] == "nosniff"
    assert client.get("/readyz").json()["status"] == "ready"
    assert client.get("/openapi.json").json()["info"]["version"] == "0.1.0-rc.4"

    metrics = client.get("/metrics")
    assert metrics.status_code == 200
    assert "kuberight_ml_service_info" in metrics.text
    assert "kuberight_ml_forecast_duration_seconds" in metrics.text


@pytest.mark.parametrize(
    "payload",
    [
        {"samples": [-1.0] * 168},
        {"samples": [1.0] * 47},
        {"samples": [1.0] * 168, "horizon": "24"},
        {"samples": [1.0] * 168, "unknown": True},
        {"samples": [1.0] * 168, "persist": True},
        {"samples": [1.0] * 168, "confidence_level": 0.85},
        {"samples": [1.0] * 168, "series_id": "../escape", "persist": True},
    ],
)
def test_strict_request_validation(client: TestClient, payload: dict[str, Any]) -> None:
    response = client.post("/api/v1/forecast", json=payload)
    assert response.status_code == 422
    body = response.json()
    assert body["error"]["code"] == "validation_error"
    assert "requestId" in body
    # Sanitized errors do not reflect the potentially large submitted input.
    assert all("input" not in detail for detail in body["error"]["details"])


def test_timestamp_validation_rejects_duplicates(client: TestClient) -> None:
    start = datetime.now(UTC) - timedelta(hours=167)
    timestamps = [(start + timedelta(hours=index)).isoformat() for index in range(168)]
    timestamps[100] = timestamps[99]
    response = client.post(
        "/api/v1/forecast",
        json={"samples": _samples(), "timestamps": timestamps},
    )
    assert response.status_code == 422
    assert "strictly increasing" in response.text


def test_periodic_forecast_has_no_leakage_contract_and_intervals(
    client: TestClient,
) -> None:
    response = client.post(
        "/api/v1/forecast",
        json={
            "samples": _samples(192),
            "horizon": 12,
            "seasonal_period": 24,
            "resource": "cpu",
            "confidence_level": 0.9,
        },
    )
    assert response.status_code == 200, response.text
    body = response.json()
    assert len(body["predictions"]) == 12
    assert len(body["confidenceIntervals"]) == 12
    assert all(value >= 0 for value in body["predictions"])
    assert all(
        interval["lower"] <= prediction <= interval["upper"]
        for prediction, interval in zip(
            body["predictions"], body["confidenceIntervals"], strict=True
        )
    )
    assert body["seasonality"]["detected"] is True
    assert body["seasonality"]["period"] == 24
    assert body["validation"]["strategy"] == ("expanding-window-recursive-no-future-observations")
    assert body["validation"]["folds"] >= 2
    assert body["validation"]["trainingEndIndices"] == sorted(
        body["validation"]["trainingEndIndices"]
    )
    assert max(body["validation"]["trainingEndIndices"]) < 192
    # The exact seasonal baseline wins this series, so ML is not selected without evidence.
    assert body["fallbackUsed"] is True
    assert body["baselineValidationMAE"] == pytest.approx(0.0, abs=1e-8)
    assert body["experimental"] is True


def test_forecast_is_deterministic_for_identical_history(client: TestClient) -> None:
    payload = {"samples": [4.0] * 168, "horizon": 6}
    first = client.post("/api/v1/forecast", json=payload).json()
    second = client.post("/api/v1/forecast", json=payload).json()
    assert first["predictions"] == second["predictions"] == [4.0] * 6
    assert first["fallbackUsed"] is True
    assert first["model"].endswith("naive-v1")


def test_anomaly_and_drift_detection() -> None:
    periodic = np.asarray(_samples(192))
    periodic[170] = 20.0
    anomalies = service._anomalies(periodic, 24)
    assert anomalies.count >= 1
    assert 170 in anomalies.recent_indices

    shifted = np.concatenate((np.ones(120), np.full(72, 5.0)))
    drift = service._drift(shifted, 24)
    assert drift.detected is True
    assert drift.score >= 0.5


def test_validation_plan_has_no_synthetic_fold_for_minimum_history() -> None:
    cutoffs, fold_size = service._validation_plan(48, horizon=24, period=None)
    assert cutoffs == []
    assert fold_size == 24

    longer_cutoffs, _ = service._validation_plan(64, horizon=24, period=None)
    assert longer_cutoffs == [56]
    assert all(0 < cutoff < 64 for cutoff in longer_cutoffs)


def test_minimum_history_reports_validation_unavailable(client: TestClient) -> None:
    response = client.post(
        "/api/v1/forecast",
        json={"samples": _samples(48), "horizon": 3},
    )
    assert response.status_code == 200, response.text
    body = response.json()
    assert body["experimental"] is True
    assert body["fallbackUsed"] is True
    assert body["confidence"] == 0.0
    assert body["validationMAE"] is None
    assert body["baselineValidationMAE"] is None
    assert body["validation"] == {
        "strategy": "unavailable-insufficient-history-for-holdout",
        "folds": 0,
        "validationSamples": 0,
        "trainingEndIndices": [],
        "modelMAE": None,
        "baselineMAE": None,
        "improvementRatio": None,
    }
    assert all(interval["lower"] < interval["upper"] for interval in body["confidenceIntervals"])
    assert "conservative heuristics" in body["warning"]


def test_irregular_or_stale_series_uses_conservative_fallback(client: TestClient) -> None:
    start = datetime.now(UTC) - timedelta(days=30)
    timestamps = [start + timedelta(hours=index) for index in range(168)]
    timestamps[100] += timedelta(minutes=40)
    response = client.post(
        "/api/v1/forecast",
        json={
            "samples": _samples(),
            "timestamps": [stamp.isoformat() for stamp in timestamps],
            "horizon": 4,
        },
    )
    assert response.status_code == 200
    body = response.json()
    assert body["fallbackUsed"] is True
    assert body["model"] == "conservative-seasonal-baseline-v1"
    assert body["dataQuality"]["irregularIntervals"] > 0
    assert body["dataQuality"]["stale"] is True
    assert body["retraining"]["recommended"] is True


def test_persistence_and_metadata_endpoint(settings: service.Settings, client: TestClient) -> None:
    response = client.post(
        "/api/v1/forecast",
        json={
            "samples": _samples(),
            "horizon": 3,
            "series_id": "cluster-a.ns.workload:cpu",
            "persist": True,
        },
    )
    assert response.status_code == 200, response.text
    assert response.json()["modelMetadata"]["persisted"] is True

    metadata = client.get("/api/v1/models/cluster-a.ns.workload:cpu")
    assert metadata.status_code == 200
    assert metadata.json()["resource"] == "cpu"
    assert list(settings.model_dir.rglob("*.joblib"))
    assert list(settings.model_dir.rglob("latest.json"))


def test_repeated_identical_forecast_reuses_validated_persisted_model(
    settings: service.Settings,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    class FixedDateTime(datetime):
        @classmethod
        def now(cls, tz: Any = None) -> FixedDateTime:
            return cls(2026, 1, 2, 3, 4, 5, 678901, tzinfo=tz)

    monkeypatch.setattr(service, "datetime", FixedDateTime)
    client = TestClient(service.create_app(settings))
    payload = {
        "samples": _samples(),
        "horizon": 3,
        "interval_seconds": 1_800,
        "series_id": "cluster-a.ns.workload:cpu",
        "persist": True,
    }

    first = client.post("/api/v1/forecast", json=payload)
    second = client.post("/api/v1/forecast", json=payload)

    assert first.status_code == second.status_code == 200
    first_version = first.json()["modelVersion"]
    second_version = second.json()["modelVersion"]
    assert first_version == second_version
    assert first.json()["modelMetadata"]["reused"] is False
    assert second.json()["modelMetadata"]["reused"] is True
    metadata_paths = [
        path for path in settings.model_dir.rglob("*.json") if path.name != "latest.json"
    ]
    assert len(metadata_paths) == 1
    metadata = [json.loads(path.read_text(encoding="utf-8")) for path in metadata_paths]
    input_digests = {item["inputDigest"] for item in metadata}
    assert len(input_digests) == 1
    input_digest = input_digests.pop()
    assert input_digest[:20] in first_version
    assert input_digest[:20] in second_version
    assert len(list(settings.model_dir.rglob("*.joblib"))) == 1


def test_model_input_digest_covers_request_and_model_configuration(
    settings: service.Settings,
) -> None:
    request = service.ForecastRequest(
        samples=_samples(),
        horizon=3,
        interval_seconds=3_600,
        series_id="series",
        persist=True,
    )
    baseline = service._model_input_digest(
        request, settings, effective_period=24, model_name="seasonal-naive-v1"
    )
    changed_request = request.model_copy(update={"interval_seconds": 1_800})
    changed_config = replace(settings, min_model_improvement=0.25)

    assert (
        service._model_input_digest(
            changed_request,
            settings,
            effective_period=24,
            model_name="seasonal-naive-v1",
        )
        != baseline
    )
    assert (
        service._model_input_digest(
            request,
            changed_config,
            effective_period=24,
            model_name="seasonal-naive-v1",
        )
        != baseline
    )


def test_model_store_retains_bounded_versions(tmp_path: Path) -> None:
    store = service.ModelStore(tmp_path / "store", max_versions=2)
    for index in range(4):
        version = f"v{index}"
        store.save(
            "series",
            version,
            {"version": version, "estimator": None},
            {"seriesId": "series", "version": version},
        )
    series_dir = next(path for path in (tmp_path / "store").iterdir() if path.is_dir())
    assert len(list(series_dir.glob("v*.json"))) == 2
    assert len(list(series_dir.glob("v*.joblib"))) == 2
    assert store.latest_metadata("series") == {"seriesId": "series", "version": "v3"}


def test_model_store_refuses_to_overwrite_an_existing_version(tmp_path: Path) -> None:
    store = service.ModelStore(tmp_path / "store", max_versions=2)
    store.save(
        "series",
        "v1",
        {"version": "v1", "estimator": None},
        {"seriesId": "series", "version": "v1", "marker": "original"},
    )

    with pytest.raises(FileExistsError, match="already exists"):
        store.save(
            "series",
            "v1",
            {"version": "v1", "estimator": None},
            {"seriesId": "series", "version": "v1", "marker": "replacement"},
        )

    assert store.latest_metadata("series") == {
        "seriesId": "series",
        "version": "v1",
        "marker": "original",
    }


def test_corrupt_persisted_model_is_refused_and_retrained(
    settings: service.Settings, client: TestClient
) -> None:
    payload = {
        "samples": _samples(),
        "horizon": 3,
        "series_id": "corrupt-model",
        "persist": True,
    }
    first = client.post("/api/v1/forecast", json=payload)
    assert first.status_code == 200, first.text
    artifact = next(settings.model_dir.rglob("*.joblib"))
    artifact.write_bytes(b"not-a-trusted-joblib-artifact")

    second = client.post("/api/v1/forecast", json=payload)
    assert second.status_code == 200, second.text
    assert second.json()["modelMetadata"]["reused"] is False
    assert second.json()["modelVersion"] != first.json()["modelVersion"]


def test_fit_failure_reports_and_persists_final_baseline_mae(
    settings: service.Settings,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    def validation_stub(
        values: np.ndarray,
        period: int | None,
        horizon: int,
        deadline: service._Deadline,
    ) -> service._ValidationResult:
        del values, period, horizon, deadline
        return service._ValidationResult(
            folds=2,
            validation_samples=48,
            training_end_indices=[96, 120],
            model_mae=0.01,
            baseline_mae=2.5,
            improvement_ratio=0.996,
            model_residuals=[0.01] * 48,
            baseline_residuals=[2.5] * 48,
        )

    def fit_failure(values: np.ndarray, period: int | None) -> service.Pipeline:
        del values, period
        raise ValueError("simulated fit failure")

    monkeypatch.setattr(service, "_chronological_validation", validation_stub)
    monkeypatch.setattr(service, "_fit_model", fit_failure)
    client = TestClient(service.create_app(settings))
    response = client.post(
        "/api/v1/forecast",
        json={
            "samples": _samples(),
            "horizon": 3,
            "series_id": "fit-fallback",
            "persist": True,
        },
    )

    assert response.status_code == 200, response.text
    body = response.json()
    assert body["fallbackUsed"] is True
    assert body["model"].endswith("naive-v1")
    assert body["validation"]["modelMAE"] == pytest.approx(0.01)
    assert body["validationMAE"] == pytest.approx(2.5)
    assert body["baselineValidationMAE"] == pytest.approx(2.5)
    metadata = client.get("/api/v1/models/fit-fallback")
    assert metadata.status_code == 200
    assert metadata.json()["validationMAE"] == pytest.approx(2.5)


def test_api_key_protects_data_routes_but_not_probes(settings: service.Settings) -> None:
    protected = replace(settings, api_key="a-secure-test-key")
    client = TestClient(service.create_app(protected))
    payload = {"samples": [1.0] * 168, "horizon": 1}
    assert client.get("/healthz").status_code == 200
    assert client.post("/api/v1/forecast", json=payload).status_code == 401
    assert (
        client.post(
            "/api/v1/forecast",
            json=payload,
            headers={"X-API-Key": "wrong-key"},
        ).status_code
        == 401
    )
    authorized = client.post(
        "/api/v1/forecast",
        json=payload,
        headers={"X-API-Key": "a-secure-test-key"},
    )
    assert authorized.status_code == 200


def test_request_body_limit(settings: service.Settings) -> None:
    limited = replace(settings, max_body_bytes=1_024)
    client = TestClient(service.create_app(limited))
    response = client.post("/api/v1/forecast", content=b"x" * 1_025)
    assert response.status_code == 413
    assert response.json()["error"]["code"] == "payload_too_large"


def test_cooperative_compute_budget_returns_safe_fallback(
    settings: service.Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    def exhausted(_self: service._Deadline) -> None:
        raise service._DeadlineExceeded

    monkeypatch.setattr(service._Deadline, "check", exhausted)
    client = TestClient(service.create_app(settings))
    response = client.post("/api/v1/forecast", json={"samples": _samples(), "horizon": 3})
    assert response.status_code == 200
    body = response.json()
    assert body["fallbackUsed"] is True
    assert body["confidence"] == 0.0
    assert body["experimental"] is True
    assert body["validationMAE"] is None
    assert body["baselineValidationMAE"] is None
    assert body["validation"]["folds"] == 0
    assert body["validation"]["baselineMAE"] is None
    assert all(interval["lower"] < interval["upper"] for interval in body["confidenceIntervals"])
    assert "rather than empirically calibrated confidence intervals" in body["warning"]
    assert "cooperative compute budget" in body["fallbackReasons"][0]


def test_readiness_distinguishes_unwritable_store(
    settings: service.Settings, tmp_path: Path
) -> None:
    not_a_directory = tmp_path / "file"
    not_a_directory.write_text("occupied", encoding="utf-8")
    broken = replace(settings, model_dir=not_a_directory)
    client = TestClient(service.create_app(broken))
    assert client.get("/healthz").status_code == 200
    readiness = client.get("/readyz")
    assert readiness.status_code == 503
    assert readiness.json()["status"] == "not-ready"


def test_settings_reject_insecure_api_key() -> None:
    with pytest.raises(ValueError, match="at least 16"):
        service.Settings(api_key="short")


SIGNING_KEY = "k" * 48


def _signed_store(tmp_path: Path, key: str | None = SIGNING_KEY) -> service.ModelStore:
    return service.ModelStore(tmp_path / "store", max_versions=2, signing_key=key)


def _save_loadable(store: service.ModelStore, version: str = "v1") -> None:
    store.save(
        "series",
        version,
        {
            "artifact_schema": 1,
            "feature_schema": service.MODEL_SCHEMA_VERSION,
            "model_name": "seasonal-naive-v1",
            "estimator": None,
            "period": 24,
            "input_digest": "digest",
            "version": version,
        },
        {
            "seriesId": "series",
            "version": version,
            "name": "seasonal-naive-v1",
            "featureSchema": service.MODEL_SCHEMA_VERSION,
            "inputDigest": "digest",
            "resource": "cpu",
        },
    )


def _artifact_path(tmp_path: Path, version: str = "v1") -> Path:
    series_dir = next(path for path in (tmp_path / "store").iterdir() if path.is_dir())
    return series_dir / f"{version}.joblib"


def test_signed_artifacts_load_and_carry_a_signature(tmp_path: Path) -> None:
    store = _signed_store(tmp_path)
    _save_loadable(store)
    assert _artifact_path(tmp_path).with_suffix(".mac").is_file()
    loaded = store.load_latest("series", "digest", "cpu")
    assert loaded is not None
    assert loaded[0]["version"] == "v1"


def test_tampered_artifacts_are_refused_before_unpickling(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    store = _signed_store(tmp_path)
    _save_loadable(store)
    artifact = _artifact_path(tmp_path)
    artifact.write_bytes(artifact.read_bytes() + b"\x00")

    def never(*_: Any, **__: Any) -> Any:
        raise AssertionError("a tampered artifact must not be deserialized")

    monkeypatch.setattr(service.joblib, "load", never)
    assert store.load_latest("series", "digest", "cpu") is None


@pytest.mark.parametrize(
    ("writer_key", "reader_key", "remove_signature"),
    [
        (None, SIGNING_KEY, False),  # unsigned artifacts are refused once signing is on
        ("x" * 48, SIGNING_KEY, False),  # signed with a different key
        (SIGNING_KEY, SIGNING_KEY, True),  # signature file deleted
    ],
)
def test_artifacts_without_a_valid_signature_are_refused(
    tmp_path: Path, writer_key: str | None, reader_key: str, remove_signature: bool
) -> None:
    _save_loadable(_signed_store(tmp_path, writer_key))
    if remove_signature:
        _artifact_path(tmp_path).with_suffix(".mac").unlink()
    assert _signed_store(tmp_path, reader_key).load_latest("series", "digest", "cpu") is None


def test_signatures_bind_the_version(tmp_path: Path) -> None:
    store = _signed_store(tmp_path)
    _save_loadable(store, "v1")
    _save_loadable(store, "v2")
    v1, v2 = _artifact_path(tmp_path, "v1"), _artifact_path(tmp_path, "v2")
    # A validly signed artifact copied under another version's name is refused.
    v2.with_suffix(".mac").write_text(v1.with_suffix(".mac").read_text())
    v2.write_bytes(v1.read_bytes())
    assert store.load_latest("series", "digest", "cpu") is None


def test_obsolete_versions_remove_their_signatures(tmp_path: Path) -> None:
    store = _signed_store(tmp_path)
    for index in range(4):
        _save_loadable(store, f"v{index}")
    series_dir = _artifact_path(tmp_path, "v3").parent
    assert sorted(path.name for path in series_dir.glob("*.mac")) == ["v2.mac", "v3.mac"]


def test_short_artifact_keys_are_rejected() -> None:
    with pytest.raises(ValueError, match="KUBERIGHT_ML_ARTIFACT_KEY"):
        service.Settings(artifact_key="short")


def test_readiness_reports_artifact_signing(tmp_path: Path) -> None:
    settings = service.Settings(model_dir=tmp_path / "models", artifact_key=SIGNING_KEY)
    client = TestClient(service.create_app(settings))
    body = client.get("/readyz").json()
    assert body["checks"]["artifactSigning"] == "enabled"
