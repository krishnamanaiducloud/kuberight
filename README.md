# KubeRight

KubeRight is a Kubernetes resource-inventory and right-sizing project. It combines a Go API, a React dashboard, an optional Python forecasting service, and a Helm chart.

> **Development preview:** this repository is not a verified production release. A source checkout, chart version, or Dockerfile is not evidence that an image has been published or passed the required release gates. Verify a signed release and its digest before deploying.

## What works today

- Namespace-scoped or cluster-scoped, read-only inventory of Kubernetes workloads and autoscalers.
- Deterministic CPU and memory recommendations, including confidence, risk, data-quality, burst, HPA, VPA, KEDA, restart, and OOM warnings. Recommendations can be calculated from caller-supplied samples or **generated server-side** from the workload's live requests, limits, replicas, and autoscalers plus its Prometheus usage history. Sparse, stale, or missing history blocks reductions.
- CPU-throttling awareness: five-minute usage averages hide sub-second bursts, so KubeRight also reads CFS throttling. A throttled container is never given less CPU, and its CPU limit is raised instead.
- Validated values: every recommendation is backtested against the observed history, showing how often usage would have exceeded the current and the recommended requests and limits. A recommended memory limit always keeps 10% headroom above the observed peak, so it would not have caused an OOM kill.
- Trend-aware sizing: a sustained rising trend (linear fit over at least a day, R² ≥ 0.5, ≥ 2% per day) is projected over a configurable horizon. Growing memory is never reduced and is flagged as a possible leak.
- Rollout-aware windows: when a Deployment rollout shifts CPU by 25% or more, only post-rollout samples are used, or reductions are blocked if too few remain.
- Correlated signals per container: CPU, throttling, memory, replicas, and optional application latency on one time axis. Restarts, OOM kills, and rollouts are marked on every chart, with Pearson correlations and findings such as "latency follows throttling".
- Built-in conservative, balanced, and aggressive profiles, plus fine-tuning of the sizing percentile (P90, P95, P99, or peak), safety margins, and maximum reduction per change. The dashboard recalculates a live preview against real usage as you adjust them, before anything is saved.
- Fleet efficiency per namespace, with a per-workload drill-down: requested versus average and P95 usage, reclaimable CPU and memory, reclaimable monthly cost, and containers that set no requests.
- Node impact: pods are bin-packed by requests onto the cluster's node shapes to estimate how many nodes are needed now and after the recommendations are applied. The view also explains how Karpenter or Cluster Autoscaler would realize the savings.
- A workload page with every container's settings, latest recommendation, usage charts, and recommendation history. Whole namespaces or workloads can be analyzed in one action.
- Prometheus usage history per container, charted against current requests and limits.
- A review workflow: recommendations are approved or rejected, individually or in bulk, with optional separation of duties, and every decision is audited. Approval records a decision only; KubeRight never applies a change. The dashboard shows a `kubectl` command or a strategic-merge YAML patch for each approved change, and exports every approved change as a patch file for GitOps or `kubectl patch`.
- Durable PostgreSQL storage for recommendations, reviews, idempotency keys, and audit events, with embedded checksummed migrations applied under an advisory lock. Without a database, a bounded in-process store is used.
- Forecasts from the forecasting service using Prometheus history, with confidence intervals and model diagnostics.
- Request-based monthly cost estimates from operator-configured unit prices.
- Bearer authentication using Kubernetes `TokenReview` plus per-request `SubjectAccessReview`, or a development-only static token mode.
- A dashboard backed by the versioned Go API. Views whose integrations are unavailable display that state instead of fabricated data.
- Secure-by-default workload settings in a Helm chart, plus development, production-shaped, OpenShift, and cluster-wide rendering profiles.

