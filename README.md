<img src="web/public/favicon.svg" alt="KubeRight logo" width="72" height="72">

# KubeRight

KubeRight is a Kubernetes resource-inventory and right-sizing project. It is a single Go service that serves the API, the React dashboard, recommendations, diagnostics, and a built-in forecasting engine, installed with a Helm chart or standalone manifests. It needs only an existing Prometheus.

> **Release candidate:** the source candidate passes its pull-request quality gates, but no signed release has been published yet. A source checkout, chart version, or Dockerfile is not evidence that an image exists. Verify a signed release and its digest before deploying.

## What works today

- Namespace-scoped or cluster-scoped inventory of Kubernetes workloads and autoscalers. It is read-only: KubeRight never changes workloads.
- Historical workload attribution: usage is attributed to the current workload UID across rollouts, scale-downs, replaced StatefulSet pods, Jobs, and same-name recreation, using kube-state-metrics ownership and creation history rather than today's pod list. Replica coverage is time-aware. See [historical attribution and replica coverage](#historical-attribution-and-replica-coverage).
- Deterministic CPU and memory recommendations, including confidence, risk, data-quality, burst, HPA, VPA, KEDA, restart, and OOM warnings. Recommendations can be calculated from caller-supplied samples or **generated server-side** from the workload's live requests, limits, replicas, and autoscalers plus its Prometheus usage history. Sparse, stale, or missing history blocks reductions.
- CPU-throttling awareness: five-minute usage averages hide sub-second bursts, so KubeRight also reads CFS throttling. A throttled container is never given less CPU, and an existing CPU limit is raised instead.
- Saturation awareness: on cgroup v2 nodes with Kubernetes 1.34 or later, cAdvisor exports pressure stall information (PSI). Tasks that wait for a CPU indicate saturation even when usage looks low, for example on a busy node or with no CPU limit. KubeRight does not reduce CPU when tasks wait 5% of the time overall or 10% in peak intervals. At 20% or more it raises the CPU request by 25%. It does not reduce memory when tasks stall on memory reclaim 5% of the time in peak intervals. Without PSI series, these checks are skipped and the other signals apply.
- A fleet hotspot view ranks workload containers by sustained and peak throttling, CPU-limit pinning, and peak-to-median spike ratio. Spike analysis distinguishes rare, frequent, and daily recurring bursts; frequent bursts size from P99 and keep CPU-limit headroom.
- Validated values: every recommendation is backtested against the observed history, showing how often usage would have exceeded the current and the recommended requests and limits. A recommended memory limit always keeps 10% headroom above the observed peak, so it would not have caused an OOM kill.
- Trend-aware sizing: a sustained rising trend (linear fit over at least a day, R² ≥ 0.5, ≥ 2% per day) is projected over a configurable horizon. Growing memory is never reduced and is flagged as a possible leak.
- Rollout-aware windows: when a Deployment rollout shifts CPU by 25% or more, only post-rollout samples are used, or reductions are blocked if too few remain.
- Correlated signals per container: CPU, throttling, memory, replicas, and optional application latency and request rate on one time axis. Restarts, OOM kills, and rollouts are marked on every chart, with Pearson correlations and findings such as "latency follows throttling".
- A correlation analysis view that reports association, never causation: Pearson and Spearman coefficients with confidence intervals and autocorrelation-adjusted significance, lead/lag detection, change points matched to rollouts and scaling events, and before/after comparisons. See [correlation analysis and recommendation accuracy](#correlation-analysis-and-recommendation-accuracy).
- Every generated recommendation carries an assessment with a verdict of `recommend`, `hold`, `insufficient_data`, `unsupported`, or `no_change` (already right-sized). Only `recommend` can be approved. Applied changes are verified afterwards, and regressions raise an alert.
- Stability controls stop advice from flapping. Changes below the minimum meaningful change are suppressed. Reductions are held during a cooldown after requests or limits change, while recent recommendations alternate direction, and until enough consistent observations agree. Increases are never held. See [stability](#stability).
- JVM-aware memory sizing: with an explicit `-Xmx`, memory is never sized below the heap plus 25% non-heap overhead. A JVM without one (defaults or `MaxRAMPercentage`) is limited to 10% memory reductions per change.
- Built-in conservative, balanced, and aggressive profiles, plus fine-tuning of the sizing percentile (P90, P95, P99, or peak), safety margins, and maximum reduction per change. The dashboard recalculates a live preview against real usage as you adjust them, before anything is saved.
- Alternatives: every recommendation also shows what each profile would set from the same data, within the policy's resource bounds. For each one it shows the change, the assessment verdict, and a held-out test: the profile sizes from the first 70% of samples, and KubeRight counts how often the remaining 30% exceeded the CPU request or the memory limit. Only the recommendation itself can be approved.
- Fleet efficiency per namespace, with a per-workload drill-down: requested versus average and P95 usage, reclaimable CPU and memory, reclaimable monthly cost, and containers that set no requests.
- Node impact: pods are bin-packed by requests onto the cluster's node shapes to estimate how many nodes are needed now and after the recommendations are applied. Packing honors taints and tolerations, nodeSelector, required node and pod affinity, required pod anti-affinity, and DoNotSchedule topology spread, and reports pods that no node accepts. Pods covered by a PodDisruptionBudget that allows no evictions stay on their current nodes, so those nodes are kept. The view also explains how Karpenter or Cluster Autoscaler would realize the savings.
- Node groups: node impact is split by the group that provisions each node: Karpenter NodePools, EKS managed and eksctl node groups, GKE node pools, AKS agent pools, and OpenShift and Cluster API MachineSets. Membership is *verified* when it comes from the platform's own label or annotation, or from the node's OpenShift Machine. It is *inferred* when it comes from a Machine name or, for a node with no group, from its shape. Each group says how it would shrink: Karpenter consolidation, Cluster Autoscaler, the provider's node-pool autoscaler, a MachineAutoscaler, or by hand. Minimum and maximum sizes from a MachineAutoscaler or from Cluster Autoscaler's status bound how far each group can shrink.
- A workload page with every container's settings, latest recommendation, usage charts, and recommendation history. Whole namespaces or workloads can be analyzed in one action.
- Prometheus usage history per container, charted against current requests and limits.
- A review workflow: recommendations are approved or rejected, individually or in bulk, and every decision is audited. Prometheus-generated advice carries workload/specification identity, observation/query/policy provenance, and a 24-hour expiry; approval fails if that advice is expired or no longer matches the live workload and resources. Approval records a decision; KubeRight never applies it. For each approved change the dashboard shows a `kubectl` command and a strategic-merge YAML patch, exports approved changes as a patch file for GitOps or `kubectl patch`, and can open a [GitOps pull request](#gitops-pull-requests).
- Durable PostgreSQL storage for recommendations, reviews, idempotency keys, and audit events. Embedded checksummed migrations run transactionally under an advisory lock; the chart uses a bounded pre-install/pre-upgrade migration Job while serving pods verify the exact schema without DDL. An append-only trigger rejects audit-table mutation. Without a database, a bounded in-process store is used.
- Workload health from live Kubernetes status, most severe first: crash loops, image pull and configuration errors, OOM kills, repeated restarts, pods that cannot be scheduled (with the scheduler's reason, such as insufficient CPU or memory), evictions, failing readiness, and nodes that are not ready or under pressure. Each finding is labelled as observed, and older causes that Kubernetes no longer records are not claimed.
- Forecasts from a built-in engine using Prometheus history. Each forecast chooses its model by testing candidates on the series' own past against a seasonal-naive baseline, and reports that accuracy with its prediction interval. Hourly forecasts reach 7 days; daily forecasts reach 30 and 90 days from daily mean CPU and daily peak memory, and need at least 48 days of Prometheus history, and no fewer days than the horizon.
- Request-based monthly cost estimates priced per namespace from [OpenCost](#cost-and-opencost) when configured, or from operator-configured unit prices. Every savings item names its price source. Node impact keeps three amounts apart: the request reduction, the feasible savings from removing the nodes the packing frees, and the bill, which KubeRight does not measure.
- Persistent scoped policies (global, namespace, workload, container) with deterministic precedence. A cluster-specific scope wins over an all-clusters scope at the same level, then higher priority wins. Duplicate enabled scopes with equal priority are rejected, and resolution lists every applicable policy in order.
- **No sign-in**, like Goldilocks: the dashboard and API open directly, with no token, login page, or sidecar. See [access](#access).
- [GitOps pull requests](#gitops-pull-requests): an approved recommendation can be proposed as a pull request (GitHub, Gitea/Forgejo) or merge request (GitLab) that edits the workload's manifest.
- A dashboard backed by the versioned Go API. It opens on CPU recommendations. The fleet overview, at `/overview`, counts pods, healthy workloads and those that need attention, OOM kills, restart anomalies, unschedulable pods, and CPU hotspots, next to Prometheus and forecasting health. Health counts use the same rules as the workload health view and cover workloads that have pods. It passes an automated WCAG 2.1 AA audit on every view. Views whose integrations are unavailable display that state instead of fabricated data.
- Prometheus metric coverage on the Integrations page: whether CPU usage, memory usage, CPU throttling, kube-state-metrics, and node condition series are present, counted over the last 10 minutes, and which features need each. Missing metrics are reported as missing, never treated as zero.
- Secure-by-default workload settings in a Helm chart, plus development, production-shaped, OpenShift, and cluster-wide rendering profiles.

KubeRight does **not**:

- ingest cloud billing or provide a complete FinOps ledger;
- register remote clusters.

It never changes workloads: its ServiceAccount can only read them (see [read-only by design](#read-only-by-design)). With the PostgreSQL store, webhook delivery is durable; without it, the delivery queue is process-local. See [known limitations](docs/known-limitations.md) before evaluating it for production.

## Quick start from source

Prerequisites are Go 1.27.4, Node.js 24, and npm. This local mode disables the in-cluster inventory; it is for development only.

```sh
npm ci --prefix web
npm run build --prefix web

export KUBERIGHT_INVENTORY_ENABLED=false
export KUBERIGHT_WEB_ROOT="$PWD/web/dist"
go run ./cmd/kuberight
```

Open `http://127.0.0.1:8080`, or call the API directly:

```sh
curl http://127.0.0.1:8080/api/v1/version
```

### Optional integrations

Each integration is independent. An unconfigured or unreachable one is reported on the Integrations page and in `/readyz`; only the recommendation store and an invalid configuration can make the service not ready.

| Setting | Effect |
| --- | --- |
| `KUBERIGHT_DATABASE_URL` | PostgreSQL connection string (`postgres://…`, prefer `sslmode=verify-full`). Enables durable, shared API state. `KUBERIGHT_DATABASE_MIGRATE` defaults to `false`, so serving startup verifies without DDL; run the one-shot `kuberight migrate` command first. The chart owns that command through its migration hook. |
| `KUBERIGHT_PROMETHEUS_URL` | Prometheus with cAdvisor container metrics. kube-state-metrics adds restart and OOM history. HTTP URLs also need `KUBERIGHT_ALLOW_INSECURE_UPSTREAMS=true`; `KUBERIGHT_PROMETHEUS_MAX_IN_FLIGHT` defaults to 8 per API pod. |
| `KUBERIGHT_PROMETHEUS_BEARER_TOKEN_FILE`, `KUBERIGHT_PROMETHEUS_CA_FILE`, `KUBERIGHT_PROMETHEUS_TENANT_NAMESPACE` | Access to a protected Prometheus: a bearer token file, re-read every minute so a rotating ServiceAccount token keeps working; a PEM bundle trusted in addition to the system roots; and the `namespace` query parameter that multi-tenant proxies require. A token is only ever sent over HTTPS, and redirects are refused. |
| `KUBERIGHT_PROMETHEUS_DISCOVERY` | With no URL set (default `true`), KubeRight uses the cluster's own monitoring when it can find it. On OpenShift, recognised by the service CA that OpenShift mounts next to every ServiceAccount token, that is Thanos Querier with the pod's token and service CA: port 9091 for a cluster-wide inventory, which needs the `cluster-monitoring-view` role, or the tenancy port 9092 for a namespace-scoped one. Elsewhere there is no endpoint all installations share, so set the URL. The Integrations page names the endpoint in use. |
| `KUBERIGHT_RECOMMENDATION_WINDOW`, `KUBERIGHT_RECOMMENDATION_STEP` | Usage lookback and resolution for generated recommendations (defaults `168h` and `5m`). |
| `KUBERIGHT_DEFAULT_SCAN_INTERVAL` | When no scan schedule exists, KubeRight creates one that scans every namespace the inventory can see, starting at once and repeating at this interval (default `6h`), so recommendations appear without anyone queueing a scan. `0s` creates none. Once any schedule exists, schedules are managed on the Fleet page. |
| `KUBERIGHT_FORECAST_WINDOW`, `KUBERIGHT_FORECAST_STEP` | Usage lookback and resolution for forecasts (defaults `336h` and `1h`). Forecasting runs inside KubeRight; there is no service to deploy or URL to set. The removed `KUBERIGHT_ML_*` settings are ignored with a startup warning. |
| `KUBERIGHT_COST_CPU_CORE_HOUR`, `KUBERIGHT_COST_MEMORY_GIB_HOUR`, `KUBERIGHT_COST_CURRENCY` | Unit prices for savings estimates. Set both prices or neither. |
| `KUBERIGHT_LATENCY_QUERY` | One-line PromQL template in seconds, using `$namespace`, `$workload`, or `$container`, for example a p95 from an HTTP server or service-mesh histogram. Enables latency correlation. |
| `KUBERIGHT_REQUEST_RATE_QUERY` | One-line PromQL template in requests per second, with the same placeholders, for example `sum(rate(http_requests_total{namespace="$namespace",service="$workload"}[5m]))`. Enables traffic correlation. |
| `KUBERIGHT_VERIFICATION_INTERVAL` | How often each API pod verifies applied recommendations (default `15m`, between `1m` and `24h`). `0` disables the background check; the verification endpoint still works on demand. Needs Prometheus and the in-cluster inventory. |
| `KUBERIGHT_WEBHOOK_URL`, `KUBERIGHT_WEBHOOK_SECRET`, `KUBERIGHT_WEBHOOK_FORMAT` | Sends `recommendation.created`, `.approved`, `.rejected`, and `.regressed` events as JSON or Slack messages. With a secret, each request carries `X-KubeRight-Signature: sha256=HMAC(secret, timestamp + "." + body)` and `X-KubeRight-Timestamp`. With `database.enabled`, events are stored in a PostgreSQL outbox before delivery, retried with backoff for about four hours, and dead-lettered when rejected. `GET /api/v1/notifications?status=dead` lists them and `POST /api/v1/notifications/{id}/replay` queues one again; `X-KubeRight-Delivery` stays the same across retries. |
| `KUBERIGHT_EXPORT_RECOMMENDATION_METRICS` | Adds per-container current and recommended request and limit gauges, review status, and savings to `/metrics`. Off by default because `/metrics` is unauthenticated. |

### Integrations and platforms

- **Node autoscalers:** reads Karpenter NodePools (consolidation policy, `consolidateAfter`, disruption budgets) and detects Cluster Autoscaler to explain how node savings are realized. KubeRight never changes NodePools.
- **Observability:** Prometheus text exposition for Grafana, Coralogix, Datadog, New Relic, and OpenMetrics-capable scrapers, plus correlation with your own latency series. Per-recommendation gauges are opt-in because `/metrics` is public.
- **Change delivery:** strategic-merge patches for Argo CD, Flux, kustomize, or `kubectl patch`, and signed webhooks for Slack, Microsoft Teams workflows, PagerDuty, Argo Events, or CI.
- **Autoscaler awareness:** HPA, KEDA, and VPA settings are read and respected per container.
- **Platforms:** linux/amd64 and linux/arm64 images; profiles for production, OpenShift, and cluster-wide inventory; spot capacity labels recognized on EKS, GKE, AKS, and Karpenter. Pods default to `kubernetes.io/os: linux` so mixed Windows and Linux clusters schedule them correctly. The API binary also builds and runs on Windows and macOS for development.

Generating recommendations from Prometheus requires the in-cluster inventory, which provides each container's current identity, requests, and limits. The Helm chart exposes these settings as `database`, `prometheus`, `recommendations`, `forecasting`, `cost`, `latencyQuery`, `requestRateQuery`, and `verification.interval` values. It never deploys PostgreSQL or Prometheus. It refuses `replicaCount > 1` or API autoscaling unless `database.enabled=true`. The production-shaped profile enables two API replicas and an HPA, so it requires a pre-created external PostgreSQL Secret, a safe migration credential, and environment-specific topology, network, TLS, backup, and monitoring configuration.

For Kubernetes installation, required RBAC, immutable-image guidance, and platform notes, use the [installation guide](docs/installation.md).

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

Verification starts only when every live pod runs the approved values. A matching workload template is not enough: during a rollout, in an `OnDelete` workload, or while an in-place resize is pending, pods still run the previous resources. Until they change, the status is `not_applied` and the reason gives the number of pods that are behind.

Severe regressions are caught sooner. During the first hour, the verifier rechecks each change every `verification.earlyInterval` (two minutes by default), starting five minutes after the change is first observed. An OOM kill, a restart, or throttling or CPU or memory pressure that reaches 25% and rises by at least 20 points marks the change regressed. The evidence must cover exactly the live pods. A regression sends the `recommendation.regressed` webhook and raises the `KubeRightRecommendationRegressed` alert. KubeRight does not change the workload: restoring the previous values is up to you or your GitOps process.

The background verifier exports these metrics:

- `kuberight_recommendation_verifications{status}`;
- `kuberight_recommendation_regressions`.

It sends one `recommendation.regressed` webhook per regression. With `metrics.prometheusRule.enabled=true`, the chart adds the `KubeRightRecommendationRegressed` alert.

### Historical attribution and replica coverage

The live pod list describes the workload *now*. A window that spans a rollout, a scale-down, or a replaced StatefulSet pod contains pods that no longer exist. Attributing only live pods would drop their history, and with it the busiest replica's peak or an OOM kill on a replaced pod.

KubeRight reconstructs the population for every step from kube-state-metrics:

1. `kube_pod_info` `created_by` is joined to `kube_replicaset_owner` (Deployments) or `kube_job_owner` (CronJobs).
2. Every pod and intermediate owner must have been created at or after the current workload's creation (`kube_pod_created`, `kube_replicaset_created`, `kube_job_created`). Pods of an earlier same-name workload are therefore excluded.
3. Steps where one pod name maps to two UIDs are excluded. These are the overlap when a StatefulSet pod is replaced.

The workload series is the busiest attributed replica at each step. Restart, OOM, throttling, and pressure evidence covers replaced pods too.

Coverage is time-aware. At each step the expected population is the larger of `spec.replicas` history and the attributed running pods; scaled-to-zero steps are excluded. Evidence is complete only when all of the following hold:

- CPU, memory, and attributed identity cover at least 90% of the expected replica-time.
- The desired replica count is recorded for 90% of active steps.
- Samples cover 80% of the requested window.
- Live pods agree with kube-state-metrics.

Three expected replicas with one reporting is 33%. Two replicas that report alternately cover 50%, even though their timestamps cover the whole window. The evidence view lists the time ranges below 90%.

Post-change verification uses only the pods that exist after the change. `ResourceChangedAt` dates the last request or limit change from `kube_pod_container_resource_requests` and `kube_pod_container_resource_limits`, which starts the stability cooldown.

These queries are evaluated by a real Prometheus in CI. `scripts/attribution-replay.sh` backfills a deterministic fixture into a throwaway Prometheus with `promtool` and runs 13 lifecycle scenarios against it:

- rollout with an OOM in the replaced revision
- 6→2 scale-down
- one of three replicas reporting
- disjoint reporting
- same-name recreation
- StatefulSet replacement
- missing kube-state-metrics
- autoscaling
- scale to zero
- CronJob runs
- resource-change dating
- live-pods mode

```sh
PROMETHEUS_BIN=/path/to/prometheus PROMTOOL_BIN=/path/to/promtool scripts/attribution-replay.sh
```

### Stability

Each recommendation records a `stability` section:

- **Minimum meaningful change** (`minimumChangeRatio`, default 5%, also at least 10m CPU or 16Mi memory): smaller changes are left unchanged. A request and its limit are treated as a pair, so a Guaranteed pod stays Guaranteed. A small increase is kept when OOM kills, throttling, or a pinned limit make it matter.
- **Cooldown** (`cooldownHours`, default 24): reductions wait after the container's requests or limits last changed, so usage under the new configuration is measured first.
- **Oscillation**: a reduction is held when recent recommendations (14 days) alternated between increases and reductions twice.
- **Confirmation** (`requiredObservations`, default 1, conservative profile 2): a reduction is offered only after that many consistent observations at least an hour apart, against the same current values.

When nothing changes, the verdict is `no_change`. When reductions are held and nothing else changes, it is `hold`, with the reasons.

### Benchmarks

The datasets are synthetic and reproducible, generated from a fixed seed, and the results come from those datasets only. They are not a measurement of accuracy on your clusters. Run them yourself:

```sh
go run ./cmd/kuberight benchmark            # optimizer: JSON report with methodology
go run ./cmd/kuberight benchmark --forecast-traces FILE # forecasting engine on a measured trace
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
| P95, no margin | 11 | 8 | 8 | 8 | 56.2% | 45.2% |
| VPA-style (P90 + 15%) | 11 | 0 | 0 | 0 | 51.5% | 37.6% |
| Peak + 10% | 11 | 0 | 0 | 0 | 34.4% | 37.9% |

- **Savings cap:** the balanced policy reduces at most 20% per change, so KubeRight saves less per step than the uncapped VPA-style strategy on these datasets.
- **Withheld change:** it made no change on the incomplete-metrics dataset (`insufficient_data`).
- **Increase:** it raised requests on the CronJob dataset, whose busy hours exceed the current settings.

**Real production traces.** `kuberight benchmark --traces FILE` runs the same strategies on measured usage. The published run uses the Bitbrains GWA-T-12 fastStorage trace. It holds 30 days of 5-minute CPU and memory usage from 1,250 business-critical enterprise VMs, taken from the [GIFT-Eval](https://huggingface.co/datasets/Salesforce/GiftEval) redistribution. `scripts/convert-bitbrains-trace.py` converts it into the trace format.

- **Scope:** 945 VMs with measurable CPU use, giving 2,809 evaluations.
- **Method:** rolling origin. Each strategy sizes from 7 days and is scored on the next 7 unseen days, three times per VM.
- **Current settings:** the VM's provisioned CPU is the current request and limit. The trace has no provisioned memory, so current memory is assumed to be 1.5× the training peak, and memory savings are not reported.
- **Confidence:** intervals are 95% bootstrap intervals over VMs.

| Strategy | Regressions | False positives | CPU under-provisioned | Mean CPU savings |
| --- | ---: | ---: | ---: | ---: |
| KubeRight balanced | **1.9%** (1.5–2.4) | 9.3% (8.2–10.2) | 0.6% | 3.5% (1.5–5.3) |
| KubeRight aggressive | 2.4% (1.9–3.0) | 9.4% (8.4–10.4) | 0.8% | 20.1% (18.1–22.0) |
| Peak + 10% | 18.3% (16.9–19.7) | 26.4% | 1.3% | 60.2% |
| P95, no margin | 39.0% (37.2–40.7) | 52.0% | 42.9% | 77.2% |
| VPA-style (P90 + 15%) | 86.9% (85.4–88.5) | 91.8% | 26.0% | 75.6% |
| Keep current | 0% | 0% | 12.5% | 0% |

- **A regression** is a new limit breach in the unseen week that the current settings did not have.
- **CPU under-provisioned** means CPU above the request in more than 5% of samples.
- **Baselines:** strategies that size to percentiles of one week save more but regress 10 to 45 times as often. KubeRight saves less per step because a single change is capped (20% balanced, 40% aggressive) and because it raised CPU on VMs that ran at their provisioned capacity.
- **Defect found and fixed:** the first run found that a fixed 10% memory-limit headroom above the training peak let 9.1% of evaluations regress. Next week's memory peaks were 1.2 to 1.5× this week's. The headroom is now three coefficients of variation of the daily peaks, between 10% and 50%. This rule was chosen on the first week of each VM and confirmed on the two held-out weeks (1.7%).
- **Limitations:** these are virtual machines, not containers, and the memory baseline is assumed. Treat the numbers as evidence about the sizing rules, not as a prediction for your clusters.

**Forecast accuracy on real traces.** `kuberight benchmark --forecast-traces FILE` backtests the built-in forecasting engine on hourly means of a measured trace. Each forecast sees only earlier hours and chooses its model and interval exactly as live forecasts do. The published run uses the Bitbrains fastStorage trace: 200 VMs drawn at random, of which 188 were scored (12 had gaps longer than three hours), 3 origins with a 24-hour horizon, 90% intervals, and 95% bootstrap intervals over VMs.

| Resource | Interval coverage (nominal 90%) | Beats seasonal-naive | Median skill | Median WAPE, engine / seasonal-naive |
| --- | ---: | ---: | ---: | ---: |
| CPU | 92.3% (91.0–93.6) | 60% of VMs (53–68) | 0.06 | 14.6% / 12.9% |
| Memory | 92.4% (91.2–93.5) | 81% of VMs (75–86) | 0.20 | 17.2% / 20.9% |

- **Intervals:** conformal quantiles of the chosen model's errors over up to 14 past origins. The first version used interpolated quantiles from 3 origins and covered only 76.7% (CPU) and 82.3% (memory) of held-out hours; this run measured the fix.
- **CPU is the harder case:** the engine beats seasonal-naive on most VMs, but where it loses it loses by more, so its median WAPE is higher.
- **Models chosen:** naive, seasonal profiles, and weekly seasonal-naive were picked most often for CPU; exponential smoothing and seasonal profiles for memory. The full counts are in the JSON report.
- **The engine replaced a separate Python forecasting service.** On a different random sample of 200 VMs from the same trace, that service covered 91.4% (CPU) and 93.1% (memory) and beat seasonal-naive on 23% and 49% of VMs.
- **Sizing:** KubeRight uses a forecast for sizing only when its own held-out coverage is within 10 points of the nominal level, and a forecast can only raise a recommendation.
- **Synthetic check:** on eight noisy synthetic daily-cycle series, the engine's out-of-sample error is 18.7% lower than seasonal-naive on average (`internal/forecast` tests).

The Correlation analysis page runs the same rolling-origin backtest on the selected container's own history.

## Read-only by design

KubeRight observes, recommends, and verifies. It never patches a workload, changes requests or limits, restarts or deletes a pod, scales replicas, or edits an autoscaler, and it never runs commands in your containers. Its ServiceAccount can only `list` and `get` what it inventories, and read metrics (on OpenShift through the read-only `cluster-monitoring-view` role or the namespace's pod metrics). The chart renders no other rules unless you add `rbac.extraRules`. Installing it creates only KubeRight's own Deployment, Service, ServiceAccount, RBAC, and optional Route, Ingress, NetworkPolicy, and monitoring objects.

To act on an approved recommendation, open a [GitOps pull request](#gitops-pull-requests), export the patch, or copy the `kubectl` command from the dashboard. Verification then checks the change once the live pods run it.

Earlier releases had an opt-in apply mode. It was removed:

- `GET /api/v1/automation` and `POST /api/v1/automation:run` answer `410 Gone` (`automation_removed`).
- `KUBERIGHT_OPERATION_MODE=apply` and the `KUBERIGHT_AUTOMATION_*` settings are ignored, with a startup warning. KubeRight runs in `recommend` mode instead.
- The chart rejects `operationMode=apply` and no longer has `automation` values.
- Schema version 8 drops the table that tracked in-place resizes.

## GitOps pull requests

When Argo CD or Flux owns a workload, changing it in the cluster would be reverted, so KubeRight proposes the change in Git instead. Configure a provider:

```sh
kubectl -n kuberight create secret generic kuberight-gitops --from-literal=token=<token with contents and pull-request write>
helm upgrade --install kuberight charts/kuberight --namespace kuberight \
  --set gitops.provider=github --set gitops.repository=acme/deploy --set gitops.token.existingSecret=kuberight-gitops
```

For GitHub Enterprise, set `gitops.apiURL` to `https://<host>/api/v3`. For GitLab, use `gitops.provider=gitlab`; it opens merge requests, and repositories may be nested group paths. For self-managed GitLab, set `gitops.apiURL` to `https://<host>/api/v4`. For Gitea or Forgejo, use `gitops.provider=gitea` and set `gitops.apiURL` to `https://<host>/api/v1`.

Then annotate each workload with the file that defines it:

```sh
kubectl -n shop annotate deploy checkout kuberight.io/gitops-path=apps/shop/checkout.yaml
# optional: kuberight.io/gitops-repository=acme/deploy kuberight.io/gitops-base-branch=main
```

For a workload deployed from a Helm chart, point the path at the values file and name the key that holds the container's resources:

```sh
kubectl -n shop annotate deploy checkout kuberight.io/gitops-path=charts/shop/values-prod.yaml \
  kuberight.io/gitops-values-key=api.resources
```

The mapping that holds the key must exist. A missing, null, or empty (`{}`) resources value is replaced with a block, and an existing block is edited in place.

For an approved recommendation, the drawer's **Open pull request** button (or `POST /api/v1/recommendations/{id}/pull-request`) does the following:

1. Reads the file from the base branch.
2. Changes only the container's requests and limits, at their exact positions. Comments, quoting, ordering, and other documents in the file are kept. Missing keys are inserted with matching indentation.
3. Commits the change to the branch `kuberight/<id>`.
4. Opens a pull request with a current-versus-recommended table.

Opening it again returns the same pull request. The request is refused when:

- the recommendation is not approved, or the workload has changed since;
- the workload has no annotation;
- the manifest already has the values;
- the YAML layout cannot be edited in place.

Every attempt is audited.

## Access

KubeRight has no sign-in, like [Goldilocks](https://github.com/FairwindsOps/goldilocks): no token, login page, OAuth proxy, or sidecar. Open it with a port-forward:

```sh
kubectl -n kuberight port-forward svc/kuberight 8080:8080
```

Then browse to `http://127.0.0.1:8080/`. Every request is served as the anonymous identity `system:anonymous`, which audit events and reviews record.

- **Who can use it is decided by the network.** Anyone who can reach the Service, Route, or Ingress can read the cluster inventory and approve or reject recommendations. KubeRight never changes workloads, but keep it on a port-forward, or restrict the Route or Ingress with the NetworkPolicy, an IP allowlist, or your own authenticating ingress.
- **What KubeRight can see is decided by its ServiceAccount.** `rbac.scope=namespace` (the default) shows one namespace; `rbac.scope=cluster` shows every namespace.
- **Upgrades from releases with sign-in.** The `auth`, `approvals`, and `rbac.authReviews` values are accepted and ignored, and the `KUBERIGHT_AUTH_MODE`, `KUBERIGHT_API_TOKEN`, `KUBERIGHT_OIDC_*`, `KUBERIGHT_TOKEN_AUDIENCE`, and `KUBERIGHT_REQUIRE_DISTINCT_APPROVER` settings are logged at startup as removed. The oauth-proxy and oauth2-proxy sidecars, the cookie-secret init container, and the TokenReview and SubjectAccessReview RBAC are no longer rendered.

### Serving under a URL prefix

To share a host with other tools, set `basePath` (`KUBERIGHT_BASE_PATH`), for example `/kuberight`, and route that path to KubeRight without rewriting it:

- **Ingress:** set the host's path to `/kuberight` with `pathType: Prefix`.
- **OpenShift Route:** the chart sets the Route's `path` from `basePath`.

The dashboard, API, icons, and manifest are then served under `/kuberight/`, and `/kuberight` redirects there. KubeRight writes the prefix into the page's `<base href>`, so every asset URL is relative to it. A proxy that strips the prefix also works, because unprefixed paths are still served, and the probes keep using `/healthz` and `/readyz`.

### Browser icons

The dashboard ships its own icon set: `favicon.ico` (16, 32, and 48 px), 16 and 32 px PNGs, `favicon.svg`, a 180 px Apple touch icon, 192 and 512 px Android icons, and `site.webmanifest`. They are served with their own media types, and cached for an hour, so a new icon shows after a cache refresh. Content-hashed scripts and styles under `/assets/` are cached for a year. A request for a missing file, such as an icon size that does not exist, gets a 404 rather than the dashboard page. To change the mark, edit `web/public/favicon.svg` and run `npm run brand:assets` in `web/`.

## Data safety

- **Upgrades:** every schema version since the first is upgraded to the current one with data in place, and every down migration is applied in reverse and re-applied, in the PostgreSQL test suite.
- **Backups:** back up the database with `pg_dump -Fc`. A live restore with `pg_restore` into a new database reproduced every table byte for byte (row counts and content hashes). A second KubeRight release pointed at the restore served the same recommendations and audit trail, and the audit log stayed append-only.

## Cost and OpenCost

Set `cost.opencost.url` (for example `http://opencost.opencost.svc:9003` with `cost.opencost.allowHTTP=true`) to price each namespace from the OpenCost allocation API. KubeRight takes the effective unit prices from the last 7 days: `cpuCost / cpuCoreHours` and `ramCost / ramGiBHours`. These reflect the cluster's cloud or custom pricing.

When a namespace has no priced allocation, OpenCost's cluster-wide blend is used, then `cost.cpuCoreHour` and `cost.memoryGiBHour`. Allocations are cached for 15 minutes and bounded in size. Implausible unit prices are rejected, and negative allocations, which OpenCost reports for pods that just started, are ignored.

Each savings item reports `priceSource` as `opencost`, `opencost-cluster`, or `configured`. Estimates remain request-based: realized savings stay unavailable without billing evidence and a counterfactual.

## Monitoring

`metrics.serviceMonitor.enabled` and `metrics.prometheusRule.enabled` add a ServiceMonitor and alerts: unavailable replicas, a 5xx ratio above 5%, and a regressed recommendation.

`metrics.grafanaDashboard.enabled=true` adds a ConfigMap labeled for the Grafana dashboard sidecar. Set `metrics.grafanaDashboard.namespace` if the sidecar watches only Grafana's namespace, or import [the dashboard](charts/kuberight/dashboards/kuberight.json) by hand. It shows:

- API rate, 5xx share, and mean latency.
- Post-change verification outcomes and recommendations by risk.
- Estimated savings, which needs `exportRecommendationMetrics`.
- Webhook delivery.

Status colors carry meaning only: green is good, yellow and orange are warnings, red is a failure, and gray is a neutral state such as pending.

Every counter exports all of its label combinations from zero, because `increase()` cannot see an increment that creates a series. The per-recommendation series carry the workload's own `namespace` and `container` labels. The dashboard selects them by scrape target and works with `honor_labels: true` as well as with the `exported_*` renaming a ServiceMonitor applies.

`scripts/verify-monitoring.sh` renders the rules, runs promtool unit tests against them, parses every dashboard query, and evaluates the savings queries under both label layouts. CI runs it.

## Live validation

The chart was installed on a two-node kind cluster running Kubernetes 1.35, with Prometheus 3.16, kube-state-metrics 2.20, OpenCost 1.121, PostgreSQL 18, and Grafana 12.3. The run covered these workloads:

- A Deployment scaled from 6 to 2 replicas.
- Two rollouts.
- A StatefulSet pod recreated under the same name.
- An over-requested Deployment.

| Area | Result |
| --- | --- |
| Attribution | Every pod, including the deleted ones, was attributed to its workload and revision, and each StatefulSet pod incarnation was kept apart by UID. CPU and memory replica-time coverage was 97–99%, and every workload's coverage was complete. |
| Approval | With the since-removed sign-in, the creator could not approve their own recommendation, and a second identity approved it. That run also exercised the since-removed apply mode: the background pass patched the Deployment under the `kuberight-automation` field manager with exactly the recommended values and did not reapply. On PostgreSQL, two approved changes were applied in one pass under the advisory lock. |
| Verification | The applied change was `verified` from exact pod-UID evidence over 3h20m: no OOM kills, restarts, or throttling. A follow-up reduction for the same workload was held by the 24-hour cooldown, detected from Prometheus resource history. |
| Cost | Savings were priced from OpenCost's per-namespace allocation (`priceSource: opencost`). |
| Dashboards | Grafana loaded the chart's dashboard ConfigMap with no panel errors. |
| Load and concurrency | Three replicas on PostgreSQL were driven by fortio inside the cluster at 24 connections: 224 requests per second on the recommendations list, with a 93 ms median and 352 ms p99. Twelve concurrent creates with one Idempotency-Key produced one record. Eight concurrent automation runs applied a change once, and the other seven were refused by the lock. |
| Accessibility | axe-core (WCAG 2.0/2.1 A and AA) on all 20 views against live data, at 1440 and 390 px: no violations, no horizontal overflow, no console errors. |
| Backup and restore | The `pg_dump`/`pg_restore` restore was identical to the source on every table. A second release served the same recommendations and audit trail from it. |
| GitOps | Against a real Gitea, an approved recommendation opened a mergeable pull request. The diff changed only the four resource values and kept the comments and the second document. A repeated request returned the same pull request. |
| Single sign-on (since removed) | Through Dex and the chart's oauth2-proxy sidecar, a browser signed in and used the API under `oidc:alice@example.com`. A write sent with forged `X-Forwarded-*` headers for another user and `system:masters` was still recorded as alice's. Requests without a session got 401. |
| OpenShift conditions | Under Pod Security `restricted`, with arbitrary UIDs (1000680000 and 1000680001, GID 0), read-only root filesystems, and OpenShift's Route CRD: the API pod and the then-separate forecasting pod ran, and the Route validated. This was a simulation on kind, not OpenShift. |

Real infrastructure found 13 defects that unit tests had not caught, and all are fixed:

- Deleted pods' cAdvisor series lingered and were flagged as foreign usage.
- A failed OpenCost query was cached for 15 minutes and shown as $0.00.
- Fleet scans could not complete on PostgreSQL. The first run of the store contract suite against PostgreSQL found this.
- A one-step CPU gap after node restarts blocked every recommendation.
- Counters that first appeared at 1 hid the first failure from alerts.
- The dashboard's savings panels assumed one scrape label layout.
- Uncached authentication capped the API at 5 requests per second.
- A cooldown was missed when the analysis window was shorter than the cooldown.
- Empty PostgreSQL pages were returned as `null` and broke the Fleet view.
- Serious WCAG violations appeared on 19 of 20 views.
- The memory-limit headroom was too small for real week-to-week memory growth. The [real-trace benchmark](#benchmarks) found this.
- OIDC mode lacked the RBAC for `SubjectAccessReview`.
- Policy bounds that a browser cannot represent made every dashboard policy edit fail.

These were functional runs on a small cluster. Accuracy is evaluated separately, on [public production traces](#benchmarks).

## Troubleshooting

**Why is there only one KubeRight pod?** The API, dashboard, recommendation engine, diagnostics, and forecasting all run in one Deployment with one container, on OpenShift too. There is no separate forecasting or ML service, and no agent or exporter. KubeRight reads the Kubernetes API and an existing Prometheus.

**No recommendations?**

1. Open **Integrations**. Prometheus must be healthy, and **Prometheus metric coverage** must show CPU usage, memory usage, and kube-state-metrics as available: without kube-state-metrics, usage cannot be attributed to workloads. On OpenShift, KubeRight finds Thanos Querier itself and needs the monitoring permission the OpenShift profile grants (`prometheus.openshiftMonitoring`). Elsewhere, set `prometheus.url`.
2. Open **Fleet operations**. A default scan runs at startup and every 6 hours (`defaultScanInterval`). A failed scan shows its error there.
3. A recommendation with the verdict `insufficient_data` names what is missing: usually too little history, replicas without metrics, or a recent restart or rollout. KubeRight holds advice rather than guessing.
4. A namespace-scoped install sees only its own namespace. Use the cluster-wide manifest or `rbac.scope=cluster` to see every namespace.

**No forecast?** A forecast needs at least 48 samples with no gap longer than three steps, and no fewer samples than the horizon. That is 48 hours for hourly forecasts and 48 days for 30- and 90-day forecasts. The error says how many samples exist. On OpenShift, platform Prometheus keeps 15 days by default.

**A shell for debugging.** The production image has no shell. Build the debug variant, which adds a BusyBox shell (`/busybox/sh`) and still runs as non-root, with `docker build --target debug -t <registry>/kuberight:<version>-debug .`. Then attach it to the running pod without restarting it, or start a copy on OpenShift:

```sh
kubectl debug -n kuberight -it deploy/kuberight --image=<registry>/kuberight:<version>-debug --target=kuberight -- /busybox/sh
oc debug -n kuberight deploy/kuberight --image=<registry>/kuberight:<version>-debug -- /busybox/sh
```

Ephemeral containers need permission to update `pods/ephemeralcontainers`. Never run the debug image as the regular Deployment.

## Repository map

| Path | Purpose |
| --- | --- |
| `cmd/kuberight`, `internal/` | Go API, inventory, Prometheus usage, forecasting engine, recommendations, reviews, and storage |
| `internal/usage/attribution.go` | Historical pod attribution and time-aware replica coverage |
| `internal/optimizer/stability.go`, `runtime.go` | Hysteresis, cooldown, oscillation, and confirmation controls; JVM memory rules |
| `scripts/attribution-replay.sh` | Real-Prometheus replay of the attribution queries |
| `internal/gitops/` | In-place manifest editing and the GitHub, GitLab, and Gitea/Forgejo providers |
| `internal/benchmark/traces.go`, `scripts/convert-bitbrains-trace.py` | Real-trace evaluation with rolling-origin folds and bootstrap intervals |
| `scripts/verify-monitoring.sh`, `charts/kuberight/dashboards/` | promtool tests for the alerts and dashboard queries; the Grafana dashboard |
| `internal/repository/postgres/migrations/` | Embedded, checksummed PostgreSQL schema migrations |
| `web/` | React and TypeScript dashboard |
| `charts/kuberight/` | Helm chart and profiles |
| `deploy/manifests/` | Generated standalone manifests; do not edit by hand |
| `api/openapi.yaml` | Go API contract artifact; it is not served by the process |
| `scripts/check-openapi-compat.py` | Reports breaking `/api/v1` changes between two versions of the contract |
| `.github/workflows/` | Required quality gates and immutable tag release workflow |

## API compatibility

`/api/v1` changes are additive within a major version: new endpoints, new optional parameters and fields, and new enum values that clients can ignore. Removing or renaming an operation, parameter, schema, field, or enum value, making an optional parameter or field required, or changing a field's type needs a new API version. The older version is then served for at least two minor releases. Before 1.0.0, a minor release may still break the API, and its release notes list each break. To check a change against the last release:

```sh
git show v0.1.0:api/openapi.yaml > /tmp/base.yaml
scripts/check-openapi-compat.py /tmp/base.yaml api/openapi.yaml
```

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
