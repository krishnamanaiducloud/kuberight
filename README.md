# KubeRight

KubeRight is a Kubernetes resource-inventory and right-sizing project. It combines a Go API, a React dashboard, an optional Python forecasting service, and a Helm chart.

> **Development preview:** this repository is not a verified production release. A source checkout, chart version, or Dockerfile is not evidence that an image has been published or passed the required release gates. Verify a signed release and its digest before deploying.

## What works today

- Namespace-scoped or cluster-scoped inventory of Kubernetes workloads and autoscalers. It is read-only unless you enable [continuous rightsizing](#continuous-rightsizing).
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
- A review workflow: recommendations are approved or rejected, individually or in bulk, with optional separation of duties, and every decision is audited. Prometheus-generated advice carries workload/specification identity, observation/query/policy provenance, and a 24-hour expiry; approval fails if that advice is expired or no longer matches the live workload and resources. Approval records a decision. Changes are applied only by opt-in [continuous rightsizing](#continuous-rightsizing). Otherwise the dashboard shows a `kubectl` command or a strategic-merge YAML patch for each approved change, and exports approved changes as a patch file for GitOps or `kubectl patch`.
- Durable PostgreSQL storage for recommendations, reviews, idempotency keys, and audit events. Embedded checksummed migrations run transactionally under an advisory lock; the chart uses a bounded pre-install/pre-upgrade migration Job while serving pods verify the exact schema without DDL. An append-only trigger rejects audit-table mutation. Without a database, a bounded in-process store is used.
- Forecasts from the forecasting service using Prometheus history, with confidence intervals and model diagnostics.
- Request-based monthly cost estimates priced per namespace from [OpenCost](#cost-and-opencost) when configured, or from operator-configured unit prices. Every savings item names its price source. Node impact keeps three amounts apart: the request reduction, the feasible savings from removing the nodes the packing frees, and the bill, which KubeRight does not measure.
- Persistent scoped policies (global, namespace, workload, container) with deterministic precedence. A cluster-specific scope wins over an all-clusters scope at the same level, then higher priority wins. Duplicate enabled scopes with equal priority are rejected, and resolution lists every applicable policy in order.
- Authentication:
  - Bearer tokens checked with Kubernetes `TokenReview`.
  - [Browser single sign-on](#single-sign-on) with any OIDC provider.
  - A development-only static token.
- Every request is authorized with `SubjectAccessReview`. Verified identities and decisions are cached briefly; see [Single sign-on](#single-sign-on).
- [GitOps pull requests](#gitops-pull-requests): an approved recommendation can be proposed as a pull request (GitHub, Gitea/Forgejo) or merge request (GitLab) that edits the workload's manifest.
- A dashboard backed by the versioned Go API. It passes an automated WCAG 2.1 AA audit on every view. Views whose integrations are unavailable display that state instead of fabricated data.
- Secure-by-default workload settings in a Helm chart, plus development, production-shaped, OpenShift, and cluster-wide rendering profiles.

KubeRight does **not**:

- ingest cloud billing or provide a complete FinOps ledger;
- register remote clusters.

It never changes workloads unless `operationMode=apply` is set and a namespace opts in. With the PostgreSQL store, webhook delivery is durable; without it, the delivery queue is process-local. Persisted ML models are signed, and the forecasting service refuses any model file whose signature does not verify. See [known limitations](docs/known-limitations.md) before evaluating it for production.

## Quick start from source

Prerequisites are Go 1.27.4, Node.js 24, and npm. This local mode disables in-cluster inventory and uses a static token; it is for development only.

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
| `KUBERIGHT_WEBHOOK_URL`, `KUBERIGHT_WEBHOOK_SECRET`, `KUBERIGHT_WEBHOOK_FORMAT` | Sends `recommendation.created`, `.approved`, `.rejected`, and `.regressed` events as JSON or Slack messages. With a secret, each request carries `X-KubeRight-Signature: sha256=HMAC(secret, timestamp + "." + body)` and `X-KubeRight-Timestamp`. With `database.enabled`, events are stored in a PostgreSQL outbox before delivery, retried with backoff for about four hours, and dead-lettered when rejected. `GET /api/v1/notifications?status=dead` lists them and `POST /api/v1/notifications/{id}/replay` queues one again; `X-KubeRight-Delivery` stays the same across retries. |
| `KUBERIGHT_EXPORT_RECOMMENDATION_METRICS` | Adds per-container current and recommended request and limit gauges, review status, and savings to `/metrics`. Off by default because `/metrics` is unauthenticated. |

### Integrations and platforms

- **Node autoscalers:** reads Karpenter NodePools (consolidation policy, `consolidateAfter`, disruption budgets) and detects Cluster Autoscaler to explain how node savings are realized. KubeRight never changes NodePools.
- **Observability:** Prometheus text exposition for Grafana, Coralogix, Datadog, New Relic, and OpenMetrics-capable scrapers, plus correlation with your own latency series. Per-recommendation gauges are opt-in because `/metrics` is public.
- **Change delivery:** strategic-merge patches for Argo CD, Flux, kustomize, or `kubectl patch`, and signed webhooks for Slack, Microsoft Teams workflows, PagerDuty, Argo Events, or CI.
- **Autoscaler awareness:** HPA, KEDA, and VPA settings are read and respected per container.
- **Platforms:** linux/amd64 and linux/arm64 images; profiles for production, OpenShift, and cluster-wide inventory; spot capacity labels recognized on EKS, GKE, AKS, and Karpenter. Pods default to `kubernetes.io/os: linux` so mixed Windows and Linux clusters schedule them correctly. The API binary also builds and runs on Windows and macOS for development.

Generating recommendations from Prometheus requires the in-cluster inventory, which provides each container's current identity, requests, and limits. The Helm chart exposes these settings as `database`, `prometheus`, `recommendations`, `forecasting`, `cost`, `approvals`, `latencyQuery`, `requestRateQuery`, `verification.interval`, and `ml.apiKey` values. It never deploys PostgreSQL or Prometheus. It refuses `replicaCount > 1` or API autoscaling unless `database.enabled=true`, and rejects ML scaling while model storage remains local. The production-shaped profile enables two API replicas and an HPA, so it requires a pre-created external PostgreSQL Secret, a safe migration credential, and environment-specific topology, network, TLS, backup, and monitoring configuration.

For Kubernetes installation, required RBAC, immutable-image guidance, and platform notes, use the [installation guide](docs/installation.md). In Kubernetes authentication mode, users also need RBAC for the virtual `kuberight.io` resources. Reviewing requires `update` on `recommendations/approval`, generating forecasts requires `create` on `forecasts`, the efficiency views require `list` on `efficiency`, node impact requires `list` on `capacity`, and the webhook outbox requires `list` on `notifications` and `update` on `notifications/replay`:

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

Severe regressions are caught sooner. During the first hour, the verifier rechecks each change every `verification.earlyInterval` (two minutes by default), starting five minutes after the change is first observed. An OOM kill, a restart, or throttling or CPU or memory pressure that reaches 25% and rises by at least 20 points marks the change regressed. The evidence must cover exactly the live pods. In apply mode the change is then rolled back on the same cadence. Rollbacks are not deferred by the change window or the per-run change limit.

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

**Forecast benchmark (`ml/benchmark.py`).**

- **Datasets:** the same 11 archetypes, 336 hourly samples each.
- **Method:** a rolling-origin backtest with 3 origins and a 24-hour horizon. Each forecast sees only earlier samples.
- **Scoring:** 792 held-out samples in total, measured against a seasonal-naive baseline.

Results:

- **Coverage:** mean interval coverage was 94.3% against a nominal 90%. 10 of 11 datasets met the nominal level.
- **Against the baseline:** 7 of 11 beat it (skill > 0). On the GC-sawtooth, KEDA-scaled, and CronJob datasets the service selected the seasonal-naive model itself.
- **Weak case:** a level shift (the recent-rollout dataset) reached 75% coverage and was worse than the baseline. KubeRight therefore sizes from post-rollout samples only, shortens validity to 6 hours, and uses a forecast for sizing only when it is calibrated.

**Forecasts on real traces.** `python -m ml.benchmark --traces FILE --limit 200` runs the same backtest on hourly means of the Bitbrains trace: 200 random VMs, 3 origins with a 24-hour horizon, and 95% bootstrap intervals over VMs.

- **Memory:** intervals covered 90.5% (89.0–91.9) of held-out hours against a nominal 90%. The service beat seasonal-naive on 49% of VMs.
- **CPU:** intervals covered 86%, because the calibration factor was fitted on the last validation fold only. It is now the largest factor any fold needs. Chosen on one set of 100 VMs and confirmed on a different 100, coverage rose to 88.4% (85.0–91.2). The remaining shortfall comes from VMs whose CPU level shifts abruptly; there the service usually selects the seasonal-naive model itself (median skill 0). KubeRight uses a forecast for sizing only when its own calibration check passes.

**Quantile accuracy.** `scripts/benchmark-quantile.py FILE --limit 200` scores quantile forecasts on the same VMs, origins, and horizon. It compares seasonal-naive with empirical quantiles, the forecasting service, and LightGBM quantile regression with and without split-conformal calibration. LightGBM is a benchmark-only dependency. The score is pinball loss scaled by each VM's mean, averaged over 195 VMs with 95% bootstrap intervals.

| P95 forecast | CPU coverage | CPU pinball | Memory coverage | Memory pinball |
| --- | ---: | ---: | ---: | ---: |
| Seasonal-naive | 95.1% | 0.132 | 94.6% | 0.086 |
| Forecasting service (Ridge) | 96.0% | 0.211 (0.131–0.328) | 96.5% | 0.134 |
| LightGBM, conformal | 91.0% (90.0–92.1) | 0.102 (0.066–0.152) | 91.9% | 0.072 |

- The service's P95 forecast is less accurate than seasonal-naive on most VMs: it wins on 40% for CPU and 37% for memory. Its upper bound covers enough hours, but it is wider or more biased than it needs to be. Its P50 wins on 38% of VMs for CPU and 53% for memory.
- LightGBM with conformal calibration has the lowest P95 loss and beats seasonal-naive on 73% (CPU) and 72% (memory) of VMs. It under-covers, though: 91–92% at P95 and 96–97% at P99, so it is not yet safe to size from.
- Seasonal-naive is the best calibrated: 95% at P95 and 98.6–98.7% at P99.

`POST /api/v1/backtest` on the forecasting service runs the same rolling-origin evaluation on your own history. The Correlation analysis page shows its result for the selected container.

## Continuous rightsizing

With `operationMode=apply`, KubeRight applies approved recommendations itself. It evaluates them every `automation.interval` (default 15m), and `GET /api/v1/automation` shows the plan in any mode without writing. A change is made only when all of the following hold:

- The namespace is labeled `kuberight.io/automation=enabled` and the workload is not annotated `kuberight.io/automation=disabled`.
- The workload is a Deployment, StatefulSet, or DaemonSet that Argo CD or Flux does not manage. Annotate `kuberight.io/automation-gitops=allow` only if the GitOps source will not revert the change.
- The recommendation is approved, its verdict is `recommend`, and its evidence is unexpired and covered every replica.
- The workload UID, generation, spec fingerprint, and current requests and limits still match what the recommendation was calculated against.
- No VPA manages the container.

Each change is a strategic-merge patch of one container's requests and limits:

- It is owned by the `kuberight-automation` field manager.
- A `resourceVersion` precondition rejects it if the workload changed after evaluation.
- It is annotated with the recommendation and the exact previous resources.

At most `automation.maxChangesPerRun` changes (default 5) run per pass, inside the optional daily UTC `automation.window`. Rollbacks go first. When post-change verification reports a regression, KubeRight restores the previous values, provided nobody changed them since.

Every attempt is audited, and `recommendation.applied` and `recommendation.rolled_back` webhooks are sent. `POST /api/v1/automation:run` runs a pass on demand and requires `update` on the virtual `automation` resource.

Applying passes are singletons across API replicas. A PostgreSQL advisory lock (or a process lock for the in-memory store) is held for the pass, and a concurrent `POST /api/v1/automation:run` returns `409 automation_busy`. `/metrics` exports `kuberight_automation_changes_total{action,outcome}`, `kuberight_automation_eligible_changes`, `kuberight_automation_last_pass_timestamp_seconds`, and `kuberight_automation_interval_seconds`. With `metrics.prometheusRule.enabled=true`, apply mode adds alerts for failed changes, rollbacks, and a stalled automation loop.

Apply mode requires `auth.mode=kubernetes` and `rbac.scope=cluster`. It adds only `patch` on those three kinds.

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

The permission is `create` on `recommendations/pullrequest` in the `kuberight.io` group.

## Single sign-on

`auth.mode=oidc` adds an [oauth2-proxy](https://oauth2-proxy.github.io/oauth2-proxy/) sidecar for browser sign-in with Entra ID, Okta, Google, Keycloak, Dex, or any other OIDC provider:

```sh
kubectl -n kuberight create secret generic kuberight-oidc \
  --from-literal=client-secret=<client secret> \
  --from-literal=cookie-secret="$(openssl rand -base64 32 | tr -- '+/' '-_')"
helm upgrade --install kuberight charts/kuberight --namespace kuberight \
  --set auth.mode=oidc --set auth.oidc.issuerURL=https://login.example.com \
  --set auth.oidc.clientID=kuberight --set auth.oidc.existingSecret=kuberight-oidc \
  --set auth.oidc.redirectURL=https://kuberight.example.com/oauth2/callback
```

- **Trust boundary:** the API listens on loopback only, so the identity headers can come only from the sidecar.
- **Authorization:** every request is still checked with `SubjectAccessReview`. Bind users and groups with the `oidc:` prefixes, for example `--user=oidc:alice@example.com`.
- **Apply mode** accepts OIDC identities, and the creator of a recommendation still cannot approve it.
- **Session cookie:** `HttpOnly`, `Secure`, and `SameSite=Lax`. API calls without a session get `401` instead of a redirect.

In token mode, verified identities are cached for up to 2 minutes (never past the token's expiry), allow decisions for 30 seconds, and denials for 10 seconds. A revoked RoleBinding therefore takes effect within 30 seconds. Under load, three replicas served about 520 requests per second; without the cache, client-side rate limiting had capped them at about 5.

## Data safety

- **Upgrades:** every schema version since the first is upgraded to the current one with data in place, and every down migration is applied in reverse and re-applied, in the PostgreSQL test suite.
- **Backups:** back up the database with `pg_dump -Fc`. A live restore with `pg_restore` into a new database reproduced every table byte for byte (row counts and content hashes). A second KubeRight release pointed at the restore served the same recommendations and audit trail, and the audit log stayed append-only.
- **ML models:** set `ml.persistence.signingKey.existingSecret`, which the chart requires with `ml.persistence.enabled`. Each model file is signed with HMAC-SHA256 and verified before it is loaded. A file that does not verify is never deserialized, and the model is retrained.

## Cost and OpenCost

Set `cost.opencost.url` (for example `http://opencost.opencost.svc:9003` with `cost.opencost.allowHTTP=true`) to price each namespace from the OpenCost allocation API. KubeRight takes the effective unit prices from the last 7 days: `cpuCost / cpuCoreHours` and `ramCost / ramGiBHours`. These reflect the cluster's cloud or custom pricing.

When a namespace has no priced allocation, OpenCost's cluster-wide blend is used, then `cost.cpuCoreHour` and `cost.memoryGiBHour`. Allocations are cached for 15 minutes and bounded in size. Implausible unit prices are rejected, and negative allocations, which OpenCost reports for pods that just started, are ignored.

Each savings item reports `priceSource` as `opencost`, `opencost-cluster`, or `configured`. Estimates remain request-based: realized savings stay unavailable without billing evidence and a counterfactual.

```sh
helm upgrade --install kuberight charts/kuberight --namespace kuberight \
  --set operationMode=apply --set rbac.scope=cluster --set automation.window=22:00-04:00
kubectl label namespace shop kuberight.io/automation=enabled
```

## Monitoring

`metrics.serviceMonitor.enabled` and `metrics.prometheusRule.enabled` add a ServiceMonitor and alerts: unavailable replicas, a 5xx ratio above 5%, a regressed recommendation, and, in apply mode, failed changes, rollbacks, and a stalled automation loop.

`metrics.grafanaDashboard.enabled=true` adds a ConfigMap labeled for the Grafana dashboard sidecar. Set `metrics.grafanaDashboard.namespace` if the sidecar watches only Grafana's namespace, or import [the dashboard](charts/kuberight/dashboards/kuberight.json) by hand. It shows:

- API rate, 5xx share, and mean latency.
- Post-change verification outcomes and recommendations by risk.
- Estimated savings, which needs `exportRecommendationMetrics`.
- Automated changes by outcome, and how many intervals the last pass is behind.
- Webhook delivery.

Status colors carry meaning only: green is good, yellow and orange are warnings, red is a failure, and gray is a neutral state such as pending.

Every counter exports all of its label combinations from zero. `increase()` cannot see an increment that creates a series, so the first failed change still alerts. The per-recommendation series carry the workload's own `namespace` and `container` labels. The dashboard selects them by scrape target and works with `honor_labels: true` as well as with the `exported_*` renaming a ServiceMonitor applies.

`scripts/verify-monitoring.sh` renders the rules, runs promtool unit tests against them, parses every dashboard query, and evaluates the savings queries under both label layouts. CI runs it.

## Live validation

The chart was installed in apply mode on a two-node kind cluster running Kubernetes 1.35, with Prometheus 3.16, kube-state-metrics 2.20, OpenCost 1.121, PostgreSQL 18, and Grafana 12.3. The run covered these workloads:

- A Deployment scaled from 6 to 2 replicas.
- Two rollouts.
- A StatefulSet pod recreated under the same name.
- An over-requested Deployment.

| Area | Result |
| --- | --- |
| Attribution | Every pod, including the deleted ones, was attributed to its workload and revision, and each StatefulSet pod incarnation was kept apart by UID. CPU and memory replica-time coverage was 97–99%, and every workload's coverage was complete. |
| Approval and automation | The creator could not approve their own recommendation. A second identity approved it. The background pass patched the Deployment under the `kuberight-automation` field manager with exactly the recommended values and did not reapply. On PostgreSQL, two approved changes were applied in one pass under the advisory lock. |
| Verification | The applied change was `verified` from exact pod-UID evidence over 3h20m: no OOM kills, restarts, or throttling. A follow-up reduction for the same workload was held by the 24-hour cooldown, detected from Prometheus resource history. |
| Cost | Savings were priced from OpenCost's per-namespace allocation (`priceSource: opencost`). |
| Dashboards | Grafana loaded the chart's dashboard ConfigMap with no panel errors. |
| Load and concurrency | Three replicas on PostgreSQL were driven by fortio inside the cluster at 24 connections: 224 requests per second on the recommendations list, with a 93 ms median and 352 ms p99. Twelve concurrent creates with one Idempotency-Key produced one record. Eight concurrent automation runs applied a change once, and the other seven were refused by the lock. |
| Accessibility | axe-core (WCAG 2.0/2.1 A and AA) on all 20 views against live data, at 1440 and 390 px: no violations, no horizontal overflow, no console errors. |
| Backup and restore | The `pg_dump`/`pg_restore` restore was identical to the source on every table. A second release served the same recommendations and audit trail from it. |
| GitOps | Against a real Gitea, an approved recommendation opened a mergeable pull request. The diff changed only the four resource values and kept the comments and the second document. A repeated request returned the same pull request. |
| Single sign-on | Through Dex and the chart's oauth2-proxy sidecar, a browser signed in and used the API under `oidc:alice@example.com`. A write sent with forged `X-Forwarded-*` headers for another user and `system:masters` was still recorded as alice's. Requests without a session got 401. |
| ML models | A persisted model was reused after its pod was replaced. After one byte was appended to the file on the volume, it was refused before unpickling ("signature does not match") and retrained. |
| OpenShift conditions | Under Pod Security `restricted`, with arbitrary UIDs (1000680000 and 1000680001, GID 0), read-only root filesystems, and OpenShift's Route CRD: both pods ran, the Route validated, and the ML service wrote signed models. |

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

## Repository map

| Path | Purpose |
| --- | --- |
| `cmd/kuberight`, `internal/` | Go API, authentication, inventory, Prometheus usage, forecasting client, recommendations, reviews, and storage |
| `internal/usage/attribution.go` | Historical pod attribution and time-aware replica coverage |
| `internal/optimizer/stability.go`, `runtime.go` | Hysteresis, cooldown, oscillation, and confirmation controls; JVM memory rules |
| `internal/automation/`, `internal/httpapi/automation.go` | Opt-in continuous rightsizing and rollback |
| `scripts/attribution-replay.sh` | Real-Prometheus replay of the attribution queries |
| `internal/gitops/` | In-place manifest editing and the GitHub, GitLab, and Gitea/Forgejo providers |
| `internal/benchmark/traces.go`, `scripts/convert-bitbrains-trace.py` | Real-trace evaluation with rolling-origin folds and bootstrap intervals |
| `scripts/verify-monitoring.sh`, `charts/kuberight/dashboards/` | promtool tests for the alerts and dashboard queries; the Grafana dashboard |
| `internal/repository/postgres/migrations/` | Embedded, checksummed PostgreSQL schema migrations |
| `web/` | React and TypeScript dashboard |
| `ml/` | Independent FastAPI forecasting service |
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
