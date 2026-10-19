"""KubeRight's experimental, bounded CPU and memory forecasting API.

The service is recommendation-only: it forecasts a numeric resource series and
reports uncertainty, but never writes to Kubernetes. Every model is compared
against a chronological seasonal-naive baseline before it may be selected.
"""

from __future__ import annotations

import hashlib
import hmac
import json
import logging
import math
import os
import re
import tempfile
import threading
import time
import uuid
from contextlib import suppress
from dataclasses import dataclass
from datetime import UTC, datetime
from pathlib import Path
from typing import Annotated, Any, Literal

import joblib
import numpy as np
from fastapi import Depends, FastAPI, Header, HTTPException, Request
from fastapi import Path as APIPath
from fastapi.exceptions import RequestValidationError
from fastapi.responses import JSONResponse, Response
from prometheus_client import CollectorRegistry, Counter, Gauge, Histogram, Info, generate_latest
from prometheus_client.exposition import CONTENT_TYPE_LATEST
from pydantic import BaseModel, ConfigDict, Field, field_validator, model_validator
from sklearn.linear_model import Ridge
from sklearn.pipeline import Pipeline, make_pipeline
from sklearn.preprocessing import StandardScaler
from starlette.types import ASGIApp, Message, Receive, Scope, Send

SERVICE_VERSION = "0.1.0-rc.1"
MODEL_SCHEMA_VERSION = "ridge-features-v1"
MIN_API_SAMPLES = 48
MAX_API_SAMPLES = 10_080
MAX_API_HORIZON = 168
MAX_SAMPLE_VALUE = 1.0e18
SERIES_ID_PATTERN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$")
REQUEST_ID_PATTERN = re.compile(r"^[A-Za-z0-9._-]{1,64}$")

logger = logging.getLogger("kuberight.ml")
if not logger.handlers:
    logging.basicConfig(level=os.getenv("KUBERIGHT_ML_LOG_LEVEL", "INFO").upper())


def _env_int(name: str, default: int, minimum: int, maximum: int) -> int:
    raw = os.getenv(name)
    try:
        value = default if raw is None else int(raw)
    except ValueError as exc:
        raise RuntimeError(f"{name} must be an integer") from exc
    if not minimum <= value <= maximum:
        raise RuntimeError(f"{name} must be between {minimum} and {maximum}")
    return value


def _env_float(name: str, default: float, minimum: float, maximum: float) -> float:
    raw = os.getenv(name)
    try:
        value = default if raw is None else float(raw)
    except ValueError as exc:
        raise RuntimeError(f"{name} must be numeric") from exc
    if not math.isfinite(value) or not minimum <= value <= maximum:
        raise RuntimeError(f"{name} must be between {minimum} and {maximum}")
    return value


@dataclass(frozen=True)
class Settings:
    """Validated process configuration loaded once at startup."""

    min_samples: int = 168
    max_samples: int = MAX_API_SAMPLES
    max_horizon: int = MAX_API_HORIZON
    max_body_bytes: int = 1_048_576
    max_concurrent_forecasts: int = 2
    # Cooperative checkpoints bound forecast phases; this does not preempt an
    # in-progress NumPy or scikit-learn call or replace an HTTP wall-clock timeout.
    prediction_timeout_seconds: float = 8.0
    training_sample_limit: int = 4_096
    min_model_improvement: float = 0.02
    min_confidence: float = 0.45
    max_model_versions: int = 5
    model_dir: Path = Path(tempfile.gettempdir()) / "kuberight-ml" / "models"
    api_key: str | None = None

    def __post_init__(self) -> None:
        if not MIN_API_SAMPLES <= self.min_samples <= self.max_samples <= MAX_API_SAMPLES:
            raise ValueError("invalid sample bounds")
        if not 1 <= self.max_horizon <= MAX_API_HORIZON:
            raise ValueError("invalid horizon bound")
        if self.max_body_bytes < 1_024:
            raise ValueError("max_body_bytes must be at least 1024")
        if self.max_concurrent_forecasts < 1:
            raise ValueError("max_concurrent_forecasts must be positive")
        if not 0.05 <= self.prediction_timeout_seconds <= 60:
            raise ValueError("prediction_timeout_seconds must be between 0.05 and 60")
        if not self.min_samples <= self.training_sample_limit <= self.max_samples:
            raise ValueError("training_sample_limit must be within sample bounds")
        if not 0 <= self.min_model_improvement <= 1:
            raise ValueError("min_model_improvement must be between zero and one")
        if not 0 <= self.min_confidence <= 1:
            raise ValueError("min_confidence must be between zero and one")
        if self.max_model_versions < 1:
            raise ValueError("max_model_versions must be positive")
        if self.api_key is not None and len(self.api_key) < 16:
            raise ValueError("KUBERIGHT_ML_API_KEY must contain at least 16 characters")

    @classmethod
    def from_env(cls) -> Settings:
        api_key = os.getenv("KUBERIGHT_ML_API_KEY") or None
        model_dir = Path(
            os.getenv(
                "KUBERIGHT_ML_MODEL_DIR",
                str(Path(tempfile.gettempdir()) / "kuberight-ml" / "models"),
            )
        )
        min_samples = _env_int("KUBERIGHT_ML_MIN_SAMPLES", 168, MIN_API_SAMPLES, MAX_API_SAMPLES)
        max_samples = _env_int(
            "KUBERIGHT_ML_MAX_SAMPLES", MAX_API_SAMPLES, min_samples, MAX_API_SAMPLES
        )
        return cls(
            min_samples=min_samples,
            max_samples=max_samples,
            max_horizon=_env_int("KUBERIGHT_ML_MAX_HORIZON", MAX_API_HORIZON, 1, MAX_API_HORIZON),
            max_body_bytes=_env_int("KUBERIGHT_ML_MAX_BODY_BYTES", 1_048_576, 1_024, 16_777_216),
            max_concurrent_forecasts=_env_int("KUBERIGHT_ML_MAX_CONCURRENT", 2, 1, 32),
            prediction_timeout_seconds=_env_float("KUBERIGHT_ML_TIMEOUT_SECONDS", 8.0, 0.05, 60.0),
            training_sample_limit=_env_int(
                "KUBERIGHT_ML_TRAINING_SAMPLE_LIMIT",
                min(4_096, max_samples),
                min_samples,
                max_samples,
            ),
            min_model_improvement=_env_float("KUBERIGHT_ML_MIN_IMPROVEMENT", 0.02, 0.0, 1.0),
            min_confidence=_env_float("KUBERIGHT_ML_MIN_CONFIDENCE", 0.45, 0.0, 1.0),
            max_model_versions=_env_int("KUBERIGHT_ML_MAX_MODEL_VERSIONS", 5, 1, 100),
            model_dir=model_dir,
            api_key=api_key,
        )


