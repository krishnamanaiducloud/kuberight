# KubeRight

KubeRight is a Kubernetes resource-inventory and right-sizing project. It combines a Go API, a React dashboard, an optional Python forecasting service, and a Helm chart.

> **Development preview:** this repository is not a verified production release. A source checkout, chart version, or Dockerfile is not evidence that an image has been published or passed the required release gates. Verify a signed release and its digest before deploying.

## What works today

- Namespace-scoped or cluster-scoped, read-only inventory of Kubernetes workloads and autoscalers.
- Deterministic CPU and memory recommendations, including confidence, risk, data-quality, burst, HPA, VPA, KEDA, restart, and OOM warnings. Recommendations can be calculated from caller-supplied samples or **generated server-side** from the workload's live requests, limits, replicas, and autoscalers plus its Prometheus usage history. Sparse, stale, or missing history blocks reductions.
- CPU-throttling awareness: five-minute usage averages hide sub-second bursts, so KubeRight also reads CFS throttling. A throttled container is never given less CPU, and an existing CPU limit is raised instead.
- A fleet hotspot view ranks workload containers by sustained and peak throttling, CPU-limit pinning, and peak-to-median spike ratio. Spike analysis distinguishes rare, frequent, and daily recurring bursts; frequent bursts size from P99 and keep CPU-limit headroom.
- Validated values: every recommendation is backtested against the observed history, showing how often usage would have exceeded the current and the recommended requests and limits. A recommended memory limit always keeps 10% headroom above the observed peak, so it would not have caused an OOM kill.
- Trend-aware sizing: a sustained rising trend (linear fit over at least a day, R² ≥ 0.5, ≥ 2% per day) is projected over a configurable horizon. Growing memory is never reduced and is flagged as a possible leak.
- Rollout-aware windows: when a Deployment rollout shifts CPU by 25% or more, only post-rollout samples are used, or reductions are blocked if too few remain.
- Correlated signals per container: CPU, throttling, memory, replicas, and optional application latency and request rate on one time axis. Restarts, OOM kills, and rollouts are marked on every chart, with Pearson correlations and findings such as "latency follows throttling".
- A correlation analysis view that reports association, never causation: Pearson and Spearman coefficients with confidence intervals and autocorrelation-adjusted significance, lead/lag detection, change points matched to rollouts and scaling events, and before/after comparisons. See [correlation analysis and recommendation accuracy](#correlation-analysis-and-recommendation-accuracy).
- Every generated recommendation carries an assessment with a verdict of `recommend`, `hold`, `insufficient_data`, or `unsupported`. Only `recommend` can be approved. Applied changes are verified afterwards, and regressions raise an alert.
- Built-in conservative, balanced, and aggressive profiles, plus fine-tuning of the sizing percentile (P90, P95, P99, or peak), safety margins, and maximum reduction per change. The dashboard recalculates a live preview against real usage as you adjust them, before anything is saved.
- Fleet efficiency per namespace, with a per-workload drill-down: requested versus average and P95 usage, reclaimable CPU and memory, reclaimable monthly cost, and containers that set no requests.
- Node impact: pods are bin-packed by requests onto the cluster's node shapes to estimate how many nodes are needed now and after the recommendations are applied. The view also explains how Karpenter or Cluster Autoscaler would realize the savings.
- A workload page with every container's settings, latest recommendation, usage charts, and recommendation history. Whole namespaces or workloads can be analyzed in one action.
- Prometheus usage history per container, charted against current requests and limits.
- A review workflow: recommendations are approved or rejected, individually or in bulk, with optional separation of duties, and every decision is audited. Prometheus-generated advice carries workload/specification identity, observation/query/policy provenance, and a 24-hour expiry; approval fails if that advice is expired or no longer matches the live workload and resources. Approval records a decision only; KubeRight never applies a change. The dashboard shows a `kubectl` command or a strategic-merge YAML patch for each approved change, and exports approved changes as a patch file for GitOps or `kubectl patch`.
- Durable PostgreSQL storage for recommendations, reviews, idempotency keys, and audit events. Embedded checksummed migrations run transactionally under an advisory lock; the chart uses a bounded pre-install/pre-upgrade migration Job while serving pods verify the exact schema without DDL. An append-only trigger rejects audit-table mutation. Without a database, a bounded in-process store is used.
- Forecasts from the forecasting service using Prometheus history, with confidence intervals and model diagnostics.
- Request-based monthly cost estimates from operator-configured unit prices.
- Bearer authentication using Kubernetes `TokenReview` plus per-request `SubjectAccessReview`, or a development-only static token mode.
- A dashboard backed by the versioned Go API. Views whose integrations are unavailable display that state instead of fabricated data.
- Secure-by-default workload settings in a Helm chart, plus development, production-shaped, OpenShift, and cluster-wide rendering profiles.

KubeRight does **not** integrate with OpenCost or cloud billing, provide a complete FinOps ledger, register remote clusters, create GitOps pull requests, or apply recommendations; the chart rejects apply mode and grants no write RBAC. ML model files and the webhook delivery queue are pod/process-local. See [known limitations](docs/known-limitations.md) before evaluating it for production.

## Quick start from source

Prerequisites are Go 1.27.3, Node.js 24, and npm. This local mode disables in-cluster inventory and uses a static token; it is for development only.

```sh
npm ci --prefix web
npm run build --prefix web

export KUBERIGHT_AUTH_MODE=static
export KUBERIGHT_API_TOKEN='replace-with-at-least-32-random-characters'
export KUBERIGHT_INVENTORY_ENABLED=false
export KUBERIGHT_WEB_ROOT="$PWD/web/dist"
go run ./cmd/kuberight
```

Open `http://127.0.0.1:8080`, enter the same bearer token in the dashboard, or call the API directly:

```sh
curl -H "Authorization: Bearer $KUBERIGHT_API_TOKEN" \
  http://127.0.0.1:8080/api/v1/version
```

### Optional integrations

Each integration is independent. An unconfigured or unreachable one is reported on the Integrations page and in `/readyz`; only the recommendation store and authentication can make the service not ready.

| Setting | Effect |
| --- | --- |
| `KUBERIGHT_DATABASE_URL` | PostgreSQL connection string (`postgres://…`, prefer `sslmode=verify-full`). Enables durable, shared API state. `KUBERIGHT_DATABASE_MIGRATE` defaults to `false`, so serving startup verifies without DDL; run the one-shot `kuberight migrate` command first. The chart owns that command through its migration hook. |
| `KUBERIGHT_PROMETHEUS_URL` | Prometheus with cAdvisor container metrics. kube-state-metrics adds restart and OOM history. HTTP URLs also need `KUBERIGHT_ALLOW_INSECURE_UPSTREAMS=true`; `KUBERIGHT_PROMETHEUS_MAX_IN_FLIGHT` defaults to 8 per API pod. |
| `KUBERIGHT_RECOMMENDATION_WINDOW`, `KUBERIGHT_RECOMMENDATION_STEP` | Usage lookback and resolution for generated recommendations (defaults `168h` and `5m`). |
| `KUBERIGHT_ML_URL`, `KUBERIGHT_ML_API_KEY` | Forecasting service base URL and optional shared key. HTTP URLs also need `KUBERIGHT_ML_ALLOW_HTTP=true`. `KUBERIGHT_FORECAST_WINDOW` and `KUBERIGHT_FORECAST_STEP` default to `336h` and `1h`. |
| `KUBERIGHT_COST_CPU_CORE_HOUR`, `KUBERIGHT_COST_MEMORY_GIB_HOUR`, `KUBERIGHT_COST_CURRENCY` | Unit prices for savings estimates. Set both prices or neither. |
| `KUBERIGHT_REQUIRE_DISTINCT_APPROVER` | Rejects reviews by the identity that created a recommendation. Defaults to `true` with Kubernetes authentication and `false` in static-token mode. |
| `KUBERIGHT_LATENCY_QUERY` | One-line PromQL template in seconds, using `$namespace`, `$workload`, or `$container`, for example a p95 from an HTTP server or service-mesh histogram. Enables latency correlation. |
| `KUBERIGHT_REQUEST_RATE_QUERY` | One-line PromQL template in requests per second, with the same placeholders, for example `sum(rate(http_requests_total{namespace="$namespace",service="$workload"}[5m]))`. Enables traffic correlation. |
| `KUBERIGHT_VERIFICATION_INTERVAL` | How often each API pod verifies applied recommendations (default `15m`, between `1m` and `24h`). `0` disables the background check; the verification endpoint still works on demand. Needs Prometheus and the in-cluster inventory. |
| `KUBERIGHT_WEBHOOK_URL`, `KUBERIGHT_WEBHOOK_SECRET`, `KUBERIGHT_WEBHOOK_FORMAT` | Sends `recommendation.created`, `.approved`, `.rejected`, and `.regressed` events as JSON or Slack messages. With a secret, each request carries `X-KubeRight-Signature: sha256=HMAC(secret, timestamp + "." + body)` and `X-KubeRight-Timestamp`. |
| `KUBERIGHT_EXPORT_RECOMMENDATION_METRICS` | Adds per-container current and recommended request and limit gauges, review status, and savings to `/metrics`. Off by default because `/metrics` is unauthenticated. |

### Integrations and platforms

- **Node autoscalers:** reads Karpenter NodePools (consolidation policy, `consolidateAfter`, disruption budgets) and detects Cluster Autoscaler to explain how node savings are realized. KubeRight never changes NodePools.
- **Observability:** Prometheus text exposition for Grafana, Coralogix, Datadog, New Relic, and OpenMetrics-capable scrapers, plus correlation with your own latency series. Per-recommendation gauges are opt-in because `/metrics` is public.
- **Change delivery:** strategic-merge patches for Argo CD, Flux, kustomize, or `kubectl patch`, and signed webhooks for Slack, Microsoft Teams workflows, PagerDuty, Argo Events, or CI.
- **Autoscaler awareness:** HPA, KEDA, and VPA settings are read and respected per container.
- **Platforms:** linux/amd64 and linux/arm64 images; profiles for production, OpenShift, and cluster-wide inventory; spot capacity labels recognized on EKS, GKE, AKS, and Karpenter. Pods default to `kubernetes.io/os: linux` so mixed Windows and Linux clusters schedule them correctly. The API binary also builds and runs on Windows and macOS for development.

Generating recommendations from Prometheus requires the in-cluster inventory, which provides each container's current identity, requests, and limits. The Helm chart exposes these settings as `database`, `prometheus`, `recommendations`, `forecasting`, `cost`, `approvals`, `latencyQuery`, `requestRateQuery`, `verification.interval`, and `ml.apiKey` values. It never deploys PostgreSQL or Prometheus. It refuses `replicaCount > 1` or API autoscaling unless `database.enabled=true`, and rejects ML scaling while model storage remains local. The production-shaped profile enables two API replicas and an HPA, so it requires a pre-created external PostgreSQL Secret, a safe migration credential, and environment-specific topology, network, TLS, backup, and monitoring configuration.

For Kubernetes installation, required RBAC, immutable-image guidance, and platform notes, use the [installation guide](docs/installation.md). In Kubernetes authentication mode, users also need RBAC for the virtual `kuberight.io` resources. Reviewing requires `update` on `recommendations/approval`, generating forecasts requires `create` on `forecasts`, the efficiency views require `list` on `efficiency`, and node impact requires `list` on `capacity`:

```yaml
- apiGroups: ["kuberight.io"]
  resources: ["recommendations/approval"]
  verbs: ["update"]
```

## Correlation analysis and recommendation accuracy

### Correlation analysis

`GET /api/v1/correlation?namespace=&kind=&workload=&container=&window=` aligns CPU, CPU per replica, memory, throttling, replicas, node pressure, and the optional latency and request-rate series on one timestamp grid. `window` defaults to `168h` and can be up to `720h`. The Correlation analysis page in the dashboard shows the same report. The endpoint needs `list` on `metrics-history`. The page includes the latest recommendation and its verification when the caller can also `list` `recommendations`.

- **Coefficients:** Pearson with a 95% Fisher confidence interval, plus Spearman rank correlation. The interval and the p-value use an effective sample size that is reduced for autocorrelation, so smooth, trending series are not reported as significant by accident.
- **Lead/lag:** cross-correlation finds whether one signal leads another and by how much, for example throttling that precedes latency by one step.
- **Normalization:** total CPU is divided by replicas, so HPA scale-outs do not hide per-pod changes.
- **Regimes:** after a rollout, correlations use only the post-rollout regime when enough samples follow it.
- **Change points:** binary segmentation on a median-filtered series, with a BIC-style penalty. Each change point is matched to the nearest rollout or scaling event.
- **Events:** each rollout and scale event gets a before/after comparison per signal.

Findings are worded as associations. KubeRight does not claim that one signal causes another.

### Assessment and verdicts

Recommendations generated from Prometheus carry an `assessment`:

| Verdict | Meaning |
| --- | --- |
| `recommend` | Every check passed. Only this verdict can be approved. |
| `hold` | At least one check failed. The reasons are listed, and approval returns `409 recommendation_not_approvable`. |
| `insufficient_data` | Too little, stale, or patchy history. No change is proposed. |
| `unsupported` | VPA controls both CPU and memory, so KubeRight defers to it. Init containers are rejected with `422 unsupported_container`. |

The assessment covers:

- **Quantities:** Kubernetes quantities (`250m`, `512Mi`), rounded to `5m` above one core and to whole MiB.
- **Margins:** dynamic safety margins. The policy base gains additions for variability (up to 25 points), regular bursts (10), forecast uncertainty (up to 20), and high-criticality or stateful workloads (10 and 5).
- **Validation windows:** multi-window validation over recent sub-windows.
- **Held-back test:** the recommendation is sized on the first 70% of the history and checked against the last 30%.
- **Pod effects:** QoS-class protection, and pod-level requests that include init containers, sidecars, and overhead.
- **Namespace and node limits:** LimitRange bounds and defaults, ResourceQuota headroom, and whether the pod still fits on a schedulable node.
- **HPA impact:** the projected HPA utilization and replica count. A projection above `maxReplicas` holds the change. KEDA is treated like an HPA when it scales on CPU utilization.
- **Forecast:** the forecast is used for sizing only when its interval is calibrated, meaning held-out coverage is within 10 points of the nominal level.
- **Risk and validity:** a 0–100 risk score and an expiry of 24 hours. The expiry is 12 hours with significant trends, regular spikes, or highly variable usage, and 6 hours after a recent rollout.

### Post-change verification and alerting

`GET /api/v1/recommendations/{id}/verification` compares an approved recommendation against the live workload. Once the live requests and limits are within 2% of the approved values and at least one hour of post-change history exists, it compares the following against the same length of history before the change:

- OOM kills;
- the restart rate;
- CPU throttling;
- latency, when configured;
- peak memory.

A regression is any of the following:

- any new OOM kill;
- at least double the restart rate;
- throttling that reaches 5% and rises by at least 5 points;
- latency that rises 25% or more.

The background verifier exports these metrics:

- `kuberight_recommendation_verifications{status}`;
- `kuberight_recommendation_regressions`.

It sends one `recommendation.regressed` webhook per regression. With `metrics.prometheusRule.enabled=true`, the chart adds the `KubeRightRecommendationRegressed` alert.

### Benchmarks

The datasets are synthetic and reproducible, generated from a fixed seed, and the results come from those datasets only. They are not a measurement of accuracy on your clusters. Run them yourself:

```sh
go run ./cmd/kuberight benchmark            # optimizer: JSON report with methodology
python -m ml.benchmark --output report.json # forecasting service
```

**Optimizer benchmark (`internal/benchmark`).**

- **Datasets:** 11 workload archetypes: stable, bursty API, memory-intensive, Java GC sawtooth, StatefulSet, HPA-scaled, KEDA-scaled, CronJob, sidecar, recent rollout, and incomplete metrics.
- **Samples:** 4,032 samples per dataset at 5-minute steps.
- **Method:** each strategy sizes from 7 training days and is scored on the following 7 unseen days.
- **False positive:** a reduction followed by CPU above the new request in more than 25% of test samples, or memory above the new limit.
- **Regression:** new limit breaches that the current settings did not have.

| Strategy | Changes | OOM exposures | False positives | Regressions | Mean CPU savings | Mean memory savings |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| KubeRight (balanced, full assessment) | 10 | 0 | 0 | 0 | 11.7% | 12.0% |
| Keep current | 0 | 0 | 0 | 0 | 0% | 0% |
| P95, no margin | 11 | 8 | 8 | 8 | 56.3% | 45.2% |
| VPA-style (P90 + 15%) | 11 | 0 | 0 | 0 | 51.5% | 37.6% |
| Peak + 10% | 11 | 0 | 0 | 0 | 34.4% | 37.9% |

- **Savings cap:** the balanced policy reduces at most 20% per change, so KubeRight saves less per step than the uncapped VPA-style strategy on these datasets.
- **Withheld change:** it made no change on the incomplete-metrics dataset (`insufficient_data`).
- **Increase:** it raised requests on the CronJob dataset, whose busy hours exceed the current settings.

**Forecast benchmark (`ml/benchmark.py`).**

- **Datasets:** the same 11 archetypes, 336 hourly samples each.
- **Method:** a rolling-origin backtest with 3 origins and a 24-hour horizon. Each forecast sees only earlier samples.
- **Scoring:** 792 held-out samples in total, measured against a seasonal-naive baseline.

Results:

- **Coverage:** mean interval coverage was 94.3% against a nominal 90%. 10 of 11 datasets met the nominal level.
- **Against the baseline:** 7 of 11 beat it (skill > 0). On the GC-sawtooth, KEDA-scaled, and CronJob datasets the service selected the seasonal-naive model itself.
- **Weak case:** a level shift (the recent-rollout dataset) reached 75% coverage and was worse than the baseline. KubeRight therefore sizes from post-rollout samples only, shortens validity to 6 hours, and uses a forecast for sizing only when it is calibrated.

`POST /api/v1/backtest` on the forecasting service runs the same rolling-origin evaluation on your own history. The Correlation analysis page shows its result for the selected container.

## Repository map

| Path | Purpose |
| --- | --- |
| `cmd/kuberight`, `internal/` | Go API, authentication, inventory, Prometheus usage, forecasting client, recommendations, reviews, and storage |
| `internal/repository/postgres/migrations/` | Embedded, checksummed PostgreSQL schema migrations |
| `web/` | React and TypeScript dashboard |
| `ml/` | Independent FastAPI forecasting service |
| `charts/kuberight/` | Helm chart and profiles |
| `deploy/manifests/` | Generated standalone manifests; do not edit by hand |
| `api/openapi.yaml` | Go API contract artifact; it is not served by the process |
| `.github/workflows/` | Required quality gates and immutable tag release workflow |

## Documentation

- [Architecture](docs/architecture.md)
- [Installation and platform notes](docs/installation.md)
- [Helm values](docs/helm-values.md)
- [API reference](docs/api.md)
- [Operations runbook](docs/operations-runbook.md)
- [Known limitations](docs/known-limitations.md)
- [Security policy](SECURITY.md)
- [Contributing](CONTRIBUTING.md)

Build and release status are tracked separately in [production readiness](docs/PRODUCTION-READINESS.md) and [release status](docs/RELEASE-STATUS.md). Treat unverified checklist entries as incomplete.

## License

Apache License 2.0. See [LICENSE](LICENSE).
