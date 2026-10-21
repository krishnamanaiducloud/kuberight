# API reference

The Go API is versioned under `/api/v1`. Its OpenAPI 3 contract is stored at [`api/openapi.yaml`](../api/openapi.yaml). The process does **not** serve that file at runtime; use the repository artifact with your preferred OpenAPI viewer or validator.

KubeRight is a development preview. `/api/v1` identifies the current contract namespace, but no long-term compatibility or deprecation window has been established.

## Authentication and authorization

All `/api/*` routes require:

```http
Authorization: Bearer <token>
```

In `kubernetes` mode, the local API server authenticates the token with `TokenReview` and authorizes each operation with `SubjectAccessReview` against API group `kuberight.io`. In `static` mode, the configured token grants all application operations.

`/healthz`, `/readyz`, `/metrics`, and dashboard assets are public. Network controls must protect them. The server does not implement TLS or cross-origin access; use same-origin access behind an HTTPS ingress, Route, gateway, or service mesh.

Clients may send `X-Request-ID` using 1–128 letters, digits, `.`, `_`, `:`, or `-`. The server returns the accepted or generated ID. JSON requests must use `Content-Type: application/json`, contain exactly one object, and have no unknown fields. The default body limit is 1 MiB.

Errors use this shape:

```json
{
  "error": {
    "code": "forbidden",
    "message": "permission was denied",
    "requestId": "e52e39d62bc7495a"
  }
}
```

Expected classes include `400` invalid requests, `401` missing or rejected credentials, `403` denied operations, `404` unknown records, `409` conflicting idempotent replays, `413` oversized bodies, `422` invalid recommendation inputs, and `503` unavailable auth, authorization, or provider dependencies.

## Operations endpoints

| Method and path | Authentication | Current behavior |
| --- | --- | --- |
| `GET /healthz` | Public | Process liveness; returns `{"status":"alive"}`. |
| `GET /readyz` | Public | Checks auth dependency and a configured inventory provider; returns 503 if either configured dependency is unavailable. |
| `GET /metrics` | Public | Prometheus text metrics for request counts/duration sums, in-flight requests, and generated recommendations. |

Readiness reports `dataProvider: not_configured` without failing when inventory is intentionally disabled.

## Versioned endpoints

| Method and path | SAR verb/resource | Current behavior |
| --- | --- | --- |
| `GET /api/v1/version` | `get version` | Build version, commit, build date, and operation mode. |
| `GET /api/v1/overview` | `get overview` | In-process recommendation count and inventory availability. |
| `GET /api/v1/clusters` | `list clusters` | Local cluster/version and node summary; node denial becomes a warning. |
| `GET /api/v1/namespaces` | `list namespaces` | Namespace inventory or the configured namespace scope. |
| `GET /api/v1/workloads` | `list workloads` | Supported workload resource settings and replica health. |
| `GET /api/v1/autoscalers` | `list autoscalers` | HPA plus optional KEDA/VPA discovery. |
| `GET /api/v1/recommendations` | `list recommendations` | Bounded in-process recommendation history. |
| `POST /api/v1/recommendations` | `create recommendations` | Calculate and store one recommendation. |
| `POST /api/v1/recommendations:calculate` | `create recommendations` | Alias of the preceding operation. |
| `GET /api/v1/recommendations/{id}` | `get recommendations` | Fetch an in-process record by `rec_...` ID. |
| `GET /api/v1/policies` | `list policies` | The built-in default policy. |
| `GET /api/v1/audit` | `list audit` | In-process recommendation-creation events. |
| `GET /api/v1/settings/integrations` | `list integrations` | Runtime configuration flags, not dependency health. |
| `GET /api/v1/metrics/history` | `list metrics-history` | Explicitly unavailable with the current provider. |
| `GET /api/v1/savings` | `list savings` | Explicitly unavailable until an OpenCost provider supplies verified estimates. |
| `GET /api/v1/forecasts` | `list forecasts` | Explicitly unavailable; the ML service is separate. |
| `GET /api/v1/approvals` | `list approvals` | Explicitly unavailable. |
| `GET /api/v1/gitops/changes` | `list gitops-changes` | Explicitly unavailable. |

