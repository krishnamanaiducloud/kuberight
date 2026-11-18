# API reference

The Go API is versioned under `/api/v1`. Its OpenAPI 3 contract is stored at [`api/openapi.yaml`](../api/openapi.yaml). The process does **not** serve that file at runtime; use the repository artifact with an OpenAPI viewer or validator.

KubeRight is a development preview. `/api/v1` identifies the current contract namespace, but no long-term compatibility or deprecation window has been established.

## Authentication and authorization

All `/api/*` routes require:

```http
Authorization: Bearer <token>
```

In `kubernetes` mode, the local API server authenticates the token with `TokenReview` and authorizes each operation with `SubjectAccessReview` against API group `kuberight.io`. In `static` mode, the configured token grants all application operations.

`/healthz`, `/readyz`, `/metrics`, and dashboard assets are public. Network controls must protect them. The server does not implement TLS or cross-origin access; use same-origin access behind an HTTPS ingress, Route, gateway, or service mesh.

Clients may send `X-Request-ID` using 1-128 letters, digits, `.`, `_`, `:`, or `-`. The server returns the accepted or generated ID. JSON requests must use `Content-Type: application/json`, contain exactly one object, and have no unknown fields. The default body limit is 1 MiB.

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

Expected classes include `400` invalid requests, `401` missing or rejected credentials, `403` denied operations, `404` unknown records, `409` conflicting/stale operations, `413` oversized bodies, `422` rejected analysis input, `429` a busy forecasting service, `503` unavailable dependencies, and `504` dependency timeouts.

## Operations endpoints

| Method and path | Authentication | Current behavior |
| --- | --- | --- |
| `GET /healthz` | Public | Process liveness; returns `{"status":"alive"}`. |
| `GET /readyz` | Public | Treats authentication, inventory when configured, and the recommendation repository as critical. Prometheus and forecasting report `ready`, `degraded`, or `not_configured` without failing base readiness. |
| `GET /metrics` | Public | Prometheus text exposition for request counts/duration sums, in-flight requests, generated recommendations, webhook outcomes, and optional per-recommendation gauges. |

`/metrics` is compatible with Prometheus and OpenMetrics-capable collectors, but it uses the Prometheus text format rather than OpenMetrics negotiation. Per-recommendation series are disabled by default because their labels identify workloads.

## Versioned endpoints

| Method and path | SAR verb/resource | Current behavior |
| --- | --- | --- |
| `GET /api/v1/version` | `get version` | Build version, commit, build date, and operation mode. |
| `GET /api/v1/overview` | `get overview` | Inventory/dependency status, recommendation/review counts, and configured-price savings summary. |
| `GET /api/v1/clusters` | `list clusters` | Local cluster/version and node summary; node denial becomes a warning. |
| `GET /api/v1/namespaces` | `list namespaces` | Namespace inventory or the configured namespace scope. |
| `GET /api/v1/workloads` | `list workloads` | Workload identity, resource settings, replica health, and autoscaler context. |
| `GET /api/v1/autoscalers` | `list autoscalers` | HPA plus optional KEDA/VPA discovery. |
| `GET /api/v1/recommendations` | `list recommendations` | Repository-backed recommendation and review history. |
| `POST /api/v1/recommendations` | `create recommendations` | Calculate and store one caller-supplied recommendation. |
| `POST /api/v1/recommendations:calculate` | `create recommendations` | Alias of the preceding operation. |
| `POST /api/v1/recommendations:generate` | `create recommendations` | Read live workload settings and Prometheus usage, then preview or store a recommendation. |
| `GET /api/v1/recommendations/{id}` | `get recommendations` | Fetch one record by `rec_...` ID. |
| `POST /api/v1/recommendations/{id}/review` | `update recommendations/approval` | Approve or reject one pending record; approval performs live freshness checks for Prometheus-backed advice. |
| `GET /api/v1/approvals` | `list approvals` | Pending recommendations by default, or a selected review state. |
| `GET /api/v1/metrics/history` | `list metrics-history` | Prometheus CPU/memory history and current resource overlays for one container. |
| `GET /api/v1/metrics/signals` | `list metrics-history` | CPU, memory, throttling, replicas, events, optional latency, correlations, insights, and spike profile. |
| `GET /api/v1/hotspots` | `list hotspots` | Fleet containers ranked by throttling, CPU-limit pinning, and spikiness. |
| `GET /api/v1/forecasts` | `list forecasts` | Forecast integration status; forecasts are computed on demand and not stored by the Go API. |
| `POST /api/v1/forecasts` | `create forecasts` | Query Prometheus and call the configured forecasting service for one resource series. |
| `GET /api/v1/efficiency` | `list efficiency` | Namespace requested-versus-used efficiency from inventory and Prometheus. |
| `GET /api/v1/efficiency/workloads` | `list efficiency` | Workload/container efficiency for a namespace. |
| `GET /api/v1/savings` | `list savings` | Request-based estimates from operator-configured CPU/memory unit prices. |
| `GET /api/v1/capacity` | `list capacity` | Requests-based node packing for current resources and qualified approved advice. |
| `GET /api/v1/policies` | `list policies` | Conservative, balanced, and aggressive built-in profiles. |
| `GET /api/v1/audit` | `list audit` | Paginated recommendation/review audit events from the active repository. |
| `GET /api/v1/settings/integrations` | `list integrations` | Bounded runtime health checks and configuration state for integrations. |
| `GET /api/v1/gitops/changes` | `list gitops-changes` | Unavailable collection; the dashboard exports patches client-side, but no Git provider is implemented. |

An unavailable collection normally returns HTTP 200 with an empty `items` array and `status.available=false`; this distinguishes an unconfigured integration from a genuine empty result. A failure in a configured provider normally returns 503.

