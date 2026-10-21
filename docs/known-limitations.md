# Known limitations

This list describes the current implementation, not planned behavior. KubeRight is a development preview and should not be treated as production-ready.

## Data and recommendations

- Kubernetes inventory is read live from only the cluster in which KubeRight runs. There is no remote-cluster registration, credential lifecycle, federation, informer cache, or durable inventory history.
- Inventory supports clusters, namespaces, Deployments, StatefulSets, DaemonSets, unmanaged ReplicaSets, Jobs, CronJobs, HPA, KEDA objects, and VPA detection. Optional API failures are summarized as warnings; other resource kinds are absent.
- Metrics history is not collected. The recommendation endpoint requires callers to provide CPU and memory samples.
- Prometheus configuration is syntax-checked, but Prometheus is not queried. OpenCost is not integrated.
- Recommendation, idempotency, and audit records are bounded in-memory data. Restarts lose them, and replicas do not share them.
- Horizontal scaling of the API is unsupported until those records use an external shared durable store. The chart and every supplied profile therefore default to one replica with the HPA disabled.
- Recommendations are advisory. The Go provider is read-only and no API applies patches, creates pull requests, performs approvals, or rolls back changes. The chart rejects apply mode and grants no Kubernetes write permissions.
- There is no feedback loop, cost-savings validation, policy store, budget model, SLO model, disruption-budget evaluation, topology-aware model, or node-bin-packing simulation.

## Forecasting

- The ML service is independently callable but is not connected to the Go API or dashboard.
- Model persistence is local filesystem persistence. The chart mounts `/tmp` as `emptyDir` and offers no persistent-volume value for model artifacts, so models are ephemeral in the supplied deployment.
- Horizontal scaling of the ML service is unsupported because replicas would maintain independent model files. Its chart defaults and supplied profiles keep one replica with the HPA disabled.
- ML API-key authentication is optional and unset by chart defaults. Its NetworkPolicy limits default ingress to Go pods when the policy is enforced, but the Go service currently does not call it.
- The ML compute budget is cooperative: it is checked between forecast phases but cannot preempt native numerical work already in progress. A separate server, ingress, or proxy deadline is required for a hard wall-clock limit.
- Forecasts are experimental outputs, not an automatic source of Kubernetes changes.

## Persistence

- The chart does not deploy or configure a database. Adding an external database requires an application repository implementation first; environment variables alone cannot make recommendation or audit data durable.
- There are no application database migrations, backup hooks, restore procedure, or schema-compatibility guarantees.

## Security and operations

- TLS termination, rate limiting across replicas, web-application firewalling, identity-provider configuration, and certificate rotation are external responsibilities.
- Kubernetes authentication requires API users to receive RBAC for the virtual `kuberight.io` resources used by SubjectAccessReview. The chart grants the service account review permissions but does not automatically grant application access to human users.
- `/healthz`, `/readyz`, and `/metrics` are unauthenticated. They should not be publicly exposed.
- Static-token mode is a single all-powerful application credential with no expiry or per-user audit identity.
- The chart's default NetworkPolicy isolates ingress but not egress. Turning on egress isolation without explicit DNS and Kubernetes API rules will break the service.
- No SLO, recovery-point objective, recovery-time objective, capacity envelope, or long-running scale result has been established.
- The OpenShift and production profiles are rendered configurations, not proof of runtime certification on every supported distribution. EKS, AKS, and GKE have no provider-specific integration tests.

## Packaging and compatibility

- Source builds use digest-pinned official Node, Go, Python, and Ubuntu builders. The backend runtime is Google Distroless static and the ML runtime is Canonical's chiselled Python image. Rebuilding does not depend on `reg.mini.dev`, but every digest refresh still requires review and the complete image build, scan, SBOM, runtime, and architecture gates.
- The arm64 gate uses QEMU on an amd64 GitHub-hosted runner. CI run `39670162919` validated the instruction set, dynamic linking, scientific imports, and startup, but it is not native-arm performance or kernel-behavior evidence.
- A Dockerfile or Helm `appVersion` does not prove the corresponding public image tag exists. Only use a digest from a successful release workflow and verify its signature.
- The checked-in OpenAPI file is a repository artifact and is not served by the Go process. Contract drift remains possible and must be caught in review and tests.
- The chart declares Kubernetes 1.35 or newer, matching an actively maintained upstream line at the time of this candidate. Compatibility outside CI's tested Kubernetes schema/Kind versions is unverified.
- Standalone manifests contain fixed namespace and image values and must be regenerated after chart changes. Use Helm for environment-specific settings.
- API `/api/v1` is versioned, but a formal backward-compatibility window and deprecation schedule have not been established.
