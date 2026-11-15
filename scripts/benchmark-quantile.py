#!/usr/bin/env python3
"""Compare quantile forecasters on measured usage in KubeRight's trace format.

Every method sees the same hourly series and is scored on the same rolling
origins as the forecasting service's own backtest (``ml.benchmark --traces``):
the last ``origins`` blocks of ``horizon`` hours, each forecast from only the
hours before it.

Methods:

* ``seasonal-naive``: the value one day earlier plus empirical quantiles of
  the day-over-day change in the history.
* ``kuberight``: the forecasting service (Ridge with calibrated intervals).
  Its point forecast is scored as P50 and the upper bound of its 90% interval
  as P95; it produces no other quantiles.
* ``lightgbm``: direct multi-horizon LightGBM quantile regression per series,
  one model per quantile, on lags, same-hour-yesterday and last-week values,
  rolling statistics, hour of day, and horizon.
* ``lightgbm-conformal``: the same model fitted without the last 48 hours of
  history, shifted by a split-conformal correction computed on those hours.

Scores, averaged per workload and then over workloads with 95% bootstrap
intervals: one-sided coverage (the share of actual values at or below each
predicted quantile; ideal = the quantile) and pinball loss scaled by the
series mean.

LightGBM is a benchmark-only dependency; it is not part of the service image.

Usage: benchmark-quantile.py TRACE.jsonl.gz [--limit 200] [--output report.json]
"""

from __future__ import annotations

import argparse
import gzip
import json
import math
import multiprocessing
import os
import sys
import tempfile
import time
from pathlib import Path
from typing import Any

# One BLAS thread per worker process; the pool supplies the parallelism.
os.environ.setdefault("OMP_NUM_THREADS", "1")
os.environ.setdefault("OPENBLAS_NUM_THREADS", "1")

import lightgbm as lgb  # noqa: E402
import numpy as np  # noqa: E402
from numpy.lib.stride_tricks import sliding_window_view  # noqa: E402

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
from ml import service  # noqa: E402

QUANTILES = (0.5, 0.9, 0.95, 0.99)
PERIOD = 24
CALIBRATION_HOURS = 48
PARAMETERS = {
    "objective": "quantile",
    "n_estimators": 150,
    "learning_rate": 0.05,
    "num_leaves": 15,
    "min_child_samples": 20,
    "subsample": 1.0,
    "colsample_bytree": 1.0,
    "deterministic": True,
    "force_row_wise": True,
    "num_threads": 1,
    "verbosity": -1,
    "random_state": 20261114,
}


