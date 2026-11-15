# Known limitations

This list describes the current implementation, not planned behavior. KubeRight is a release candidate. The evidence for each capability is in [production readiness](PRODUCTION-READINESS.md).

## Data and recommendations

- One installation reads one cluster: the cluster it runs in. There is no remote-cluster registration, credential lifecycle, or federation. `inventory.clusterID` labels the local source; it does not make the installation multi-cluster.
- Inventory supports namespaces, Deployments, StatefulSets, DaemonSets, unmanaged ReplicaSets, Jobs, CronJobs, HPA, KEDA objects, VPA detection, and Karpenter NodePools. Other resource kinds are absent. Optional API failures are reported as warnings.
- Prometheus is one operator-configured endpoint. There is no durable metric cache, recording-rule installer, or remote-cluster routing. Data quality depends on the cAdvisor and kube-state-metrics series that are available.
- Pressure stall information (PSI) needs cgroup v2 and Kubernetes 1.34 or later. Without PSI series, the CPU-saturation and memory-stall checks are skipped, and the other signals apply.
- Node groups are identified from provider labels and annotations, and OpenShift MachineSets from Machine objects. Node-group minimum and maximum sizes are read from OpenShift MachineAutoscalers and from the status ConfigMap that Cluster Autoscaler writes. Cluster API and OpenShift groups match that status exactly; EKS, GKE, and AKS groups match it by the provider's naming convention, so those limits are marked inferred. Groups without a visible minimum, including those scaled by the GKE and AKS node-pool autoscalers outside the cluster, may not shrink as far as the estimate shows.
- Node impact is a requests-based bin-packing estimate, not a scheduler or autoscaler simulation. It honors taints and tolerations, nodeSelector, required node and pod affinity, required pod anti-affinity, DoNotSchedule topology spread, and PodDisruptionBudgets that allow no evictions. Pods those budgets protect are assumed to stay on their current nodes, although a rollout may move them. Namespace selectors in pod affinity terms are treated as selecting every namespace. Hostname spread is approximated by a per-node cap. Preferred affinity, ScheduleAnyway spread, matchLabelKeys, and pods that no supported controller owns are not modeled.
- Recommendation provenance and the 24-hour expiry bind advice to an observation window and a workload identity. Approval rechecks the live UID, generation, resource-spec fingerprint, container, and current requests and limits. It does not rerun the analysis or prove future safety. Caller-supplied recommendations are not checked against a live workload.
- There is no GPU model, no workload dependency model, and no joint optimization of CPU, memory, replicas, and nodes.

## Automation and GitOps

- Changes are applied only with `operationMode=apply`, only to approved and fresh recommendations, and only in namespaces labeled `kuberight.io/automation=enabled`. Apply mode requires Kubernetes or OIDC authentication and cluster-scoped RBAC.
- A change is applied by patching the workload template, which rolls its pods. In-place pod resize is not used.
- After a change, KubeRight compares OOM kills, restarts, throttling, and optional latency against the period before it. A regression is rolled back to the exact previous resources. A full verdict needs at least one hour of history after the change. Severe signals (OOM kills, restarts, and throttling or pressure that is high and rising) are checked from five minutes after the change is first observed, and the first observation happens at a full verification pass, up to `verification.interval` after the rollout. A regression is caught only after it has happened, and gradual regressions wait for the hourly verdict. Rollbacks ignore the change window.
- Pull requests can be opened on GitHub, GitLab, and Gitea or Forgejo. KubeRight edits only the resources of the named container in a Kubernetes manifest file. It does not merge or reconcile pull requests. Helm values files are edited at an annotated key; templates, Kustomize patches, and values spread across several files are not. The live pull-request test was against Gitea; GitHub and GitLab are covered by contract tests.

## Cost and FinOps

- Savings are estimates of requested CPU and memory over 730 hours, priced from configured unit prices or OpenCost. A lower request is not a lower bill until nodes are removed. They are not invoices or realized savings. Node impact reports three amounts separately: the request reduction; feasible savings from the nodes the packing frees within node-group minimums, priced at unit prices for their allocatable CPU and memory rather than instance prices; and billing, which is not measured.
- There is no cloud-billing ingestion, shared-cost allocation, discount, reservation, or spot-rate accounting, chargeback ledger, budget enforcement, or carbon model.

