# Architecture

KubeRight has a Go control-plane service, an embedded React application, and an optional Python forecasting service. PostgreSQL and Prometheus are external dependencies; the chart does not install either one.

```text
Browser
  |
  | bearer token, or OIDC session through oauth2-proxy
  v
Go API and embedded dashboard
  |-- TokenReview / SubjectAccessReview ----> local Kubernetes API
  |-- inventory / opt-in guarded patches ---> local Kubernetes API
  |-- range and instant queries ------------> Prometheus
  |-- durable records (when configured) ----> external PostgreSQL
  |-- namespace prices (when configured) ---> OpenCost
  |-- forecasts (when configured) ----------> Python forecasting service
  |-- provider-backed change proposals ------> GitHub / GitLab / Gitea
  `-- durable lifecycle events --------------> configured webhook

Python forecasting service
  `-- versioned model files under its configured local directory
```

## Go service

The executable in `cmd/kuberight` serves the dashboard and `/api/v1` API on one HTTP listener. Middleware adds request IDs, structured request logs, recovery, time and body limits, security headers, metrics, authentication, and authorization.

In `kubernetes` authentication mode, the service sends the bearer token to the local API server's `TokenReview` endpoint. In `oidc` mode, an oauth2-proxy sidecar authenticates the browser and the API listens only on loopback. Both modes send the requested `kuberight.io` resource and verb to `SubjectAccessReview`. Authentication or authorization dependency failures fail closed. `static` mode uses a single constant-time-compared token and grants full application access; it is a development facility and cannot enable apply mode.

The inventory provider uses in-cluster credentials and live client-go list/get calls. It returns the local cluster, namespaces, workload resource settings, and HPA/KEDA/VPA presence. The default Helm scope restricts inventory to the release namespace. There are no informers, shared inventory caches, remote-cluster clients, or stored inventory snapshots.

When `KUBERIGHT_PROMETHEUS_URL` is configured, the usage source queries cAdvisor-compatible CPU, memory, and CFS throttling series. kube-state-metrics adds restart, OOM, replica, and rollout evidence. Prometheus-backed API routes provide history, fleet efficiency, correlated signals, CPU hotspot ranking, generated recommendations, and input to forecasts. Hotspots use exact Kubernetes pod `ownerReferences`, including ReplicaSet/Job resolution, instead of pod-name heuristics. Queries are bounded by time, point count, response size, timeout, and a per-pod concurrent-request limit (8 by default). Prometheus is an optional enrichment: an outage degrades those routes but does not make the base API unready.

The optimizer is deterministic. It calculates CPU and memory percentiles, confidence, risk, data quality, backtests, trends, and spike behavior, then applies safety margins and bounded reductions. Throttling, OOM kills, sparse or stale data, rollout shifts, autoscalers, and frequent or recurring spikes add safeguards. The optimizer never mutates a Kubernetes object.

Prometheus-generated recommendations carry the observed workload UID, generation, resource version, resource-spec fingerprint, observation window, query and policy fingerprints, optimizer version, and a 24-hour expiry. Immediately before approval, the API rejects expired advice and re-reads the workload to detect recreation, cluster or specification changes, removed containers, and changed requests or limits. Caller-supplied recommendations remain manually reviewable but are not represented as live-workload-verified advice.

Reviews are terminal, single-assignment decisions. Optional separation of duties rejects a creator approving their own recommendation. The recommendation and its audit event are written transactionally. Approval alone never applies resources. In recommend mode the dashboard exports a patch or opens a provider-backed pull request. In explicitly enabled apply mode, a singleton automation pass can patch a fresh approved change only in an opted-in namespace, with a resource-version precondition, audit evidence, post-change verification, and rollback on detected regression.

## Persistence

With `KUBERIGHT_DATABASE_URL`, the Go service stores recommendations, idempotency claims, reviews, policies, fleet scans, verification outcomes, automation resize state, the notification outbox, and audit events in external PostgreSQL. Embedded, checksummed migrations are serialized with a PostgreSQL advisory lock and each pending migration is applied in its own transaction. The chart normally runs this through a bounded pre-install/pre-upgrade hook Job. Serving pods run with migration disabled and verify the exact schema version, migration names, and checksums before starting.