def hourly(values: list[float | None], step_seconds: int) -> np.ndarray:
    per_hour = max(1, 3_600 // step_seconds)
    hours = []
    for start in range(0, len(values) - per_hour + 1, per_hour):
        present = [value for value in values[start : start + per_hour] if value is not None]
        if present:
            hours.append(float(np.mean(present)))
    return np.asarray(hours, dtype=float)


class Features:
    """Feature rows for direct forecasts: from index ``last``, ``step`` hours ahead."""

    def __init__(self, series: np.ndarray) -> None:
        self.series = series
        sums = np.concatenate([[0.0], np.cumsum(series)])
        squares = np.concatenate([[0.0], np.cumsum(series * series)])
        index = np.arange(len(series))

        def rolling_mean(width: int) -> tuple[np.ndarray, np.ndarray]:
            start = np.maximum(0, index - width + 1)
            count = index - start + 1
            return (sums[index + 1] - sums[start]) / count, (squares[index + 1] - squares[start]) / count

        self.mean24, mean_square24 = rolling_mean(24)
        self.std24 = np.sqrt(np.maximum(0.0, mean_square24 - self.mean24**2))
        self.mean168, _ = rolling_mean(168)
        padded = np.concatenate([np.full(23, -np.inf), series])
        self.max24 = sliding_window_view(padded, 24).max(axis=1)

    def matrix(self, lasts: np.ndarray, steps: np.ndarray) -> np.ndarray:
        series, targets = self.series, lasts + steps

        def lagged(positions: np.ndarray) -> np.ndarray:
            return np.where(positions >= 0, series[np.maximum(positions, 0)], np.nan)

        return np.column_stack([
            series[lasts], lagged(lasts - 1), lagged(lasts - 2),
            lagged(targets - PERIOD), lagged(targets - 7 * PERIOD),
            self.mean24[lasts], self.max24[lasts], self.std24[lasts], self.mean168[lasts],
            (targets % PERIOD).astype(float), steps.astype(float),
        ])

    def rows(self, lasts: range, horizon: int, end: int) -> tuple[np.ndarray, np.ndarray]:
        """Rows from each origin in ``lasts`` whose targets lie before ``end``."""
        grid_lasts, grid_steps = np.meshgrid(np.asarray(lasts), np.arange(1, horizon + 1), indexing="ij")
        keep = (grid_lasts + grid_steps) < end
        chosen_lasts, chosen_steps = grid_lasts[keep], grid_steps[keep]
        return self.matrix(chosen_lasts, chosen_steps), self.series[chosen_lasts + chosen_steps]


def fit(inputs: np.ndarray, targets: np.ndarray, quantile: float) -> lgb.LGBMRegressor:
    model = lgb.LGBMRegressor(alpha=quantile, **PARAMETERS)
    model.fit(inputs, targets)
    return model


def conformal_shift(scores: np.ndarray, quantile: float) -> float:
    """Finite-sample split-conformal correction for a one-sided quantile."""
    ordered = np.sort(scores)
    rank = min(len(ordered), math.ceil((len(ordered) + 1) * quantile))
    return float(ordered[rank - 1])


def lightgbm_forecasts(scaled: np.ndarray, cut: int, horizon: int) -> dict[str, dict[float, np.ndarray]]:
    table = Features(scaled)
    steps = np.arange(1, horizon + 1)
    test = table.matrix(np.full(horizon, cut - 1), steps)
    full_inputs, full_targets = table.rows(range(PERIOD, cut - 1), horizon, cut)
    calibration_end = cut - CALIBRATION_HOURS
    train_inputs, train_targets = table.rows(range(PERIOD, calibration_end - 1), horizon, calibration_end)
    # Forecasts made at every hour of the held-back window, as the test
    # forecast is made at the last hour; their targets are all unseen by the
    # calibration model.
    calibration_x, calibration_y = table.rows(range(calibration_end - 1, cut - 1), horizon, cut)
    raw: dict[float, np.ndarray] = {}
    calibrated: dict[float, np.ndarray] = {}
    for quantile in QUANTILES:
        raw[quantile] = fit(full_inputs, full_targets, quantile).predict(test)
        model = fit(train_inputs, train_targets, quantile)
        shift = conformal_shift(calibration_y - model.predict(calibration_x), quantile)
        calibrated[quantile] = model.predict(test) + shift
    return {"lightgbm": raw, "lightgbm-conformal": calibrated}


def seasonal_naive(series: np.ndarray, cut: int, horizon: int) -> dict[float, np.ndarray]:
    history = series[:cut]
    changes = history[PERIOD:] - history[:-PERIOD]
    base = np.asarray([history[cut - PERIOD + ((step - 1) % PERIOD)] for step in range(1, horizon + 1)])
    return {quantile: base + float(np.quantile(changes, quantile)) for quantile in QUANTILES}


def pinball(actual: np.ndarray, predicted: np.ndarray, quantile: float) -> float:
    error = actual - predicted
    return float(np.mean(np.maximum(quantile * error, (quantile - 1) * error)))


def bootstrap(values: list[float], random: np.random.Generator, resamples: int) -> list[float]:
    data = np.asarray(values, dtype=float)
    means = data[random.integers(0, len(data), size=(resamples, len(data)))].mean(axis=1)
    return [round(float(np.percentile(means, 2.5)), 4), round(float(np.percentile(means, 97.5)), 4)]


METHODS = ("seasonal-naive", "kuberight", "lightgbm", "lightgbm-conformal")
_ENGINE: service.ForecastEngine | None = None


def _start_worker(horizon: int) -> None:
    global _ENGINE
    model_dir = tempfile.mkdtemp(prefix="kuberight-benchmark-")
    _ENGINE = service.ForecastEngine(
        service.Settings(
            min_samples=48, max_samples=4_096, max_horizon=horizon, max_body_bytes=10_000_000,
            max_concurrent_forecasts=1, prediction_timeout_seconds=60, training_sample_limit=4_096,
            model_dir=Path(model_dir),
        )
    )


def evaluate(task: tuple[dict[str, Any], int, int]) -> dict[str, dict[tuple[str, float], tuple[float, float]] | None]:
    """Per resource: (method, quantile) -> (coverage, scaled pinball), or None when skipped."""
    workload, horizon, origins = task
    assert _ENGINE is not None
    results: dict[str, dict[tuple[str, float], tuple[float, float]] | None] = {}
    for resource, key in (("cpu", "cpuMilli"), ("memory", "memoryBytes")):
        series = hourly(workload[key], int(workload["stepSeconds"]))
        if len(series) < horizon * (origins + 1) + 48 + CALIBRATION_HOURS or series.max() <= 0:
            results[resource] = None
            continue
        report = _ENGINE.backtest(
            service.BacktestRequest(
                samples=series.tolist(), horizon=horizon, origins=origins, interval_seconds=3_600, confidence_level=0.9
            )
        )
        points = {point.index: point for point in report.points}
        pairs: dict[tuple[str, float], list[tuple[np.ndarray, np.ndarray]]] = {}
        scale = float(series.mean()) or 1.0
        scaled = series / scale
        for position in range(origins):
            cut = len(series) - (origins - position) * horizon
            actual = scaled[cut : cut + horizon]
            for quantile, predicted in seasonal_naive(scaled, cut, horizon).items():
                pairs.setdefault(("seasonal-naive", quantile), []).append((actual, predicted))
            for method, forecasts in lightgbm_forecasts(scaled, cut, horizon).items():
                for quantile, predicted in forecasts.items():
                    pairs.setdefault((method, quantile), []).append((actual, predicted))
            indices = range(cut, cut + horizon)
            if all(index in points for index in indices):
                median = np.asarray([points[index].predicted for index in indices]) / scale
                upper = np.asarray([points[index].upper for index in indices]) / scale
                pairs.setdefault(("kuberight", 0.5), []).append((actual, median))
                pairs.setdefault(("kuberight", 0.95), []).append((actual, upper))
        scores = {}
        for (method, quantile), collected in pairs.items():
            actual = np.concatenate([pair[0] for pair in collected])
            predicted = np.concatenate([pair[1] for pair in collected])
            scores[(method, quantile)] = (float(np.mean(actual <= predicted)), pinball(actual, predicted, quantile))
        results[resource] = scores
    return results


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("trace", type=Path)
    parser.add_argument("--limit", type=int, default=200)
    parser.add_argument("--horizon", type=int, default=24)
    parser.add_argument("--origins", type=int, default=3)
    parser.add_argument("--seed", type=int, default=20261101)
    parser.add_argument("--resamples", type=int, default=2_000)
    parser.add_argument("--output", type=Path)
    parser.add_argument("--workers", type=int, default=max(1, (multiprocessing.cpu_count() or 1)))
    arguments = parser.parse_args(argv)

    opener = gzip.open if arguments.trace.suffix == ".gz" else open
    with opener(arguments.trace, "rt", encoding="utf-8") as stream:
        workloads = [json.loads(line) for line in stream if line.strip()]
    random = np.random.default_rng(arguments.seed)
    if arguments.limit and len(workloads) > arguments.limit:
        chosen = sorted(random.choice(len(workloads), size=arguments.limit, replace=False))
        workloads = [workloads[index] for index in chosen]

    tasks = [(workload, arguments.horizon, arguments.origins) for workload in workloads]
    coverage: dict[str, dict[str, dict[float, list[float]]]] = {}
    loss: dict[str, dict[str, dict[float, list[float]]]] = {}
    skipped, started = 0, time.monotonic()
    with multiprocessing.Pool(arguments.workers, initializer=_start_worker, initargs=(arguments.horizon,)) as pool:
        for number, results in enumerate(pool.imap(evaluate, tasks), 1):
            for resource, scores in results.items():
                if scores is None:
                    skipped += 1
                    continue
                for (method, quantile), (covered, scaled_loss) in scores.items():
                    coverage.setdefault(resource, {}).setdefault(method, {}).setdefault(quantile, []).append(covered)
                    loss.setdefault(resource, {}).setdefault(method, {}).setdefault(quantile, []).append(scaled_loss)
            if number % 20 == 0:
                print(f"{number}/{len(workloads)} workloads, {time.monotonic() - started:.0f}s", file=sys.stderr)

    summary: dict[str, Any] = {}
    for resource, by_method in coverage.items():
        summary[resource] = {}
        for method, by_quantile in by_method.items():
            summary[resource][method] = {}
            for quantile, values in by_quantile.items():
                losses = loss[resource][method][quantile]
                baseline = loss[resource]["seasonal-naive"][quantile]
                entry: dict[str, Any] = {
                    "workloads": len(values),
                    "coverage": round(float(np.mean(values)), 4),
                    "coverageCI": bootstrap(values, random, arguments.resamples),
                    "scaledPinball": round(float(np.mean(losses)), 5),
                    "scaledPinballCI": bootstrap(losses, random, arguments.resamples),
                }
                if method != "seasonal-naive" and len(baseline) == len(losses):
                    wins = [1.0 if mine < theirs else 0.0 for mine, theirs in zip(losses, baseline, strict=True)]
                    entry["shareBeatingSeasonalNaive"] = round(float(np.mean(wins)), 4)
                    entry["shareBeatingSeasonalNaiveCI"] = bootstrap(wins, random, arguments.resamples)
                summary[resource][method][f"p{round(quantile * 100)}"] = entry
    result = {
        "methodology": {
            "trace": arguments.trace.name,
            "workloads": len(workloads),
            "skipped": skipped,
            "horizon": arguments.horizon,
            "origins": arguments.origins,
            "seed": arguments.seed,
            "calibrationHours": CALIBRATION_HOURS,
            "lightgbm": {key: value for key, value in PARAMETERS.items() if key != "verbosity"},
            "lightgbmVersion": lgb.__version__,
            "scoring": "per-workload one-sided coverage and pinball loss scaled by the series mean, "
            "then the mean over workloads with a 95% percentile bootstrap",
            "seconds": round(time.monotonic() - started),
        },
        "summary": summary,
    }
    text = json.dumps(result, indent=2)
    if arguments.output:
        arguments.output.write_text(text + "\n")
    print(text)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