FiniteSample = Annotated[float, Field(ge=0.0, le=MAX_SAMPLE_VALUE, allow_inf_nan=False)]


class ForecastRequest(BaseModel):
    """A uniformly sampled resource history and requested forecast horizon."""

    model_config = ConfigDict(extra="forbid", strict=True)

    samples: list[FiniteSample] = Field(
        min_length=MIN_API_SAMPLES,
        max_length=MAX_API_SAMPLES,
        description="Finite, non-negative CPU cores or memory bytes, oldest first.",
    )
    horizon: int = Field(default=24, ge=1, le=MAX_API_HORIZON)
    interval_seconds: int = Field(default=3_600, ge=1, le=86_400)
    resource: Literal["cpu", "memory"] = "cpu"
    timestamps: list[datetime] | None = Field(
        default=None, min_length=MIN_API_SAMPLES, max_length=MAX_API_SAMPLES
    )
    seasonal_period: int | None = Field(default=None, ge=2, le=MAX_API_HORIZON)
    confidence_level: float = Field(default=0.9, ge=0.8, le=0.95)
    series_id: str | None = Field(default=None, min_length=1, max_length=128)
    persist: bool = False

    @field_validator("timestamps", mode="before")
    @classmethod
    def parse_iso_timestamps(cls, value: Any) -> Any:
        """Accept RFC 3339 JSON strings without weakening strict numeric fields."""
        if value is None or not isinstance(value, list):
            return value
        parsed: list[Any] = []
        for item in value:
            if isinstance(item, str):
                try:
                    parsed.append(datetime.fromisoformat(item.replace("Z", "+00:00")))
                except ValueError as exc:
                    raise ValueError("timestamps must contain valid RFC 3339 datetimes") from exc
            else:
                parsed.append(item)
        return parsed

    @model_validator(mode="after")
    def validate_series(self) -> ForecastRequest:
        if self.timestamps is not None:
            if len(self.timestamps) != len(self.samples):
                raise ValueError("timestamps and samples must have the same length")
            previous: datetime | None = None
            for stamp in self.timestamps:
                if stamp.tzinfo is None or stamp.utcoffset() is None:
                    raise ValueError("timestamps must include a UTC offset")
                if previous is not None and stamp <= previous:
                    raise ValueError("timestamps must be strictly increasing")
                previous = stamp
        if self.series_id is not None and SERIES_ID_PATTERN.fullmatch(self.series_id) is None:
            raise ValueError("series_id contains unsupported characters")
        if self.persist and self.series_id is None:
            raise ValueError("series_id is required when persist is true")
        if self.confidence_level not in (0.8, 0.9, 0.95):
            raise ValueError("confidence_level must be one of 0.8, 0.9, or 0.95")
        if self.seasonal_period is not None and self.seasonal_period * 3 > len(self.samples):
            raise ValueError("at least three complete requested seasons are required")
        return self


class ConfidenceInterval(BaseModel):
    model_config = ConfigDict(extra="forbid")

    lower: float
    upper: float


class ValidationSummary(BaseModel):
    model_config = ConfigDict(extra="forbid", populate_by_name=True)

    strategy: str
    folds: int
    validation_samples: int = Field(serialization_alias="validationSamples")
    training_end_indices: list[int] = Field(serialization_alias="trainingEndIndices")
    model_mae: float | None = Field(serialization_alias="modelMAE")
    baseline_mae: float | None = Field(serialization_alias="baselineMAE")
    improvement_ratio: float | None = Field(serialization_alias="improvementRatio")


class SeasonalitySummary(BaseModel):
    model_config = ConfigDict(extra="forbid")

    detected: bool
    period: int | None
    strength: float
    method: str = "detrended-autocorrelation"


class AnomalySummary(BaseModel):
    model_config = ConfigDict(extra="forbid", populate_by_name=True)

    count: int
    rate: float
    recent_indices: list[int] = Field(serialization_alias="recentIndices")
    method: str = "causal-seasonal-residual-mad"


class DriftSummary(BaseModel):
    model_config = ConfigDict(extra="forbid")

    detected: bool
    score: float
    method: str = "robust-window-distribution-shift"


class DataQualitySummary(BaseModel):
    model_config = ConfigDict(extra="forbid", populate_by_name=True)

    sample_count: int = Field(serialization_alias="sampleCount")
    missing_samples: int = Field(serialization_alias="missingSamples")
    duplicate_timestamps: int = Field(serialization_alias="duplicateTimestamps")
    irregular_intervals: int = Field(serialization_alias="irregularIntervals")
    stale: bool
    truncated_for_training: bool = Field(serialization_alias="truncatedForTraining")
    quality_score: float = Field(serialization_alias="qualityScore")


class RetrainingSummary(BaseModel):
    model_config = ConfigDict(extra="forbid", populate_by_name=True)

    recommended: bool
    reasons: list[str]
    policy: str
    minimum_new_samples: int = Field(serialization_alias="minimumNewSamples")


class ModelMetadata(BaseModel):
    model_config = ConfigDict(extra="forbid", populate_by_name=True)

    name: str
    version: str
    feature_schema: str = Field(serialization_alias="featureSchema")
    trained_at: datetime = Field(serialization_alias="trainedAt")
    training_samples: int = Field(serialization_alias="trainingSamples")
    persisted: bool


class ForecastResponse(BaseModel):
    model_config = ConfigDict(extra="forbid", populate_by_name=True)

    predictions: list[float]
    confidence_intervals: list[ConfidenceInterval] = Field(
        serialization_alias="confidenceIntervals"
    )
    validation_mae: float | None = Field(serialization_alias="validationMAE")
    baseline_validation_mae: float | None = Field(serialization_alias="baselineValidationMAE")
    model: str
    model_version: str = Field(serialization_alias="modelVersion")
    fallback_used: bool = Field(serialization_alias="fallbackUsed")
    fallback_reasons: list[str] = Field(serialization_alias="fallbackReasons")
    confidence: float
    validation: ValidationSummary
    seasonality: SeasonalitySummary
    anomalies: AnomalySummary
    drift: DriftSummary
    data_quality: DataQualitySummary = Field(serialization_alias="dataQuality")
    retraining: RetrainingSummary
    model_metadata: ModelMetadata = Field(serialization_alias="modelMetadata")
    generated_at: datetime = Field(serialization_alias="generatedAt")
    experimental: bool = True
    warning: str