An unavailable collection normally returns HTTP 200 with an empty `items` array and `status.available=false`; this distinguishes a missing integration from a genuine empty result. A failure in a configured provider returns 503.

## Collections and pagination

Collection responses use:

```json
{
  "items": [],
  "pagination": {"page": 1, "pageSize": 50, "total": 0},
  "status": {
    "available": false,
    "source": "metrics-history",
    "reason": "the required integration is not configured"
  }
}
```

`page` defaults to 1 and `pageSize` defaults to 50, with a maximum of 200. Inventory collections also accept `namespace`, `cluster`, `filter`, `sort`, and `order=asc|desc`. Recommendation listing accepts `namespace`, `workload`, a case-insensitive `filter`, `confidence=low|medium|high`, `risk=low|medium|high`, `sort=createdAt|namespace|workload|confidence|risk`, and `order`.

## Calculate a recommendation

The caller supplies observed values; KubeRight does not fetch history yet. This minimal request is valid but intentionally produces low-confidence safeguards because it has few samples:

```sh
curl -i http://127.0.0.1:8080/api/v1/recommendations \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: example-2026-10-18-001' \
  --data '{
    "cluster": "local",
    "namespace": "payments",
    "kind": "Deployment",
    "workload": "checkout",
    "container": "app",
    "cpuRequestMilli": 500,
    "memoryRequestBytes": 536870912,
    "cpuSamplesMilli": [95, 110, 130, 105, 120],
    "memorySamplesBytes": [180000000, 190000000, 210000000, 205000000, 200000000],
    "sampleIntervalSeconds": 300
  }'
```

A new record returns 201 and `Location`; replaying the same valid idempotency key, request body, authenticated identity, and namespace returns the original record with 200. Reusing that scoped key for a different body returns 409. Keys are optional, at most 128 characters, and limited to letters, digits, `.`, `_`, `:`, and `-`. State and idempotency mappings disappear on restart and are not shared between replicas.

The input can also include current limits, restart/OOM counts, criticality, autoscaling context, and a complete policy override. The output includes current and proposed requests/limits, P50/P90/P95/P99/max statistics, confidence, risk, data quality, estimated change, warnings, and the effective policy. Missing limits remain missing; the optimizer does not introduce new limits automatically.

## Forecasting API

The optional FastAPI service listens separately, normally on port 8081. It is not proxied or called by the Go service.

| Method and path | Current behavior |
| --- | --- |
| `GET /healthz` | Process health. |
| `GET /readyz` | Model-store writability and runtime readiness. |
| `GET /metrics` | Prometheus metrics. |
| `POST /api/v1/forecast` | Validate a series and return forecast, intervals, validation, quality, drift, anomaly, seasonality, and fallback details. |
| `GET /api/v1/models/{series_id}` | Metadata for the latest locally persisted model. |

When `KUBERIGHT_ML_API_KEY` is set, data routes require `X-API-Key`; probes and metrics do not. A forecast request contains 48–10,080 non-negative finite `samples`, `resource` (`cpu` or `memory`), a horizon up to 168, interval seconds, and optional offset-aware timestamps, seasonal period, confidence level, series ID, and persistence flag. Runtime configuration can impose stricter sample and horizon bounds.

Forecast responses are explicitly experimental. When no valid chronological holdout can be formed, or the cooperative compute budget expires before validation completes, validation error fields are `null`, confidence is zero, and returned uncertainty ranges are labeled conservative heuristics rather than empirically calibrated confidence intervals.

`KUBERIGHT_ML_TIMEOUT_SECONDS` is a cooperative compute budget checked between validation, fitting, prediction, and persistence phases. It cannot interrupt a NumPy or scikit-learn call already in progress and is not a hard wall-clock request timeout. Enforce hard request deadlines at the HTTP server, ingress, or proxy boundary.

FastAPI generates its own interactive schema endpoints, but exposing those endpoints is an operator network decision. Persisted models use the service's local model directory and are ephemeral in the supplied Helm deployment.
