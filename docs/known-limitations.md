# Known limitations

This list describes the current implementation, not planned behavior. KubeRight is a development preview and should not be treated as production-ready.

## Data and recommendations

- Kubernetes inventory is read live from only the cluster in which KubeRight runs. There is no remote-cluster registration, credential lifecycle, federation, informer cache, or durable inventory history. `inventory.clusterID` labels the local source; it does not make the deployment multi-cluster.
- Inventory supports clusters, namespaces, Deployments, StatefulSets, DaemonSets, unmanaged ReplicaSets, Jobs, CronJobs, HPA, KEDA objects, and VPA detection. Optional API failures are summarized as warnings; other resource kinds are absent.
- Prometheus is queried for CPU, memory, throttling, replicas, restart/OOM/rollout evidence, efficiency, hotspots, correlated signals, recommendation generation, and forecast input. It is still one operator-configured endpoint with no durable metric cache, long-term-store coordination, recording-rule installer, or remote-cluster routing. Data quality depends on the available cAdvisor and kube-state-metrics series.
- Generated recommendation provenance and the 24-hour expiry bind advice to an observation window and workload identity. Approval rechecks the live workload UID, cluster, generation, resource-spec fingerprint, container existence, and current requests/limits. It does not rerun the analysis or prove future safety, and caller-supplied recommendations are not live-workload verified.
- Hotspot ownership follows Kubernetes pod `ownerReferences` through ReplicaSets and Jobs to supported controllers; it requires read-only pod-list access. Metrics for pods without a resolvable supported owner/container are counted as unmatched and are not ranked. The hotspot view and spike analysis are diagnostics, not a continuous profiler.
- Capacity is an explanatory requests-based bin-packing estimate, not a scheduler or Cluster Autoscaler/Karpenter simulator. It ignores affinity, taints, topology spread, PodDisruptionBudgets, and pods not owned by supported controllers. Only approved, unexpired, identity/spec/resource-matched Prometheus advice affects the recommended scenario.
- Recommendations are advisory. The Kubernetes provider is read-only and no API applies patches, creates pull requests, verifies an external rollout, or automatically rolls back changes. Approval is implemented, but it records a review decision only. The chart rejects apply mode and grants no Kubernetes write permissions.
- There is no closed-loop outcome validation, policy store, budget/SLO engine, disruption-aware optimizer, GPU model, workload dependency model, or scheduler-grade joint CPU/memory/replica/node optimization.

## Cost and FinOps

- Savings are estimates from requested CPU and memory, current replicas, 730 hours, and operator-configured unit prices. They are not invoices or realized savings.
- There is no OpenCost or cloud-billing ingestion, shared-cost allocation, discounts/reservations/spot-rate accounting, chargeback/showback ledger, budget enforcement, anomaly ledger, or carbon model. KubeRight is not a full FinOps system.

## Persistence and high availability

- External PostgreSQL can durably share recommendations, idempotency claims, reviews, and audit events. The chart does not deploy PostgreSQL, create users, rotate credentials, schedule backups, test restores, or establish an RPO/RTO.
- Without `KUBERIGHT_DATABASE_URL`, the bounded in-process repository loses all of that state at restart and cannot be shared by replicas. The chart therefore rejects API scaling without `database.enabled=true`.
- Embedded migrations are forward-applied by an optional pre-install/pre-upgrade hook and serving pods verify an exact schema/checksum match. There is no automatic down-migration command or compatibility window. Because an older binary rejects a newer schema, a Helm rollback alone may leave the old pods unable to start; use a tested backup/restore or roll-forward procedure.
- The audit-table trigger rejects update, delete, and truncate operations, but it is not WORM storage. A privileged database operator can change database objects, and the product has no external immutable audit sink or built-in retention workflow.
- The production profile is HA-shaped for the Go API only: it requires external PostgreSQL, a pre-created Secret, enough topology capacity, and working network paths. No availability SLO, failover test, scale envelope, RPO, or RTO has been established.

## Forecasting and delivery integrations

- The Go API can call the optional forecasting service, but forecasts remain experimental outputs and never trigger a Kubernetes change.
- Model persistence is local filesystem persistence. The chart mounts `/tmp` as `emptyDir` and offers no persistent/shared model volume, so model artifacts are ephemeral. ML replicas and ML autoscaling are rejected.
- ML API-key authentication is optional and unset by chart defaults. Its NetworkPolicy restricts default ingress when enforced, but operators must still protect the service and rotate the key.
- The ML compute budget is cooperative: it is checked between forecast phases but cannot preempt native numerical work already in progress. A server, ingress, or proxy deadline is required for a hard wall-clock limit.
- Webhook delivery uses a bounded process-memory queue. It retries transient failures and exports delivered/failed/dropped counters, but there is no durable outbox, dead-letter queue, replay API, or exactly-once guarantee. Full queues and restarts can lose notifications.
- The dashboard can export approved strategic-merge patches, but KubeRight has no Git provider integration and does not open, merge, or reconcile pull requests.

## Security and operations

- TLS termination, rate limiting across replicas, web-application firewalling, identity-provider configuration, and certificate rotation are external responsibilities.
- Kubernetes authentication requires API users to receive RBAC for the virtual `kuberight.io` resources used by SubjectAccessReview. The chart grants the service account review permissions but does not automatically grant application access to human users.
- `/healthz`, `/readyz`, and `/metrics` are unauthenticated. They should not be publicly exposed. Optional per-recommendation metrics also reveal workload labels and are disabled by default.
- Static-token mode is a single all-powerful application credential with no expiry or per-user audit identity.
- The chart's default NetworkPolicy isolates ingress but not egress. Turning on egress isolation without explicit DNS, Kubernetes API, PostgreSQL, Prometheus, forecasting, and webhook rules will break the corresponding features.
- No SLO, recovery-point objective, recovery-time objective, capacity envelope, or long-running scale result has been established.
- The OpenShift and production profiles are rendered configurations, not proof of runtime certification on every supported distribution. EKS, AKS, and GKE have no provider-specific integration tests.

## Packaging and compatibility

- Source builds use digest-pinned official Node, Go, Python, and Ubuntu builders. The backend runtime is Google Distroless static and the ML runtime is Canonical's chiselled Python image. Every base-digest refresh still requires review and complete image build, scan, SBOM, runtime, and architecture gates.
- Historical CI run `39670162919` exercised arm64 under QEMU on an amd64 GitHub-hosted runner. That validates instruction-set startup and selected runtime imports for the older tested commit, not native-arm performance, kernel behavior, or the current uncommitted tree.
- A Dockerfile or Helm `appVersion` does not prove the corresponding public image tag exists. Only use a digest from a complete successful release workflow and verify its signature and attestations.
- The checked-in OpenAPI file is a repository artifact and is not served by the Go process. Contract drift remains possible and must be caught in review and tests.
- The chart declares Kubernetes 1.35 or newer. Compatibility outside the exact schema and Kind versions exercised by CI is unverified.
- Standalone manifests contain fixed namespace and image values and must be regenerated after chart changes. Use Helm for environment-specific settings.
- API `/api/v1` is versioned, but a formal backward-compatibility window and deprecation schedule have not been established.