@dataclass
class _ValidationResult:
    folds: int
    validation_samples: int
    training_end_indices: list[int]
    model_mae: float | None
    baseline_mae: float | None
    improvement_ratio: float | None
    model_residuals: list[float]
    baseline_residuals: list[float]


class _DeadlineExceeded(RuntimeError):
    pass


class _Deadline:
    """Cooperative compute budget checked between forecast phases.

    This deliberately does not claim hard cancellation: native numerical work
    already in progress runs until that call returns. Deployments that need a
    wall-clock request limit must enforce one at the server or proxy boundary.
    """

    def __init__(self, seconds: float) -> None:
        self._expires = time.monotonic() + seconds

    def check(self) -> None:
        if time.monotonic() >= self._expires:
            raise _DeadlineExceeded("forecast execution exceeded its cooperative compute budget")


def _model_input_digest(
    request: ForecastRequest,
    settings: Settings,
    effective_period: int | None,
    model_name: str,
) -> str:
    """Hash the complete normalized forecast input and model-affecting configuration."""
    request_payload = request.model_dump(mode="json")
    if request.timestamps is not None:
        request_payload["timestamps"] = [
            stamp.astimezone(UTC).isoformat(timespec="microseconds") for stamp in request.timestamps
        ]
    canonical = {
        "serviceVersion": SERVICE_VERSION,
        "featureSchema": MODEL_SCHEMA_VERSION,
        "request": request_payload,
        "effectivePeriod": effective_period,
        "modelName": model_name,
        "modelConfig": {
            "minSamples": settings.min_samples,
            "maxSamples": settings.max_samples,
            "maxHorizon": settings.max_horizon,
            "predictionComputeBudgetSeconds": settings.prediction_timeout_seconds,
            "trainingSampleLimit": settings.training_sample_limit,
            "minModelImprovement": settings.min_model_improvement,
            "minConfidence": settings.min_confidence,
        },
    }
    encoded = json.dumps(
        canonical,
        allow_nan=False,
        ensure_ascii=True,
        separators=(",", ":"),
        sort_keys=True,
    ).encode("utf-8")
    return hashlib.sha256(encoded).hexdigest()


def _new_model_version(input_digest: str, now: datetime) -> str:
    """Return a traceable, per-execution version safe for artifact filenames."""
    timestamp = now.astimezone(UTC).strftime("%Y%m%dT%H%M%S%fZ")
    return f"{MODEL_SCHEMA_VERSION}-{input_digest[:20]}-{timestamp}-{uuid.uuid4().hex[:16]}"


class ModelStore:
    """Versioned, atomic on-disk persistence for trusted locally trained artifacts."""

    def __init__(self, root: Path, max_versions: int) -> None:
        self.root = root.resolve()
        self.max_versions = max_versions
        self._lock = threading.Lock()

    @staticmethod
    def _series_key(series_id: str) -> str:
        return hashlib.sha256(series_id.encode("utf-8")).hexdigest()

    def readiness(self) -> tuple[bool, str]:
        try:
            self.root.mkdir(mode=0o700, parents=True, exist_ok=True)
            handle, probe = tempfile.mkstemp(prefix=".ready-", dir=self.root)
            os.close(handle)
            Path(probe).unlink(missing_ok=True)
            return True, "writable"
        except OSError:
            logger.exception("model store readiness check failed")
            return False, "unavailable"

    @staticmethod
    def _atomic_json(path: Path, payload: dict[str, Any]) -> None:
        handle, temporary = tempfile.mkstemp(prefix=".metadata-", dir=path.parent)
        try:
            with os.fdopen(handle, "w", encoding="utf-8") as stream:
                json.dump(payload, stream, separators=(",", ":"), sort_keys=True)
                stream.flush()
                os.fsync(stream.fileno())
            os.replace(temporary, path)
            with suppress(OSError):
                path.chmod(0o600)
        finally:
            Path(temporary).unlink(missing_ok=True)

    def save(
        self,
        series_id: str,
        version: str,
        artifact: dict[str, Any],
        metadata: dict[str, Any],
    ) -> None:
        series_dir = self.root / self._series_key(series_id)
        with self._lock:
            series_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
            artifact_path = series_dir / f"{version}.joblib"
            metadata_path = series_dir / f"{version}.json"
            if artifact_path.exists() or metadata_path.exists():
                raise FileExistsError(f"model version already exists: {version}")
            handle, temporary = tempfile.mkstemp(prefix=".model-", dir=series_dir)
            os.close(handle)
            try:
                joblib.dump(artifact, temporary, compress=3)
                os.replace(temporary, artifact_path)
                with suppress(OSError):
                    artifact_path.chmod(0o600)
                self._atomic_json(metadata_path, metadata)
                self._atomic_json(
                    series_dir / "latest.json",
                    {"metadata": metadata_path.name, "version": version},
                )
            finally:
                Path(temporary).unlink(missing_ok=True)
            versions = sorted(
                (path for path in series_dir.glob("*.json") if path.name != "latest.json"),
                key=lambda path: path.stat().st_mtime_ns,
                reverse=True,
            )
            for obsolete in versions[self.max_versions :]:
                obsolete.unlink(missing_ok=True)
                obsolete.with_suffix(".joblib").unlink(missing_ok=True)

    def latest_metadata(self, series_id: str) -> dict[str, Any] | None:
        series_dir = self.root / self._series_key(series_id)
        pointer_path = series_dir / "latest.json"
        try:
            pointer = json.loads(pointer_path.read_text(encoding="utf-8"))
            filename = pointer["metadata"]
            if not isinstance(filename, str) or Path(filename).name != filename:
                raise ValueError("invalid metadata pointer")
            metadata_path = series_dir / filename
            payload = json.loads(metadata_path.read_text(encoding="utf-8"))
            if payload.get("seriesId") != series_id:
                raise ValueError("model metadata identity mismatch")
            return payload
        except FileNotFoundError:
            return None
        except (OSError, ValueError, KeyError, json.JSONDecodeError):
            logger.exception(
                "failed to read model metadata", extra={"series_key": self._series_key(series_id)}
            )
            return None