## Persistence and high availability

- External PostgreSQL stores recommendations, reviews, policies, fleet scans, verification outcomes, and the audit trail, and can be shared by replicas. The chart does not deploy PostgreSQL, create users, rotate credentials, or schedule backups. A backup and restore was tested once, manually.
- Without a database, the in-process repository loses its state at restart and cannot be shared, so the chart rejects API scaling without `database.enabled=true`.
- Migrations are applied forward by a pre-install or pre-upgrade hook, and serving pods require an exact schema match. Down migrations are tested but not exposed as a command. A Helm rollback after a migration needs a restore from backup or a roll-forward; see the [runbook](operations-runbook.md).
- The audit table rejects update, delete, and truncate, but it is not WORM storage. A privileged database operator can still change it, and there is no external audit sink or retention workflow.
- No availability SLO, failover test, recovery-point objective, recovery-time objective, or long-running scale result has been established.

## Forecasting and notifications

- Forecasts are advisory. They never authorize or trigger a Kubernetes change.
- The ML model store is single-writer, so ML replicas and ML autoscaling are rejected. Without `ml.persistence.enabled`, models are lost when the pod is replaced.
- The ML API key is optional and unset by default. The NetworkPolicy limits ingress when enforced, but operators must still protect the service and rotate the key.
- The ML compute budget is cooperative: it is checked between forecast phases and cannot interrupt numerical work in progress. A proxy deadline is needed for a hard time limit.
- CPU forecast intervals cover 88.4% on held-out production VMs, short of the 90% target, on machines with abrupt level shifts.
- On 195 production VMs, the forecasting service's P95 forecasts, and its CPU P50 forecasts, have higher pinball loss than a seasonal-naive baseline on most VMs. LightGBM quantile regression with conformal calibration is more accurate but covers only 91–92% at P95. Neither replaces observed percentiles for sizing.
- With the PostgreSQL store, webhook events are written to an outbox, retried with backoff, dead-lettered after a rejection or ten attempts, and can be replayed. Delivery is at least once: an event delivered just before a crash can be sent again with the same delivery ID. The outbox write is not in the same transaction as the change it reports, so a database failure between the two loses the event to the in-memory fallback queue. Without PostgreSQL, webhooks use a process-memory queue of 256 events that a full queue or a restart can lose.

## Security and operations

- TLS termination, cross-replica rate limiting, web-application firewalling, and certificate rotation are external responsibilities.
- Kubernetes authentication requires users to have RBAC for the virtual `kuberight.io` resources. The chart does not grant these to human users.
- `/healthz`, `/readyz`, and `/metrics` are unauthenticated and should not be exposed publicly. Optional per-recommendation metrics reveal workload labels and are disabled by default.
- Static-token mode has one credential with no expiry and no per-user audit identity. Apply mode rejects it.
- The default NetworkPolicy isolates ingress but not egress. With `networkPolicy.egressIsolation`, DNS and the release's forecasting service are allowed automatically, and the chart refuses to render without the Kubernetes API server addresses in `networkPolicy.kubernetesAPI.cidrs` or an explicit rule for them. PostgreSQL, Prometheus, OpenCost, OIDC, Git hosts, and webhooks still need explicit rules. Egress isolation has been rendered and schema-checked but not tested on a live cluster.
- No external penetration test has been performed.

## Packaging and compatibility

- The OpenShift profile was tested on Kubernetes with OpenShift's restricted settings and Route CRD, not on an OpenShift cluster. It is not certified. EKS, AKS, and GKE have no provider-specific integration tests.
- ARM64 images are built, scanned, and run under QEMU in CI, not on native ARM64 hardware.
- A chart `appVersion` does not prove that an image tag exists. Use the digests recorded by a successful release workflow and verify their signatures and attestations.
- The OpenAPI file is checked against the routes in CI but is not served by the API.
- The chart requires Kubernetes 1.35 or later. Versions other than those tested in CI are unverified.
- `/api/v1` has no formal backward-compatibility window or deprecation schedule yet.
