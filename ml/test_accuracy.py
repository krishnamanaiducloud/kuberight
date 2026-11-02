from __future__ import annotations

import math
from pathlib import Path

import numpy as np
import pytest
from fastapi.testclient import TestClient

from ml import benchmark, service


def _periodic(length: int, noise: float, seed: int = 7) -> list[float]:
    random = np.random.default_rng(seed)
    values = [
        2.0 + 0.6 * math.sin(2 * math.pi * index / 24) + float(random.normal(0, noise))
        for index in range(length)
    ]
    return [max(0.0, value) for value in values]


@pytest.fixture
def settings(tmp_path: Path) -> service.Settings:
    return service.Settings(
        min_samples=48,
        max_samples=2_000,
        max_horizon=48,
        max_body_bytes=500_000,
        max_concurrent_forecasts=2,
        prediction_timeout_seconds=10,
        training_sample_limit=2_000,
        model_dir=tmp_path / "models",
    )


@pytest.fixture
def client(settings: service.Settings) -> TestClient:
    return TestClient(service.create_app(settings))


def test_calibration_widens_intervals_that_are_too_narrow() -> None:
    # Offsets learned from calm early folds understate a noisier last fold.
    calm = [float(value) for value in np.random.default_rng(1).normal(0, 0.1, 48)]
    noisy = [float(value) for value in np.random.default_rng(2).normal(0, 0.4, 24)]
    summary = service._calibrate(calm + noisy, [24, 24, 24], 0.9)
    assert summary.held_out_coverage is not None and summary.held_out_coverage < 0.9
    assert summary.factor > 1.0
    assert summary.calibrated_coverage is not None
    assert summary.calibrated_coverage >= summary.held_out_coverage
    assert summary.evaluation_samples == 24


def test_calibration_keeps_well_calibrated_intervals_and_caps_the_factor() -> None:
    stable = [float(value) for value in np.random.default_rng(3).normal(0, 0.2, 72)]
    summary = service._calibrate(stable, [24, 24, 24], 0.8)
    assert summary.factor <= 1.2
    assert summary.calibrated
    # A last fold far outside anything seen cannot be calibrated within the cap.
    shocked = stable[:48] + [50.0] * 24
    capped = service._calibrate(shocked, [24, 24, 24], 0.9)
    assert capped.factor == service.MAX_CALIBRATION_FACTOR
    assert not capped.calibrated
    # Too little data to hold any back.
    assert not service._calibrate(stable[:10], [10], 0.9).calibrated
    assert service._calibrate(stable, [70, 2], 0.9).held_out_coverage is None
    assert service._calibrate(stable, [24, 24], 0.9).held_out_coverage is None


def test_forecast_reports_calibration(client: TestClient) -> None:
    response = client.post(
        "/api/v1/forecast",
        json={"samples": _periodic(240, 0.05), "horizon": 12, "confidence_level": 0.9},
    )
    assert response.status_code == 200
    calibration = response.json()["calibration"]
    assert calibration["method"] == "split-conformal-last-fold"
    assert calibration["nominalCoverage"] == 0.9
    assert calibration["evaluationSamples"] > 0
    assert 1.0 <= calibration["factor"] <= service.MAX_CALIBRATION_FACTOR


def test_backtest_scores_rolling_origins(client: TestClient) -> None:
    response = client.post(
        "/api/v1/backtest",
        json={
            "samples": _periodic(288, 0.05),
            "horizon": 12,
            "origins": 3,
            "confidence_level": 0.9,
        },
    )
    assert response.status_code == 200
    body = response.json()
    assert body["method"] == "rolling-origin-no-future-observations"
    assert len(body["origins"]) == 3
    assert [origin["originIndex"] for origin in body["origins"]] == [252, 264, 276]
    overall = body["overall"]
    assert overall["samples"] == 36
    assert 0.0 <= overall["coverage"] <= 1.0
    assert overall["mae"] >= 0 and overall["rmse"] >= overall["mae"]
    assert overall["skill"] is None or overall["skill"] <= 1.0
    assert len(body["points"]) == 12
    point = body["points"][0]
    assert point["index"] == 276
    assert point["lower"] <= point["predicted"] <= point["upper"]


