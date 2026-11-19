// Package forecast is KubeRight's built-in forecasting engine. Forecasts
// are advisory: they never change a recommendation or a Kubernetes object.
package forecast

import (
	"encoding/json"
	"errors"
	"time"
)

var (
	// ErrBusy means the engine is at its concurrency bound, or the caller
	// gave up waiting for a slot.
	ErrBusy = errors.New("the forecasting engine is busy")
	// ErrRejected means the request cannot be forecast as given, for
	// example because the history is too short.
	ErrRejected = errors.New("the forecast request was rejected")
	// ErrUnavailable is for forecasters that depend on something that can
	// fail; the built-in engine never returns it.
	ErrUnavailable = errors.New("forecasting is unavailable")
)

// Request is one series to forecast.
type Request struct {
	Samples         []float64   `json:"samples"`
	Timestamps      []time.Time `json:"timestamps,omitempty"`
	Horizon         int         `json:"horizon"`
	IntervalSeconds int         `json:"interval_seconds"`
	Resource        string      `json:"resource"`
	ConfidenceLevel float64     `json:"confidence_level"`
	// SeriesID is an opaque, stable workload-UID/container/resource identity
	// for logs and tracing. The engine keeps no model state between requests.
	SeriesID string `json:"series_id,omitempty"`
}

// Interval is a forecast confidence interval.
type Interval struct {
	Lower float64 `json:"lower"`
	Upper float64 `json:"upper"`
}

// Response contains the fields KubeRight relies on; diagnostic sections are
// passed through unchanged for display.
type Response struct {
	Predictions         []float64       `json:"predictions,omitempty"`
	ConfidenceIntervals []Interval      `json:"confidenceIntervals,omitempty"`
	Model               string          `json:"model"`
	ModelVersion        string          `json:"modelVersion"`
	FallbackUsed        bool            `json:"fallbackUsed"`
	FallbackReasons     []string        `json:"fallbackReasons"`
	Confidence          float64         `json:"confidence"`
	GeneratedAt         time.Time       `json:"generatedAt"`
	Experimental        bool            `json:"experimental"`
	Warning             string          `json:"warning"`
	Validation          json.RawMessage `json:"validation,omitempty"`
	Seasonality         json.RawMessage `json:"seasonality,omitempty"`
	Anomalies           json.RawMessage `json:"anomalies,omitempty"`
	Drift               json.RawMessage `json:"drift,omitempty"`
	DataQuality         json.RawMessage `json:"dataQuality,omitempty"`
	Retraining          json.RawMessage `json:"retraining,omitempty"`
	Calibration         *Calibration    `json:"calibration,omitempty"`
	ModelMetadata       *ModelMetadata  `json:"modelMetadata,omitempty"`
}

// ModelMetadata describes the model the engine selected.
type ModelMetadata struct {
	Name            string    `json:"name"`
	Version         string    `json:"version"`
	FeatureSchema   string    `json:"featureSchema"`
	TrainedAt       time.Time `json:"trainedAt"`
	TrainingSamples int       `json:"trainingSamples"`
	Persisted       bool      `json:"persisted"`
	Reused          bool      `json:"reused,omitempty"`
}

// Calibration is interval coverage measured on held-out data. Intervals are
// widened by Factor when the held-out coverage fell short of the nominal level.
type Calibration struct {
	Method             string   `json:"method"`
	NominalCoverage    float64  `json:"nominalCoverage"`
	HeldOutCoverage    *float64 `json:"heldOutCoverage"`
	CalibratedCoverage *float64 `json:"calibratedCoverage"`
	Factor             float64  `json:"factor"`
	EvaluationSamples  int      `json:"evaluationSamples"`
	Calibrated         bool     `json:"calibrated"`
}

// BacktestRequest replays a history from past origins.
type BacktestRequest struct {
	Samples         []float64 `json:"samples"`
	Horizon         int       `json:"horizon"`
	Origins         int       `json:"origins"`
	IntervalSeconds int       `json:"interval_seconds"`
	Resource        string    `json:"resource"`
	ConfidenceLevel float64   `json:"confidence_level"`
}

// Score summarizes forecast errors against what actually happened.
type Score struct {
	Samples int      `json:"samples"`
	MAE     float64  `json:"mae"`
	RMSE    float64  `json:"rmse"`
	SMAPE   float64  `json:"smape"`
	MAPE    *float64 `json:"mape"`
	// WAPE is the total absolute error over the total actual value.
	WAPE              *float64 `json:"wape,omitempty"`
	Coverage          float64  `json:"coverage"`
	MeanIntervalWidth float64  `json:"meanIntervalWidth"`
	BaselineMAE       float64  `json:"baselineMAE"`
	Skill             *float64 `json:"skill"`
}

// BacktestPoint is one forecast step and the observation it predicted.
type BacktestPoint struct {
	Index     int     `json:"index"`
	Actual    float64 `json:"actual"`
	Predicted float64 `json:"predicted"`
	Lower     float64 `json:"lower"`
	Upper     float64 `json:"upper"`
	Baseline  float64 `json:"baseline"`
}

// BacktestOrigin scores one forecast origin.
type BacktestOrigin struct {
	OriginIndex int    `json:"originIndex"`
	Model       string `json:"model"`
	Calibrated  bool   `json:"calibrated"`
	Score       Score  `json:"score"`
}

// BacktestResponse is the rolling-origin evaluation.
type BacktestResponse struct {
	Method          string           `json:"method"`
	Horizon         int              `json:"horizon"`
	NominalCoverage float64          `json:"nominalCoverage"`
	Origins         []BacktestOrigin `json:"origins"`
	Overall         Score            `json:"overall"`
	Points          []BacktestPoint  `json:"points"`
	GeneratedAt     time.Time        `json:"generatedAt"`
	Warning         string           `json:"warning"`
}
