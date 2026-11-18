"""Reproducible forecast-accuracy benchmark for KubeRight's forecasting service.

Every dataset is generated from a fixed seed, so a run is repeatable bit for bit.
Each dataset is replayed with the service's rolling-origin backtest: forecasts are
made from past origins using only earlier samples and scored on the samples that
followed. Results are reported per dataset together with the method and sample
counts, and are compared with the seasonal-naive baseline (skill above zero means
the forecast beat it).

Run: python -m ml.benchmark --output report.json

Real traces: python -m ml.benchmark --traces trace.jsonl.gz --limit 300 scores
the same backtest on measured usage in KubeRight's trace format (see
scripts/convert-bitbrains-trace.py), aggregated to hourly means, with 95%
bootstrap intervals over workloads.
"""

from __future__ import annotations

import argparse
import gzip
import json
import math
import tempfile
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from ml import service

Generator = Callable[[np.random.Generator, int], np.ndarray]


def _daily(length: int, period: int = 24) -> np.ndarray:
    return np.sin(2 * math.pi * np.arange(length) / period)


def _stable(random: np.random.Generator, length: int) -> np.ndarray:
    return 0.5 + random.normal(0, 0.01, length)


def _bursty_api(random: np.random.Generator, length: int) -> np.ndarray:
    base = 0.4 + 0.15 * _daily(length)
    bursts = (random.random(length) < 0.05) * random.uniform(0.5, 1.5, length)
    return base + bursts + random.normal(0, 0.03, length)


def _memory_intensive(random: np.random.Generator, length: int) -> np.ndarray:
    return 6.0 + 0.2 * _daily(length) + random.normal(0, 0.05, length)


def _java_sawtooth(random: np.random.Generator, length: int) -> np.ndarray:
    # Heap fills over twelve steps and drops at each collection.
    return 1.0 + 0.8 * (np.arange(length) % 12) / 12 + random.normal(0, 0.02, length)


def _statefulset(random: np.random.Generator, length: int) -> np.ndarray:
    return 2.0 + 0.002 * np.arange(length) + random.normal(0, 0.03, length)


def _hpa_scaled(random: np.random.Generator, length: int) -> np.ndarray:
    # Per-pod usage stays near the HPA target as replicas absorb daily load.
    return 0.7 + 0.05 * _daily(length) + random.normal(0, 0.04, length)


