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

## Read-only operation and GitOps

- KubeRight never changes workloads. Acting on an approved recommendation (a GitOps pull request, the exported patch, or a `kubectl` command) is up to you. The opt-in apply mode of earlier releases, with in-place resize and automatic rollback, was removed; its endpoints answer `410 Gone`, and `KUBERIGHT_OPERATION_MODE=apply` falls back to `recommend` with a startup warning.
- Verification judges whether a pod runs the approved values from the pod's spec and its resize conditions (Kubernetes 1.33 or later), not from the container's cgroup. An `OnDelete` workload whose pods are never recreated or resized stays `not_applied` indefinitely.
- After a change, KubeRight compares OOM kills, restarts, throttling, and optional latency against the period before it. A regression is reported (verification status, webhook, and alert) but not undone: restoring the previous resources is up to you. A full verdict needs at least one hour of history after the change. Severe signals (OOM kills, restarts, and throttling or pressure that is high and rising) are checked from five minutes after the change is first observed, and the first observation happens at a full verification pass, up to `verification.interval` after the rollout. A regression is caught only after it has happened, and gradual regressions wait for the hourly verdict.
- Pull requests can be opened on GitHub, GitLab, and Gitea or Forgejo. KubeRight edits only the resources of the named container, in a Kubernetes manifest file or at an annotated key of a Helm values file. It does not merge or reconcile pull requests. Chart templates, Kustomize patches, and values spread across several files are not edited. The live pull-request test was against Gitea; GitHub and GitLab are covered by contract tests.

## Cost and FinOps

- Savings are estimates of requested CPU and memory over 730 hours, priced from configured unit prices or OpenCost. A lower request is not a lower bill until nodes are removed. They are not invoices or realized savings. Node impact reports three amounts separately: the request reduction; feasible savings from the nodes the packing frees within node-group minimums, priced at unit prices for their allocatable CPU and memory rather than instance prices; and billing, which is not measured.
- There is no cloud-billing ingestion, shared-cost allocation, discount, reservation, or spot-rate accounting, chargeback ledger, budget enforcement, or carbon model.

## Persistence and high availability

- External PostgreSQL stores recommendations, reviews, policies, fleet scans, verification outcomes, the webhook outbox, and the audit trail, and can be shared by replicas. The chart does not deploy PostgreSQL, create users, rotate credentials, or schedule backups. A backup and restore was tested once, manually.
- Without a database, the in-process repository loses its state at restart and cannot be shared, so the chart rejects API scaling without `database.enabled=true`.
- Migrations are applied forward by a pre-install or pre-upgrade hook, and serving pods require an exact schema match. Down migrations are tested but not exposed as a command. A Helm rollback after a migration needs a restore from backup or a roll-forward; see the [runbook](operations-runbook.md).
- The audit table rejects update, delete, and truncate, but it is not WORM storage. A privileged database operator can still change it, and there is no external audit sink or retention workflow.
- No availability SLO, failover test, recovery-point objective, recovery-time objective, or long-running scale result has been established.

## Forecasting and notifications

- Forecasts are advisory. They never authorize or trigger a Kubernetes change.
- Forecasts run inside the API process with no separate service. The engine keeps no model state between requests, so every forecast refits its candidate models on the history it is given; at most as many forecasts run at once as the process has CPUs (at least two).
- A forecast is made only from history with no gap longer than three steps; earlier history before such a gap is dropped. It never forecasts further ahead than the history behind it, so 30- and 90-day horizons need Prometheus retention of at least that long, which OpenShift's default 15-day retention does not provide.
- Forecast accuracy is measured per series by rolling-origin validation and reported with every forecast. When the history is too short to validate any model, a naive forecast is labelled as a fallback with unmeasured accuracy.
- On 188 Bitbrains VMs, the engine's 90% intervals covered 92.3% (91.0–93.6) of held-out hours for CPU and 92.4% (91.2–93.5) for memory. Its point forecast beat seasonal naive on 60% of VMs for CPU and 81% for memory, but on CPU its median WAPE (14.6%) was higher than seasonal naive's (12.9%), because where it loses it loses by more. VMs with abrupt level shifts remain the weakest case. These are virtual machines, not containers.
- With the PostgreSQL store, webhook events are written to an outbox, retried with backoff, dead-lettered after a rejection or ten attempts, and can be replayed. Delivery is at least once: an event delivered just before a crash can be sent again with the same delivery ID. The outbox write is not in the same transaction as the change it reports, so a database failure between the two loses the event to the in-memory fallback queue. Without PostgreSQL, webhooks use a process-memory queue of 256 events that a full queue or a restart can lose.

## Security and operations

- TLS termination, cross-replica rate limiting, web-application firewalling, and certificate rotation are external responsibilities.
- KubeRight has no sign-in, like Goldilocks. Anyone who can reach its Service, Route, or Ingress can read the cluster inventory, generate recommendations, and approve or reject them, and every action is audited as the one anonymous identity `system:anonymous`, so the audit trail cannot say who acted. Access control is the network's job: keep KubeRight on a port-forward, or restrict the Route or Ingress.
- The OpenShift profile has not been tested on an OpenShift cluster: the manifests are schema-checked and dry-run against Kubernetes 1.35, but the Route and monitoring access have not run there.
- `/healthz`, `/readyz`, and `/metrics` should not be exposed publicly. Optional per-recommendation metrics reveal workload labels and are disabled by default.
- The default NetworkPolicy isolates ingress but not egress. With `networkPolicy.egressIsolation`, DNS is allowed automatically, and the chart refuses to render without the Kubernetes API server addresses in `networkPolicy.kubernetesAPI.cidrs` or an explicit rule for them. PostgreSQL, Prometheus, OpenCost, Git hosts, and webhooks still need explicit rules. Egress isolation has been rendered and schema-checked but not tested on a live cluster.
- No external penetration test has been performed.

## Packaging and compatibility

- The OpenShift profile was tested on Kubernetes with OpenShift's restricted settings and Route CRD, not on an OpenShift cluster. It is not certified. EKS, AKS, and GKE have no provider-specific integration tests.
- ARM64 images are built, scanned, and run under QEMU in CI, not on native ARM64 hardware.
- A chart `appVersion` does not prove that an image tag exists. Use the digests recorded by a successful release workflow and verify their signatures and attestations.
- The OpenAPI file is checked against the routes in CI but is not served by the API.
- The chart requires Kubernetes 1.35 or later. Versions other than those tested in CI are unverified.
- The `/api/v1` compatibility policy is in the README. After the first release tag, CI runs `scripts/check-openapi-compat.py` against the newest earlier release reachable from `main`. It checks contract shape, not behavior or semantic changes, so release review remains required.