Hotspot attribution lists Kubernetes pods and resolves their `ownerReferences` through ReplicaSets and Jobs to supported workloads; it does not infer ownership from pod names. Rows aggregate the worst observed pod per workload container, and the summary reports metrics that could not be matched safely.

## Collections and pagination

Collection responses use:

```json
{
  "items": [],
  "pagination": {"page": 1, "pageSize": 50, "total": 0},
  "status": {
    "available": false,
    "source": "prometheus",
    "reason": "Prometheus is not configured"
  }
}
```

`page` defaults to 1 and `pageSize` defaults to 50, with a maximum of 200. Inventory collections also accept `namespace`, `cluster`, `filter`, `sort`, and `order=asc|desc`. Recommendation listing accepts `namespace`, `workload`, a case-insensitive `filter`, `confidence=low|medium|high`, `risk=low|medium|high`, `reviewStatus=pending|approved|rejected`, `latest=true|false`, `sort=createdAt|namespace|workload|confidence|risk`, and `order`.

`GET /api/v1/approvals` defaults `reviewStatus` to `pending`; `reviewStatus=all` removes that default. Reviews are terminal: a second review returns 409 instead of replacing the first decision.

## Calculate or generate a recommendation

The calculate route accepts caller-supplied observations. This minimal request is valid but intentionally produces low-confidence safeguards because it has few samples:

```sh
curl -i http://127.0.0.1:8080/api/v1/recommendations \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: example-2026-11-01-001' \
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

The input can also include current limits, restart/OOM counts, throttling ratios, criticality, autoscaling context, timestamps, and a complete policy override. The output includes current and proposed requests/limits, P50/P90/P95/P99/max statistics, confidence, risk, data quality, backtests, spike/trend summaries, estimated change, warnings, and the effective policy. Missing limits remain missing; the optimizer does not introduce new limits automatically.

The generate route accepts a workload subject, optional window/profile/policy, and `dryRun`. It reads the current workload, Prometheus CPU/memory/throttling and optional kube-state-metrics events. Stored Prometheus recommendations include:

- workload UID, generation, resource version, and a resource-spec fingerprint;
- observation start/end and query fingerprint;
- effective-policy fingerprint and optimizer version; and
- a 24-hour expiry.

Approval of one of these records fails with 409 if it expired or the live workload identity, generation/specification, container, or current resources changed. If the live check cannot run, approval fails with 503. Caller-supplied records use source `request` and remain manual decisions without this live identity guarantee.

A new record returns 201 and `Location`; replaying the same valid idempotency key, request body, authenticated identity, and namespace returns the original record with 200. Reusing that scoped key for a different body returns 409. Keys are optional, at most 128 characters, and limited to letters, digits, `.`, `_`, `:`, and `-`. PostgreSQL makes these claims durable/shared; the memory fallback loses them on restart.

## Approval, capacity, and cost semantics

An approval records reviewer acceptance and an audit event in one repository transaction. Rejection requires a comment. Optional separation of duties rejects reviews by the recommendation creator. Neither action mutates Kubernetes.

The capacity route will substitute a recommendation only when the newest record for the exact cluster/namespace/kind/workload/container is Prometheus-backed, approved, unexpired, carries complete observation/query/policy/optimizer provenance, and still matches the live workload UID, generation, spec fingerprint, and current requests/limits. All other containers retain current requests in the simulated scenario. The packing algorithm still ignores scheduling constraints listed in [known limitations](known-limitations.md).

Savings and efficiency cost values are estimates based on operator-configured unit prices, requests, replicas, and 730 hours. They are not billing or realized-savings data.

## Persistence behavior

With `KUBERIGHT_DATABASE_URL`, recommendations, idempotency claims, reviews, and audit events are stored in PostgreSQL. With migration disabled, process startup verifies that every embedded migration is present with the expected version, name, and checksum. A mismatch, older schema, or newer schema fails startup. Without a database URL, the bounded in-process repository is selected and its contents are not shared or durable.

The database audit table has a trigger that rejects update, delete, and truncate statements. This guard does not replace external immutable audit retention.

## Forecasting API

The Go `POST /api/v1/forecasts` route requires both Prometheus and the forecasting service. It fetches 48-10,080 CPU or memory points, calls the service, and returns timestamped predictions, lower/upper intervals, history quality, and diagnostics. Results are experimental and are not persisted by the Go API.

The optional FastAPI service also listens separately, normally on port 8081:

| Method and path | Current behavior |
| --- | --- |
| `GET /healthz` | Process health. |
| `GET /readyz` | Model-store writability and runtime readiness. |
| `GET /metrics` | Prometheus metrics. |
| `POST /api/v1/forecast` | Validate a series and return forecast, intervals, validation, quality, drift, anomaly, seasonality, and fallback details. |
| `GET /api/v1/models/{series_id}` | Metadata for the latest locally persisted model. |

When `KUBERIGHT_ML_API_KEY` is set, data routes require `X-API-Key`; probes and metrics do not. A direct forecast request contains 48-10,080 non-negative finite samples, `resource` (`cpu` or `memory`), a horizon up to 168, interval seconds, and optional offset-aware timestamps, seasonal period, confidence level, series ID, and persistence flag. Runtime configuration can impose stricter bounds.

When no valid chronological holdout can be formed, or the cooperative compute budget expires before validation completes, validation error fields are `null`, confidence is zero, and uncertainty ranges are labeled conservative heuristics rather than empirically calibrated intervals. The compute budget cannot interrupt a NumPy or scikit-learn call already in progress; enforce a hard deadline at the HTTP server, ingress, or proxy boundary.

Persisted ML models use the service's local model directory and are ephemeral in the supplied Helm deployment.