The database migration adds a trigger that rejects `UPDATE`, `DELETE`, and `TRUNCATE` on the audit table. This is a database guard against accidental application mutation, not tamper-proof WORM storage: a sufficiently privileged database operator can alter or disable database objects. The chart should use a short-lived DDL credential for the hook and a separate, restricted DML credential for serving pods.

Without a database URL, the API falls back to a bounded in-process repository. That mode loses all control-plane records on restart and cannot provide coherent state across replicas, so the chart rejects API scaling in that configuration.

## Dashboard

The React dashboard is built into the Go image and served as a same-origin single-page application. It calls only the Go API. Token mode retains an operator-supplied bearer token in browser session storage; OIDC mode uses the proxy session. It presents current resources, historical usage, recommendation evidence and freshness, risk/confidence, backtests, correlated signals, hotspots, policies, fleet scans, approvals, automation plans, savings stages, capacity estimates, and explicit unavailable states instead of fabricated data.

Approved patches can be exported in the browser. When a Git provider and workload target annotation are configured, the API can edit one manifest or Helm-values resource block, commit a dedicated branch, and open an idempotent GitHub, GitLab, Gitea, or Forgejo change request. It does not merge or reconcile that request, render chart templates, or edit multi-file/Kustomize layouts.

## Forecasting service

The FastAPI service accepts a uniformly sampled CPU or memory series, validates time order and bounds, performs time-aware validation, and returns a forecast, intervals, quality signals, anomaly/seasonality/drift summaries, and fallback reasons. The Go API can query Prometheus and call this service through `POST /api/v1/forecasts` when both integrations are configured.

The service can retain versioned model artifacts when a request supplies `series_id` and `persist=true`. With `ml.persistence.enabled`, the chart mounts a persistent volume and requires a signing-key Secret; artifacts are authenticated before deserialization and invalid files are retrained. The store is still single-writer, so ML replicas remain limited to one and ML autoscaling is rejected.

## Deployment and trust boundaries

The Helm chart renders the Go deployment, service account, RBAC, service, probes, disruption budget, NetworkPolicy, optional ingress or edge-terminated OpenShift Route, optional ML deployment, optional monitoring CRs, and an optional PostgreSQL migration hook. It never deploys PostgreSQL or Prometheus. The primary and ML containers are configured as non-root with read-only root filesystems and bounded writable `/tmp` volumes.

The default profile remains a singleton with in-process storage. The production-shaped profile enables external PostgreSQL, two API replicas, an HPA, a PDB, and zone spreading. That topology requires a reachable PostgreSQL service, a pre-created serving Secret (and preferably a separate migration Secret), enough schedulable capacity across the selected topology, and environment-specific network/TLS/monitoring controls. It is an HA-shaped baseline, not evidence of an availability SLO or production certification. The ML service remains a singleton because its artifact store is single-writer.

TLS and external identity-token issuance are outside KubeRight. The ingress, gateway, Route, or service mesh is a trust boundary. The Kubernetes API is a critical dependency for authentication and inventory. `/healthz`, `/readyz`, and `/metrics` are public endpoints and require network protection.

With PostgreSQL, webhook delivery uses a durable outbox, bounded retries, dead-letter state, replay, and retention. Delivery is at least once and may duplicate an event after a crash; the stable delivery ID lets receivers deduplicate. The outbox write is not atomic with every reported state change, so a narrow database-failure window falls back to the bounded in-memory queue. Without PostgreSQL, a full queue or process restart can lose notifications.

## Deliberate boundaries

- The only inventory provider is the local in-cluster provider; there is no multi-cluster hub, remote credential lifecycle, or informer cache.
- Direct changes remain disabled by default. Apply mode is limited to Deployments, StatefulSets, and DaemonSets; GitOps-managed workloads are refused and proposed through Git instead.
- Cost figures use per-namespace OpenCost allocation when configured, or operator-supplied unit prices. There is no cloud-billing ingestion, allocation ledger, budget engine, chargeback, or complete FinOps model.
- Capacity is a requests-based first-fit-decreasing estimate over current node shapes. It models hard taints/selectors/affinity/anti-affinity, `DoNotSchedule` spread, and fully blocking disruption budgets, but is not scheduler or autoscaler equivalence; preferred rules and several advanced constraints remain approximations.
- ML remains single-writer. Webhooks are durable only with PostgreSQL and remain at-least-once.

These boundaries are release-significant; see [known limitations](known-limitations.md).