def test_backtest_validation(client: TestClient) -> None:
    too_short = {"samples": [1.0] * 60, "horizon": 12, "origins": 3}
    assert client.post("/api/v1/backtest", json=too_short).status_code == 422
    bad_level = {"samples": [1.0] * 200, "confidence_level": 0.85}
    assert client.post("/api/v1/backtest", json=bad_level).status_code == 422
    assert (
        client.post("/api/v1/backtest", json={"samples": [1.0] * 200, "origins": 9}).status_code
        == 422
    )


def test_backtest_capacity_and_failures(
    settings: service.Settings, monkeypatch: pytest.MonkeyPatch
) -> None:
    engine = service.ForecastEngine(settings)

    def explode(request: service.BacktestRequest) -> service.BacktestResponse:
        raise RuntimeError("boom")

    monkeypatch.setattr(engine, "backtest", explode)
    failing = TestClient(service.create_app(settings, engine), raise_server_exceptions=False)
    payload = {"samples": _periodic(200, 0.05), "horizon": 6, "origins": 1}
    assert failing.post("/api/v1/backtest", json=payload).status_code == 500

    def reject(request: service.BacktestRequest) -> service.BacktestResponse:
        raise service.HTTPException(status_code=422, detail="rejected")

    monkeypatch.setattr(engine, "backtest", reject)
    assert failing.post("/api/v1/backtest", json=payload).status_code == 422

    class Busy:
        def __init__(self, value: int) -> None:
            self.value = value

        def acquire(self, blocking: bool = True) -> bool:
            return False

        def release(self) -> None:
            raise AssertionError("a slot that was never acquired must not be released")

    monkeypatch.setattr(service.threading, "BoundedSemaphore", Busy)
    busy = TestClient(service.create_app(settings))
    response = busy.post("/api/v1/backtest", json=payload)
    assert response.status_code == 429
    assert response.headers["retry-after"] == "1"


def test_score_handles_zero_actuals_and_perfect_baselines() -> None:
    zeros = np.zeros(4)
    score = service._score(zeros, np.ones(4), zeros, np.ones(4) * 2, zeros)
    assert score.mape is None
    assert score.skill is None
    assert score.coverage == 1.0
    exact = np.array([1.0, 2.0, 3.0])
    perfect = service._score(exact, exact, exact - 0.5, exact + 0.5, exact + 1)
    assert perfect.mae == 0 and perfect.skill == 1.0 and perfect.smape == 0


def test_benchmark_is_reproducible_and_reports_its_method() -> None:
    first = benchmark.run(benchmark.Config(samples=240, horizon=12, origins=2, seed=11))
    second = benchmark.run(benchmark.Config(samples=240, horizon=12, origins=2, seed=11))
    assert first["results"] == second["results"]
    assert first["methodology"]["origins"] == 2
    names = {row["dataset"] for row in first["results"]}
    assert names == set(benchmark.ARCHETYPES)
    for row in first["results"]:
        assert row["samples"] == 24
        assert 0.0 <= row["coverage"] <= 1.0
        assert row["nominalCoverage"] == 0.9
    summary = first["summary"]
    assert summary["datasets"] == len(benchmark.ARCHETYPES)
    assert 0.0 <= summary["meanCoverage"] <= 1.0


def test_benchmark_cli_writes_json(tmp_path: Path) -> None:
    output = tmp_path / "report.json"
    assert (
        benchmark.main(
            ["--samples", "200", "--horizon", "6", "--origins", "1", "--output", str(output)]
        )
        == 0
    )
    assert output.read_text().startswith("{")
    assert benchmark.main(["--samples", "200", "--horizon", "6", "--origins", "1"]) == 0