def _keda_scaled(random: np.random.Generator, length: int) -> np.ndarray:
    queue = (np.arange(length) // 18) % 2
    return 0.2 + 0.8 * queue + random.normal(0, 0.03, length)


def _cronjob(random: np.random.Generator, length: int) -> np.ndarray:
    running = (np.arange(length) % 24) < 3
    return 0.05 + 1.5 * running + random.normal(0, 0.02, length)


def _sidecar(random: np.random.Generator, length: int) -> np.ndarray:
    return 0.08 + 0.01 * _daily(length) + random.normal(0, 0.004, length)


def _recent_rollout(random: np.random.Generator, length: int) -> np.ndarray:
    level = np.where(np.arange(length) >= int(length * 0.75), 1.6, 1.0)
    return level * (1.0 + 0.2 * _daily(length)) + random.normal(0, 0.03, length)


def _incomplete_metrics(random: np.random.Generator, length: int) -> np.ndarray:
    values = 1.0 + 0.3 * _daily(length) + random.normal(0, 0.03, length)
    # A tenth of the samples are missing and bridged linearly, as a dashboard
    # would; the scores show how much that blurs the forecast.
    missing = random.random(length) < 0.10
    missing[0] = missing[-1] = False
    known = np.flatnonzero(~missing)
    values[missing] = np.interp(np.flatnonzero(missing), known, values[known])
    return values


ARCHETYPES: dict[str, tuple[str, Generator]] = {
    "stable": ("Flat usage with little noise", _stable),
    "bursty-api": ("Daily cycle with random 5% bursts up to 4x", _bursty_api),
    "memory-intensive": ("High, nearly flat memory with a small daily swing", _memory_intensive),
    "java-sawtooth": ("Heap sawtooth from periodic garbage collection", _java_sawtooth),
    "statefulset": ("Slow steady growth, as a database fills", _statefulset),
    "hpa-scaled": ("Per-pod usage held near the HPA target", _hpa_scaled),
    "keda-scaled": ("Event-driven steps as a queue fills and drains", _keda_scaled),
    "cronjob": ("Three busy hours a day, idle otherwise", _cronjob),
    "sidecar": ("Small proxy sidecar with a faint daily cycle", _sidecar),
    "recent-rollout": ("A rollout lifts usage 60% three quarters through", _recent_rollout),
    "incomplete-metrics": ("Daily cycle with 10% of samples missing", _incomplete_metrics),
}


@dataclass(frozen=True)
class Config:
    samples: int = 336
    horizon: int = 24
    origins: int = 3
    confidence_level: float = 0.9
    seed: int = 20261101


def run(config: Config) -> dict[str, Any]:
    """Backtest every archetype and return a JSON-serializable report."""
    with tempfile.TemporaryDirectory() as model_dir:
        engine = service.ForecastEngine(
            service.Settings(
                min_samples=48,
                max_samples=max(config.samples, 48),
                max_horizon=max(config.horizon, 1),
                max_body_bytes=10_000_000,
                max_concurrent_forecasts=1,
                prediction_timeout_seconds=30,
                training_sample_limit=max(config.samples, 48),
                model_dir=Path(model_dir),
            )
        )
        rows: list[dict[str, Any]] = []
        for index, (name, (description, generate)) in enumerate(ARCHETYPES.items()):
            random = np.random.default_rng(config.seed + index)
            values = np.clip(generate(random, config.samples), 0.0, None)
            report = engine.backtest(
                service.BacktestRequest(
                    samples=[float(value) for value in values],
                    horizon=config.horizon,
                    origins=config.origins,
                    interval_seconds=3_600,
                    confidence_level=config.confidence_level,
                )
            )
            overall = report.overall
            rows.append(
                {
                    "dataset": name,
                    "description": description,
                    "samples": overall.samples,
                    "mae": overall.mae,
                    "smape": overall.smape,
                    "coverage": overall.coverage,
                    "nominalCoverage": config.confidence_level,
                    "meanIntervalWidth": overall.mean_interval_width,
                    "baselineMAE": overall.baseline_mae,
                    "skill": overall.skill,
                    "models": sorted({origin.model for origin in report.origins}),
                }
            )
    coverages = [row["coverage"] for row in rows]
    skills = [row["skill"] for row in rows if row["skill"] is not None]
    return {
        "methodology": {
            "method": "rolling-origin backtest; each forecast sees only samples before its origin",
            "historySamples": config.samples,
            "horizon": config.horizon,
            "origins": config.origins,
            "seed": config.seed,
            "baseline": "seasonal-naive (or last value without detected seasonality)",
            "coverage": "share of held-out samples inside the forecast interval",
        },
        "results": rows,
        "summary": {
            "datasets": len(rows),
            "scoredSamples": sum(row["samples"] for row in rows),
            "meanCoverage": round(float(np.mean(coverages)), 6),
            "datasetsMeetingNominalCoverage": sum(
                1 for row in rows if row["coverage"] >= row["nominalCoverage"]
            ),
            "datasetsBeatingBaseline": sum(1 for skill in skills if skill > 0),
        },
    }


def _hourly(values: list[float | None], step_seconds: int) -> list[float]:
    """Hourly means of a trace series; an hour with no samples is skipped."""
    per_hour = max(1, 3_600 // step_seconds)
    hours: list[float] = []
    for start in range(0, len(values) - per_hour + 1, per_hour):
        present = [value for value in values[start : start + per_hour] if value is not None]
        if present:
            hours.append(float(np.mean(present)))
    return hours


def _interval(values: list[float], random: np.random.Generator, resamples: int) -> list[float]:
    """Two-sided 95% percentile bootstrap interval of the mean."""
    data = np.asarray(values, dtype=float)
    means = data[random.integers(0, len(data), size=(resamples, len(data)))].mean(axis=1)
    return [round(float(np.percentile(means, 2.5)), 4), round(float(np.percentile(means, 97.5)), 4)]


def run_traces(path: Path, limit: int, config: Config, resamples: int = 2_000) -> dict[str, Any]:
    """Backtest hourly CPU and memory of measured workloads."""
    opener = gzip.open if path.suffix == ".gz" else open
    with opener(path, "rt", encoding="utf-8") as stream:
        workloads = [json.loads(line) for line in stream if line.strip()]
    random = np.random.default_rng(config.seed)
    if limit and len(workloads) > limit:
        chosen = sorted(random.choice(len(workloads), size=limit, replace=False))
        workloads = [workloads[index] for index in chosen]
    rows: dict[str, list[dict[str, float]]] = {"cpu": [], "memory": []}
    skipped = 0
    with tempfile.TemporaryDirectory() as model_dir:
        engine = service.ForecastEngine(
            service.Settings(
                min_samples=48,
                max_samples=4_096,
                max_horizon=max(config.horizon, 1),
                max_body_bytes=10_000_000,
                max_concurrent_forecasts=1,
                prediction_timeout_seconds=60,
                training_sample_limit=4_096,
                model_dir=Path(model_dir),
            )
        )
        for workload in workloads:
            for resource, key in (("cpu", "cpuMilli"), ("memory", "memoryBytes")):
                hours = _hourly(workload[key], int(workload["stepSeconds"]))
                needed = config.horizon * (config.origins + 1) + 48
                if len(hours) < needed or max(hours) <= 0:
                    skipped += 1
                    continue
                report = engine.backtest(
                    service.BacktestRequest(
                        samples=hours,
                        horizon=config.horizon,
                        origins=config.origins,
                        interval_seconds=3_600,
                        confidence_level=config.confidence_level,
                    )
                )
                overall = report.overall
                rows[resource].append(
                    {
                        "coverage": overall.coverage,
                        "skill": overall.skill if overall.skill is not None else math.nan,
                        "samples": overall.samples,
                    }
                )
    summaries = {}
    for resource, results in rows.items():
        coverages = [row["coverage"] for row in results]
        skills = [row["skill"] for row in results if not math.isnan(row["skill"])]
        beats = [1.0 if skill > 0 else 0.0 for skill in skills]
        summaries[resource] = {
            "workloads": len(results),
            "scoredSamples": int(sum(row["samples"] for row in results)),
            "meanCoverage": round(float(np.mean(coverages)), 4),
            "meanCoverageCI": _interval(coverages, random, resamples),
            "nominalCoverage": config.confidence_level,
            "shareBeatingBaseline": round(float(np.mean(beats)), 4),
            "shareBeatingBaselineCI": _interval(beats, random, resamples),
            "medianSkill": round(float(np.median(skills)), 4),
        }
    return {
        "methodology": {
            "method": (
                "rolling-origin backtest on hourly means of measured usage; "
                "each forecast sees only earlier samples"
            ),
            "trace": path.name,
            "workloads": len(workloads),
            "horizon": config.horizon,
            "origins": config.origins,
            "seed": config.seed,
            "baseline": "seasonal-naive (or last value without detected seasonality)",
            "confidence": f"95% percentile bootstrap over workloads, {resamples} resamples",
            "skipped": skipped,
        },
        "summary": summaries,
    }


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--samples", type=int, default=Config.samples)
    parser.add_argument("--horizon", type=int, default=Config.horizon)
    parser.add_argument("--origins", type=int, default=Config.origins)
    parser.add_argument("--seed", type=int, default=Config.seed)
    parser.add_argument("--output", type=Path)
    parser.add_argument(
        "--traces", type=Path, help="evaluate measured usage in KubeRight's trace format"
    )
    parser.add_argument(
        "--limit", type=int, default=0, help="sample this many workloads from the trace"
    )
    arguments = parser.parse_args(argv)
    config = Config(
        samples=arguments.samples,
        horizon=arguments.horizon,
        origins=arguments.origins,
        seed=arguments.seed,
    )
    report = (
        run_traces(arguments.traces, arguments.limit, config) if arguments.traces else run(config)
    )
    text = json.dumps(report, indent=2)
    if arguments.output:
        arguments.output.write_text(text + "\n")
    else:
        print(text)
    return 0


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
