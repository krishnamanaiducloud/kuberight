"""Experimental forecasting service; no Kubernetes write access."""
from fastapi import FastAPI, HTTPException
from pydantic import BaseModel, Field
from sklearn.ensemble import HistGradientBoostingRegressor
import numpy as np

app = FastAPI(title="KubeRight Forecasting", version="1.0.0")

class ForecastRequest(BaseModel):
    samples: list[float] = Field(min_length=168, max_length=10080)
    horizon: int = Field(default=24, ge=1, le=168)

@app.get("/healthz")
def health():
    return {"status": "ok"}

@app.post("/api/v1/forecast")
def forecast(req: ForecastRequest):
    values = np.asarray(req.samples, dtype=float)
    if not np.all(np.isfinite(values)) or np.any(values < 0):
        raise HTTPException(422, "samples must be finite and nonnegative")
    lag = 24
    x = np.array([values[i-lag:i] for i in range(lag, len(values))])
    y = values[lag:]
    if len(y) < 100:
        raise HTTPException(422, "insufficient training history")
    split = int(len(y)*.8)
    model = HistGradientBoostingRegressor(max_iter=100, max_leaf_nodes=15, random_state=42)
    model.fit(x[:split], y[:split])
    validation = model.predict(x[split:])
    mae = float(np.mean(np.abs(validation-y[split:])))
    model.fit(x, y)
    history = list(values[-lag:])
    output = []
    for _ in range(req.horizon):
        pred = max(0., float(model.predict(np.array(history[-lag:]).reshape(1,-1))[0]))
        output.append(pred)
        history.append(pred)
    return {"predictions": output, "validationMAE": mae, "model": "hist-gradient-boosting-lag24", "experimental": True, "warning": "Do not apply forecast to resource requests without reliability validation"}