KubeRight does **not** integrate with OpenCost or billing data, register remote clusters, create GitOps pull requests, or apply recommendations; the chart rejects apply mode and grants no write RBAC. Some pages under `docs/` predate the integrations above and still describe them as missing; this README and `api/openapi.yaml` are current.

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
| `KUBERIGHT_DATABASE_URL` | PostgreSQL connection string (`postgres://…`, prefer `sslmode=verify-full`). Enables durable, shared state and therefore more than one API replica. `KUBERIGHT_DATABASE_MIGRATE=false` makes startup verify the schema instead of migrating it. |
| `KUBERIGHT_PROMETHEUS_URL` | Prometheus with cAdvisor container metrics. kube-state-metrics adds restart and OOM history. HTTP URLs also need `KUBERIGHT_ALLOW_INSECURE_UPSTREAMS=true`. |
| `KUBERIGHT_RECOMMENDATION_WINDOW`, `KUBERIGHT_RECOMMENDATION_STEP` | Usage lookback and resolution for generated recommendations (defaults `168h` and `5m`). |
| `KUBERIGHT_ML_URL`, `KUBERIGHT_ML_API_KEY` | Forecasting service base URL and optional shared key. HTTP URLs also need `KUBERIGHT_ML_ALLOW_HTTP=true`. `KUBERIGHT_FORECAST_WINDOW` and `KUBERIGHT_FORECAST_STEP` default to `336h` and `1h`. |
| `KUBERIGHT_COST_CPU_CORE_HOUR`, `KUBERIGHT_COST_MEMORY_GIB_HOUR`, `KUBERIGHT_COST_CURRENCY` | Unit prices for savings estimates. Set both prices or neither. |
| `KUBERIGHT_REQUIRE_DISTINCT_APPROVER` | Rejects reviews by the identity that created a recommendation. Defaults to `true` with Kubernetes authentication and `false` in static-token mode. |
| `KUBERIGHT_LATENCY_QUERY` | One-line PromQL template in seconds, using `$namespace`, `$workload`, or `$container`, for example a p95 from an HTTP server or service-mesh histogram. Enables latency correlation. |
| `KUBERIGHT_WEBHOOK_URL`, `KUBERIGHT_WEBHOOK_SECRET`, `KUBERIGHT_WEBHOOK_FORMAT` | Sends `recommendation.created`, `.approved`, and `.rejected` events as JSON or Slack messages. With a secret, each request carries `X-KubeRight-Signature: sha256=HMAC(secret, timestamp + "." + body)` and `X-KubeRight-Timestamp`. |
| `KUBERIGHT_EXPORT_RECOMMENDATION_METRICS` | Adds per-container current and recommended request and limit gauges, review status, and savings to `/metrics`. Off by default because `/metrics` is unauthenticated. |

### Integrations and platforms

- **Node autoscalers:** reads Karpenter NodePools (consolidation policy, `consolidateAfter`, disruption budgets) and detects Cluster Autoscaler to explain how node savings are realized. KubeRight never changes NodePools.
- **Observability:** Prometheus-compatible metrics for Grafana, Coralogix, Datadog, New Relic, and any OpenMetrics scraper, plus correlation with your own latency series.
- **Change delivery:** strategic-merge patches for Argo CD, Flux, kustomize, or `kubectl patch`, and signed webhooks for Slack, Microsoft Teams workflows, PagerDuty, Argo Events, or CI.
- **Autoscaler awareness:** HPA, KEDA, and VPA settings are read and respected per container.
- **Platforms:** linux/amd64 and linux/arm64 images; profiles for production, OpenShift, and cluster-wide inventory; spot capacity labels recognized on EKS, GKE, AKS, and Karpenter. Pods default to `kubernetes.io/os: linux` so mixed Windows and Linux clusters schedule them correctly. The API binary also builds and runs on Windows and macOS for development.

Generating recommendations from Prometheus requires the in-cluster inventory, which provides each container's current requests and limits. The Helm chart exposes these settings as `database`, `prometheus`, `recommendations`, `forecasting`, `cost`, `approvals`, and `ml.apiKey` values. It never deploys a database, and it refuses `replicaCount > 1` or autoscaling unless `database.enabled=true`.

For Kubernetes installation, required RBAC, immutable-image guidance, and platform notes, use the [installation guide](docs/installation.md). In Kubernetes authentication mode, users also need RBAC for the virtual `kuberight.io` resources. Reviewing requires `update` on `recommendations/approval`, generating forecasts requires `create` on `forecasts`, the efficiency views require `list` on `efficiency`, and node impact requires `list` on `capacity`:

```yaml
- apiGroups: ["kuberight.io"]
  resources: ["recommendations/approval"]
  verbs: ["update"]
```

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