def _detect_seasonality(
    values: np.ndarray, interval_seconds: int, requested_period: int | None
) -> SeasonalitySummary:
    if requested_period is not None:
        period_candidates = [requested_period]
    else:
        max_lag = min(MAX_API_HORIZON, len(values) // 4)
        if max_lag < 2:
            return SeasonalitySummary(detected=False, period=None, strength=0.0)
        period_candidates = list(range(2, max_lag + 1))
        for seconds in (3_600, 86_400, 604_800):
            period = round(seconds / interval_seconds)
            if 2 <= period <= max_lag:
                period_candidates.append(period)
        period_candidates = sorted(set(period_candidates))

    indices = np.arange(len(values), dtype=float)
    slope, intercept = np.polyfit(indices, values, 1)
    detrended = values - (slope * indices + intercept)
    if float(np.std(detrended)) <= np.finfo(float).eps:
        return SeasonalitySummary(detected=False, period=None, strength=0.0)

    best_period: int | None = None
    best_strength = -1.0
    for period in period_candidates:
        if period >= len(detrended):
            continue
        left = detrended[:-period]
        right = detrended[period:]
        if (
            float(np.std(left)) <= np.finfo(float).eps
            or float(np.std(right)) <= np.finfo(float).eps
        ):
            strength = 0.0
        else:
            strength = float(np.corrcoef(left, right)[0, 1])
        if math.isfinite(strength) and strength > best_strength:
            best_strength = strength
            best_period = period
    strength = max(0.0, min(1.0, best_strength))
    detected = best_period is not None and strength >= 0.30
    return SeasonalitySummary(
        detected=detected,
        period=best_period if detected or requested_period is not None else None,
        strength=round(strength, 6),
    )


def _feature_row(history: list[float], index: int, period: int | None) -> list[float]:
    recent = np.asarray(history[-24:], dtype=float)
    short = recent[-6:]
    trend_window = recent[-12:]
    trend = (
        float(np.polyfit(np.arange(len(trend_window)), trend_window, 1)[0])
        if len(trend_window) >= 2
        else 0.0
    )
    seasonal = history[-period] if period is not None and len(history) >= period else history[-1]
    if period is None:
        phase_sin = phase_cos = 0.0
    else:
        phase = 2.0 * math.pi * (index % period) / period
        phase_sin, phase_cos = math.sin(phase), math.cos(phase)
    return [
        history[-1],
        history[-2] if len(history) > 1 else history[-1],
        history[-3] if len(history) > 2 else history[-1],
        float(np.mean(short)),
        float(np.std(short)),
        float(np.mean(recent)),
        float(np.max(recent)),
        float(np.min(recent)),
        trend,
        float(seasonal),
        phase_sin,
        phase_cos,
    ]


def _training_matrix(values: np.ndarray, period: int | None) -> tuple[np.ndarray, np.ndarray]:
    start = max(24, period or 0)
    rows = [
        _feature_row(values[:index].tolist(), index, period) for index in range(start, len(values))
    ]
    return np.asarray(rows, dtype=float), values[start:].astype(float, copy=False)


def _fit_model(values: np.ndarray, period: int | None) -> Pipeline:
    features, targets = _training_matrix(values, period)
    if len(targets) < 32:
        raise ValueError("insufficient feature rows for ML training")
    # Scaling makes one deterministic model safe for both CPU cores and memory bytes.
    model = make_pipeline(StandardScaler(), Ridge(alpha=1.0, solver="cholesky"))
    model.fit(features, targets)
    return model


def _recursive_model_forecast(
    model: Pipeline,
    history_values: np.ndarray,
    horizon: int,
    period: int | None,
) -> list[float]:
    history = history_values.astype(float).tolist()
    output: list[float] = []
    for _ in range(horizon):
        features = np.asarray([_feature_row(history, len(history), period)], dtype=float)
        prediction = max(0.0, float(model.predict(features)[0]))
        if not math.isfinite(prediction):
            raise ValueError("model generated a non-finite prediction")
        output.append(prediction)
        history.append(prediction)
    return output


def _baseline_forecast(history_values: np.ndarray, horizon: int, period: int | None) -> list[float]:
    history = history_values.astype(float).tolist()
    output: list[float] = []
    for _ in range(horizon):
        prediction = (
            history[-period] if period is not None and len(history) >= period else history[-1]
        )
        prediction = max(0.0, float(prediction))
        output.append(prediction)
        history.append(prediction)
    return output


def _safe_baseline_forecast(
    history_values: np.ndarray, horizon: int, period: int | None
) -> list[float]:
    baseline = _baseline_forecast(history_values, horizon, period)
    window_size = min(len(history_values), max(24, (period or 24) * 2))
    recent_high = float(np.quantile(history_values[-window_size:], 0.95))
    return [max(value, recent_high) for value in baseline]


def _validation_plan(length: int, horizon: int, period: int | None) -> tuple[list[int], int]:
    desired_fold_size = max(8, min(24, horizon))
    minimum_training = max(MIN_API_SAMPLES, 2 * (period or 24) + 8)
    available_validation = length - minimum_training
    if available_validation < 8:
        return [], desired_fold_size
    fold_size = min(desired_fold_size, available_validation)
    possible_folds = available_validation // fold_size
    folds = min(3, possible_folds)
    first_cutoff = length - folds * fold_size
    return [first_cutoff + index * fold_size for index in range(folds)], fold_size


def _chronological_validation(
    values: np.ndarray,
    period: int | None,
    horizon: int,
    deadline: _Deadline,
) -> _ValidationResult:
    cutoffs, fold_size = _validation_plan(len(values), horizon, period)
    model_residuals: list[float] = []
    baseline_residuals: list[float] = []
    completed_cutoffs: list[int] = []
    model_failed = False
    for cutoff in cutoffs:
        deadline.check()
        validation_end = min(len(values), cutoff + fold_size)
        actual = values[cutoff:validation_end]
        baseline = _baseline_forecast(values[:cutoff], len(actual), period)
        baseline_residuals.extend((actual - np.asarray(baseline)).tolist())
        try:
            model = _fit_model(values[:cutoff], period)
            deadline.check()
            predicted = _recursive_model_forecast(model, values[:cutoff], len(actual), period)
            model_residuals.extend((actual - np.asarray(predicted)).tolist())
        except (ValueError, FloatingPointError):
            logger.exception("ML validation fold failed")
            model_failed = True
            model_residuals.clear()
            completed_cutoffs.append(cutoff)
            break
        completed_cutoffs.append(cutoff)
    baseline_mae = float(np.mean(np.abs(baseline_residuals))) if baseline_residuals else None
    if model_failed or not model_residuals:
        model_mae = None
        improvement = None
    else:
        model_mae = float(np.mean(np.abs(model_residuals)))
        if baseline_mae is None:
            raise RuntimeError("model validation residuals exist without baseline residuals")
        denominator = max(baseline_mae, np.finfo(float).eps)
        improvement = float((baseline_mae - model_mae) / denominator)
    return _ValidationResult(
        folds=len(completed_cutoffs),
        validation_samples=len(baseline_residuals),
        training_end_indices=completed_cutoffs,
        model_mae=model_mae,
        baseline_mae=baseline_mae,
        improvement_ratio=improvement,
        model_residuals=model_residuals,
        baseline_residuals=baseline_residuals,
    )


def _anomalies(values: np.ndarray, period: int | None) -> AnomalySummary:
    lag = period or min(24, max(2, len(values) // 8))
    residuals = values[lag:] - values[:-lag]
    if not len(residuals):
        return AnomalySummary(count=0, rate=0.0, recent_indices=[])
    center = float(np.median(residuals))
    deviations = np.abs(residuals - center)
    mad = float(np.median(deviations))
    scale = max(1.4826 * mad, 1e-9 * max(1.0, float(np.max(values))))
    indexes = (np.flatnonzero(deviations / scale > 3.5) + lag).tolist()
    return AnomalySummary(
        count=len(indexes), rate=round(len(indexes) / len(values), 6), recent_indices=indexes[-20:]
    )


def _drift(values: np.ndarray, period: int | None) -> DriftSummary:
    window = min(len(values) // 3, max(24, 2 * (period or 12)))
    if window < 12:
        return DriftSummary(detected=False, score=0.0)
    earlier = values[-2 * window : -window]
    recent = values[-window:]
    pooled = np.concatenate((earlier, recent))
    robust_scale = max(
        float(np.quantile(pooled, 0.75) - np.quantile(pooled, 0.25)),
        abs(float(np.median(pooled))) * 0.05,
        np.finfo(float).eps,
    )
    location_shift = abs(float(np.median(recent) - np.median(earlier))) / robust_scale
    quantile_shift = float(
        np.mean(
            np.abs(np.quantile(recent, [0.1, 0.5, 0.9]) - np.quantile(earlier, [0.1, 0.5, 0.9]))
        )
        / robust_scale
    )
    score = min(1.0, (location_shift + quantile_shift) / 4.0)
    return DriftSummary(detected=score >= 0.50, score=round(score, 6))


def _data_quality(request: ForecastRequest, truncated: bool) -> DataQualitySummary:
    irregular = 0
    stale = False
    if request.timestamps:
        seconds = np.asarray(
            [
                (request.timestamps[index] - request.timestamps[index - 1]).total_seconds()
                for index in range(1, len(request.timestamps))
            ]
        )
        irregular = int(
            np.count_nonzero(
                np.abs(seconds - request.interval_seconds) > request.interval_seconds * 0.20
            )
        )
        latest = request.timestamps[-1].astimezone(UTC)
        stale_after = max(86_400, request.interval_seconds * 3)
        stale = (datetime.now(UTC) - latest).total_seconds() > stale_after
    irregular_rate = irregular / max(1, len(request.samples) - 1)
    score = max(0.0, 1.0 - min(0.5, irregular_rate * 2.0) - (0.2 if stale else 0.0))
    return DataQualitySummary(
        sample_count=len(request.samples),
        missing_samples=0,
        duplicate_timestamps=0,
        irregular_intervals=irregular,
        stale=stale,
        truncated_for_training=truncated,
        quality_score=round(score, 6),
    )


def _confidence_intervals(
    predictions: list[float], residuals: list[float], level: float
) -> list[ConfidenceInterval]:
    if len(residuals) >= 8:
        alpha = (1.0 - level) / 2.0
        low_offset, high_offset = np.quantile(residuals, [alpha, 1.0 - alpha]).tolist()
        low_offset = min(0.0, float(low_offset))
        high_offset = max(0.0, float(high_offset))
    else:
        scale = float(np.std(residuals)) if residuals else 0.0
        low_offset, high_offset = -1.645 * scale, 1.645 * scale
    intervals: list[ConfidenceInterval] = []
    for index, prediction in enumerate(predictions):
        widening = math.sqrt(1.0 + index / max(1, len(predictions)))
        intervals.append(
            ConfidenceInterval(
                lower=max(0.0, prediction + low_offset * widening),
                upper=max(prediction, prediction + high_offset * widening),
            )
        )
    return intervals


def _unvalidated_uncertainty_bounds(
    predictions: list[float], values: np.ndarray, level: float
) -> list[ConfidenceInterval]:
    """Return deliberately conservative heuristic bounds when validation is unavailable.

    These are not empirically calibrated confidence intervals. A non-zero relative
    margin prevents a constant history from being presented as certain merely
    because no holdout residuals exist.
    """
    reference = max(
        max(predictions, default=0.0),
        float(np.quantile(values, 0.95)),
        np.finfo(float).eps,
    )
    observed_spread = max(
        float(np.ptp(values)),
        1.4826 * float(np.median(np.abs(values - np.median(values)))),
    )
    level_multiplier = {0.8: 1.0, 0.9: 1.5, 0.95: 2.0}[level]
    base_width = max(observed_spread, reference * 0.25, np.finfo(float).eps)
    intervals: list[ConfidenceInterval] = []
    for index, prediction in enumerate(predictions):
        widening = math.sqrt(1.0 + index / max(1, len(predictions)))
        width = base_width * level_multiplier * widening
        intervals.append(
            ConfidenceInterval(
                lower=max(0.0, prediction - width),
                upper=prediction + width,
            )
        )
    return intervals


def _forecast_confidence(
    validation_mae: float,
    values: np.ndarray,
    validation_samples: int,
    quality: DataQualitySummary,
    anomalies: AnomalySummary,
    drift: DriftSummary,
) -> float:
    scale = max(float(np.median(values)), float(np.mean(values)), np.finfo(float).eps)
    confidence = math.exp(-validation_mae / scale)
    confidence *= min(1.0, validation_samples / 48.0)
    confidence *= quality.quality_score
    confidence *= max(0.5, 1.0 - anomalies.rate)
    if drift.detected:
        confidence *= 0.75
    return max(0.0, min(1.0, confidence))


class ForecastEngine:
    def __init__(self, settings: Settings, store: ModelStore | None = None) -> None:
        self.settings = settings
        self.store = store or ModelStore(settings.model_dir, settings.max_model_versions)

    def forecast(self, request: ForecastRequest) -> ForecastResponse:
        deadline = _Deadline(self.settings.prediction_timeout_seconds)
        if len(request.samples) < self.settings.min_samples:
            raise HTTPException(
                status_code=422, detail=f"at least {self.settings.min_samples} samples are required"
            )
        if len(request.samples) > self.settings.max_samples:
            raise HTTPException(
                status_code=422, detail=f"at most {self.settings.max_samples} samples are accepted"
            )
        if request.horizon > self.settings.max_horizon:
            raise HTTPException(
                status_code=422, detail=f"horizon must not exceed {self.settings.max_horizon}"
            )

        all_values = np.asarray(request.samples, dtype=float)
        truncated = len(all_values) > self.settings.training_sample_limit
        values = all_values[-self.settings.training_sample_limit :]
        cutoffs, _ = _validation_plan(len(values), request.horizon, request.seasonal_period)
        seasonality_values = values[: cutoffs[0]] if cutoffs else values
        seasonality = _detect_seasonality(
            seasonality_values, request.interval_seconds, request.seasonal_period
        )
        period = seasonality.period
        quality = _data_quality(request, truncated)
        anomalies = _anomalies(all_values, period)
        drift = _drift(all_values, period)
        fallback_reasons: list[str] = []

        try:
            validation = _chronological_validation(values, period, request.horizon, deadline)
        except _DeadlineExceeded:
            return self._deadline_fallback(request, values, seasonality, anomalies, drift, quality)

        validation_available = (
            validation.validation_samples > 0 and validation.baseline_mae is not None
        )
        improvement = validation.improvement_ratio
        use_ml = (
            validation_available
            and validation.model_mae is not None
            and improvement is not None
            and improvement >= self.settings.min_model_improvement
        )
        if not validation_available:
            fallback_reasons.append(
                "history is too short for a non-overlapping chronological validation fold"
            )
        elif not use_ml:
            fallback_reasons.append(
                "ML did not outperform the chronological deterministic baseline"
            )

        selection_mae = (
            validation.model_mae
            if use_ml and validation.model_mae is not None
            else validation.baseline_mae
        )
        confidence = (
            _forecast_confidence(
                selection_mae,
                values,
                validation.validation_samples,
                quality,
                anomalies,
                drift,
            )
            if selection_mae is not None
            else 0.0
        )
        low_confidence = confidence < self.settings.min_confidence
        if low_confidence:
            fallback_reasons.append("validation confidence is below the configured threshold")
            use_ml = False
        if quality.irregular_intervals:
            fallback_reasons.append("timestamps contain irregular sampling intervals")
            use_ml = False
        if quality.stale:
            fallback_reasons.append("the newest timestamp is stale")
            use_ml = False

        final_model: Pipeline | None = None
        model_name = "seasonal-naive-v1" if period is not None else "last-value-naive-v1"
        residuals = validation.baseline_residuals
        predictions = _baseline_forecast(values, request.horizon, period)
        if use_ml:
            try:
                deadline.check()
                final_model = _fit_model(values, period)
                deadline.check()
                predictions = _recursive_model_forecast(
                    final_model, values, request.horizon, period
                )
                residuals = validation.model_residuals
                model_name = "ridge-autoregression-v1"
            except (_DeadlineExceeded, ValueError, FloatingPointError):
                fallback_reasons.append("ML fitting or prediction failed its safety checks")
                use_ml = False
                final_model = None

        if low_confidence or quality.irregular_intervals or quality.stale:
            predictions = _safe_baseline_forecast(values, request.horizon, period)
            residuals = validation.baseline_residuals
            model_name = "conservative-seasonal-baseline-v1"
        elif not use_ml:
            predictions = _baseline_forecast(values, request.horizon, period)
            residuals = validation.baseline_residuals
            model_name = "seasonal-naive-v1" if period is not None else "last-value-naive-v1"

        selected_mae = (
            validation.model_mae
            if use_ml and final_model is not None and validation.model_mae is not None
            else validation.baseline_mae
        )
        confidence = (
            _forecast_confidence(
                selected_mae,
                values,
                validation.validation_samples,
                quality,
                anomalies,
                drift,
            )
            if selected_mae is not None
            else 0.0
        )
        now = datetime.now(UTC)
        input_digest = _model_input_digest(request, self.settings, period, model_name)
        version = _new_model_version(input_digest, now)
        persisted = False
        metadata_payload = {
            "seriesId": request.series_id,
            "name": model_name,
            "version": version,
            "featureSchema": MODEL_SCHEMA_VERSION,
            "trainedAt": now.isoformat(),
            "trainingSamples": len(values),
            "resource": request.resource,
            "seasonalPeriod": period,
            "inputDigest": input_digest,
            "validationMAE": selected_mae,
            "baselineValidationMAE": validation.baseline_mae,
            "confidence": confidence,
            "driftDetected": drift.detected,
            "retrainingRecommended": drift.detected or low_confidence,
        }
        if request.persist and request.series_id is not None:
            try:
                deadline.check()
                self.store.save(
                    request.series_id,
                    version,
                    {
                        "artifact_schema": 1,
                        "feature_schema": MODEL_SCHEMA_VERSION,
                        "model_name": model_name,
                        "estimator": final_model,
                        "period": period,
                        "input_digest": input_digest,
                        "version": version,
                    },
                    metadata_payload,
                )
                persisted = True
            except (OSError, _DeadlineExceeded):
                logger.exception("model persistence failed")
                fallback_reasons.append("model persistence was requested but failed")

        retraining_reasons: list[str] = []
        if drift.detected:
            retraining_reasons.append("distribution drift detected")
        if low_confidence:
            retraining_reasons.append("forecast confidence below threshold")
        if anomalies.rate > 0.10:
            retraining_reasons.append("anomaly rate exceeds 10 percent")
        if quality.irregular_intervals:
            retraining_reasons.append("sampling interval is irregular")
        serialized_predictions = [round(value, 9) for value in predictions]
        intervals = (
            _confidence_intervals(serialized_predictions, residuals, request.confidence_level)
            if residuals
            else _unvalidated_uncertainty_bounds(
                serialized_predictions, values, request.confidence_level
            )
        )
        return ForecastResponse(
            predictions=serialized_predictions,
            confidence_intervals=intervals,
            validation_mae=(round(float(selected_mae), 9) if selected_mae is not None else None),
            baseline_validation_mae=(
                round(validation.baseline_mae, 9) if validation.baseline_mae is not None else None
            ),
            model=model_name,
            model_version=version,
            fallback_used=not use_ml,
            fallback_reasons=fallback_reasons,
            confidence=round(confidence, 6),
            validation=ValidationSummary(
                strategy=(
                    "expanding-window-recursive-no-future-observations"
                    if validation.folds
                    else "unavailable-insufficient-history-for-holdout"
                ),
                folds=validation.folds,
                validation_samples=validation.validation_samples,
                training_end_indices=validation.training_end_indices,
                model_mae=(
                    round(validation.model_mae, 9) if validation.model_mae is not None else None
                ),
                baseline_mae=(
                    round(validation.baseline_mae, 9)
                    if validation.baseline_mae is not None
                    else None
                ),
                improvement_ratio=(
                    round(validation.improvement_ratio, 6)
                    if validation.improvement_ratio is not None
                    else None
                ),
            ),
            seasonality=seasonality,
            anomalies=anomalies,
            drift=drift,
            data_quality=quality,
            retraining=RetrainingSummary(
                recommended=bool(retraining_reasons),
                reasons=retraining_reasons,
                policy="retrain on drift, low confidence, high anomaly rate, or irregular sampling",
                minimum_new_samples=max(24, period or 24),
            ),
            model_metadata=ModelMetadata(
                name=model_name,
                version=version,
                feature_schema=MODEL_SCHEMA_VERSION,
                trained_at=now,
                training_samples=len(values),
                persisted=persisted,
            ),
            generated_at=now,
            warning=(
                "Experimental forecast for decision support only; enforce workload policy and "
                "human approval before changing Kubernetes resources."
                + (
                    " Validation was unavailable, so uncertainty bounds are conservative "
                    "heuristics rather than empirically calibrated confidence intervals."
                    if not residuals
                    else ""
                )
            ),
        )

    def _deadline_fallback(
        self,
        request: ForecastRequest,
        values: np.ndarray,
        seasonality: SeasonalitySummary,
        anomalies: AnomalySummary,
        drift: DriftSummary,
        quality: DataQualitySummary,
    ) -> ForecastResponse:
        now = datetime.now(UTC)
        period = seasonality.period
        predictions = _safe_baseline_forecast(values, request.horizon, period)
        model_name = "conservative-seasonal-baseline-v1"
        input_digest = _model_input_digest(request, self.settings, period, model_name)
        version = _new_model_version(input_digest, now)
        return ForecastResponse(
            predictions=predictions,
            confidence_intervals=_unvalidated_uncertainty_bounds(
                predictions, values, request.confidence_level
            ),
            validation_mae=None,
            baseline_validation_mae=None,
            model=model_name,
            model_version=version,
            fallback_used=True,
            fallback_reasons=["forecast execution exceeded its cooperative compute budget"],
            confidence=0.0,
            validation=ValidationSummary(
                strategy="deadline-fallback-no-validation",
                folds=0,
                validation_samples=0,
                training_end_indices=[],
                model_mae=None,
                baseline_mae=None,
                improvement_ratio=None,
            ),
            seasonality=seasonality,
            anomalies=anomalies,
            drift=drift,
            data_quality=quality,
            retraining=RetrainingSummary(
                recommended=True,
                reasons=["cooperative compute budget exhausted before validation completed"],
                policy="retrain on drift, low confidence, high anomaly rate, or irregular sampling",
                minimum_new_samples=max(24, period or 24),
            ),
            model_metadata=ModelMetadata(
                name=model_name,
                version=version,
                feature_schema=MODEL_SCHEMA_VERSION,
                trained_at=now,
                training_samples=len(values),
                persisted=False,
            ),
            generated_at=now,
            warning=(
                "Experimental forecast for decision support only; validation did not complete, "
                "so uncertainty bounds are conservative heuristics rather than empirically "
                "calibrated confidence intervals. Enforce workload policy and human approval "
                "before changing Kubernetes resources."
            ),
        )


class _BodyTooLarge(Exception):
    pass


class BodyLimitMiddleware:
    """Reject declared and streaming request bodies above a fixed byte limit."""

    def __init__(self, app: ASGIApp, max_bytes: int) -> None:
        self.app = app
        self.max_bytes = max_bytes

    async def __call__(self, scope: Scope, receive: Receive, send: Send) -> None:
        if scope["type"] != "http":
            await self.app(scope, receive, send)
            return
        headers = dict(scope.get("headers", []))
        content_length = headers.get(b"content-length")
        if content_length is not None:
            try:
                if int(content_length) > self.max_bytes:
                    response = JSONResponse(
                        status_code=413,
                        content={
                            "error": {
                                "code": "payload_too_large",
                                "message": "request body is too large",
                            }
                        },
                    )
                    await response(scope, receive, send)
                    return
            except ValueError:
                response = JSONResponse(
                    status_code=400,
                    content={
                        "error": {
                            "code": "invalid_content_length",
                            "message": "invalid Content-Length header",
                        }
                    },
                )
                await response(scope, receive, send)
                return

        received = 0

        async def limited_receive() -> Message:
            nonlocal received
            message = await receive()
            if message["type"] == "http.request":
                received += len(message.get("body", b""))
                if received > self.max_bytes:
                    raise _BodyTooLarge
            return message

        try:
            await self.app(scope, limited_receive, send)
        except _BodyTooLarge:
            response = JSONResponse(
                status_code=413,
                content={
                    "error": {
                        "code": "payload_too_large",
                        "message": "request body is too large",
                    }
                },
            )
            await response(scope, receive, send)


@dataclass
class ServiceMetrics:
    registry: CollectorRegistry
    requests: Counter
    duration: Histogram
    in_progress: Gauge
    fallbacks: Counter
    persistence_failures: Counter


def _create_metrics() -> ServiceMetrics:
    registry = CollectorRegistry()
    service_info = Info("kuberight_ml_service", "KubeRight ML service build", registry=registry)
    service_info.info({"version": SERVICE_VERSION, "model_schema": MODEL_SCHEMA_VERSION})
    return ServiceMetrics(
        registry=registry,
        requests=Counter(
            "kuberight_ml_forecast_requests_total",
            "Forecast requests by outcome",
            ("outcome",),
            registry=registry,
        ),
        duration=Histogram(
            "kuberight_ml_forecast_duration_seconds",
            "Forecast request execution duration",
            buckets=(0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10),
            registry=registry,
        ),
        in_progress=Gauge(
            "kuberight_ml_forecasts_in_progress",
            "Forecast computations currently running",
            registry=registry,
        ),
        fallbacks=Counter(
            "kuberight_ml_forecast_fallbacks_total",
            "Forecasts served by a deterministic fallback",
            registry=registry,
        ),
        persistence_failures=Counter(
            "kuberight_ml_persistence_failures_total",
            "Requested model persistence operations that failed",
            registry=registry,
        ),
    )


def _request_id(request: Request) -> str:
    return getattr(request.state, "request_id", "unknown")


def create_app(settings: Settings | None = None, engine: ForecastEngine | None = None) -> FastAPI:
    config = settings or Settings.from_env()
    forecast_engine = engine or ForecastEngine(config)
    metrics = _create_metrics()
    slots = threading.BoundedSemaphore(config.max_concurrent_forecasts)
    application = FastAPI(
        title="KubeRight Forecasting",
        version=SERVICE_VERSION,
        description=(
            "Experimental, bounded CPU and memory forecasting with chronological "
            "validation, uncertainty, drift detection, and safe deterministic fallbacks."
        ),
    )
    application.state.settings = config
    application.state.engine = forecast_engine
    application.state.metrics = metrics
    application.add_middleware(BodyLimitMiddleware, max_bytes=config.max_body_bytes)

    @application.middleware("http")
    async def request_context(request: Request, call_next: Any) -> Response:
        supplied = request.headers.get("x-request-id", "")
        request_id = supplied if REQUEST_ID_PATTERN.fullmatch(supplied) else uuid.uuid4().hex
        request.state.request_id = request_id
        started = time.monotonic()
        response = await call_next(request)
        response.headers["X-Request-ID"] = request_id
        response.headers["X-Content-Type-Options"] = "nosniff"
        response.headers["Cache-Control"] = "no-store"
        logger.info(
            json.dumps(
                {
                    "event": "http_request",
                    "method": request.method,
                    "path": request.url.path,
                    "status": response.status_code,
                    "request_id": request_id,
                    "duration_ms": round((time.monotonic() - started) * 1_000, 3),
                },
                separators=(",", ":"),
            )
        )
        return response

    @application.exception_handler(RequestValidationError)
    async def validation_error(request: Request, exc: RequestValidationError) -> JSONResponse:
        details = [
            {
                "location": list(error["loc"]),
                "message": error["msg"],
                "type": error["type"],
            }
            for error in exc.errors()
        ]
        return JSONResponse(
            status_code=422,
            content={
                "error": {
                    "code": "validation_error",
                    "message": "request validation failed",
                    "details": details,
                },
                "requestId": _request_id(request),
            },
        )

    @application.exception_handler(HTTPException)
    async def http_error(request: Request, exc: HTTPException) -> JSONResponse:
        message = exc.detail if isinstance(exc.detail, str) else "request failed"
        return JSONResponse(
            status_code=exc.status_code,
            headers=exc.headers,
            content={
                "error": {"code": f"http_{exc.status_code}", "message": message},
                "requestId": _request_id(request),
            },
        )

    def require_api_key(
        supplied: Annotated[str | None, Header(alias="X-API-Key")] = None,
    ) -> None:
        if config.api_key is None:
            return
        if supplied is None or not hmac.compare_digest(supplied, config.api_key):
            raise HTTPException(
                status_code=401,
                detail="valid API key required",
                headers={"WWW-Authenticate": "ApiKey"},
            )

    @application.get("/healthz", tags=["operations"])
    def health() -> dict[str, str]:
        return {"status": "ok", "service": "kuberight-ml", "version": SERVICE_VERSION}

    @application.get("/readyz", tags=["operations"])
    def readiness() -> JSONResponse:
        ready, model_store = forecast_engine.store.readiness()
        payload = {
            "status": "ready" if ready else "not-ready",
            "checks": {"modelStore": model_store, "modelRuntime": "ready"},
        }
        return JSONResponse(status_code=200 if ready else 503, content=payload)

    @application.get("/metrics", include_in_schema=False)
    def prometheus_metrics() -> Response:
        return Response(content=generate_latest(metrics.registry), media_type=CONTENT_TYPE_LATEST)

    @application.post(
        "/api/v1/forecast",
        response_model=ForecastResponse,
        response_model_by_alias=True,
        dependencies=[Depends(require_api_key)],
        tags=["forecasting"],
    )
    def forecast(request: ForecastRequest) -> ForecastResponse:
        if not slots.acquire(blocking=False):
            metrics.requests.labels(outcome="busy").inc()
            raise HTTPException(
                status_code=429,
                detail="forecast capacity is busy; retry later",
                headers={"Retry-After": "1"},
            )
        metrics.in_progress.inc()
        started = time.monotonic()
        try:
            response = forecast_engine.forecast(request)
            outcome = "fallback" if response.fallback_used else "ml"
            metrics.requests.labels(outcome=outcome).inc()
            if response.fallback_used:
                metrics.fallbacks.inc()
            if request.persist and not response.model_metadata.persisted:
                metrics.persistence_failures.inc()
            return response
        except HTTPException:
            metrics.requests.labels(outcome="rejected").inc()
            raise
        except Exception as exc:
            metrics.requests.labels(outcome="error").inc()
            logger.exception("unexpected forecast failure")
            raise HTTPException(status_code=500, detail="forecast failed safely") from exc
        finally:
            metrics.duration.observe(time.monotonic() - started)
            metrics.in_progress.dec()
            slots.release()

    @application.get(
        "/api/v1/models/{series_id}",
        dependencies=[Depends(require_api_key)],
        tags=["forecasting"],
    )
    def model_metadata(
        series_id: Annotated[
            str,
            APIPath(min_length=1, max_length=128, pattern=SERIES_ID_PATTERN.pattern),
        ],
    ) -> dict[str, Any]:
        metadata = forecast_engine.store.latest_metadata(series_id)
        if metadata is None:
            raise HTTPException(status_code=404, detail="model metadata not found")
        return metadata

    return application


app = create_app()
