# Architecture

KubeRight currently has two deployable services and a browser application. The Go service is the system entry point; the Python service is optional and is not yet called by the Go service.

```text
Browser
  |
  | HTTP + bearer token
  v
Go API and embedded dashboard
  |-- TokenReview / SubjectAccessReview --> local Kubernetes API
  |-- read-only inventory -------------> local Kubernetes API
  |-- deterministic recommendation ----> bounded in-memory history
  `-- Prometheus-format service metrics

Optional Python forecasting service
  |-- direct HTTP API only
  `-- versioned model files under its configured local directory
```

## Go service

The executable in `cmd/kuberight` serves the dashboard and `/api/v1` API on one HTTP listener. Middleware adds request IDs, structured request logs, recovery, time and body limits, security headers, metrics, authentication, and authorization.

In the default `kubernetes` authentication mode, the service sends the bearer token to the local API server's `TokenReview` endpoint. It then sends the requested `kuberight.io` resource and verb to `SubjectAccessReview`. Authentication or authorization dependency failures fail closed. `static` mode uses a single constant-time-compared token and grants full application access; it is a development facility.

The local inventory provider uses in-cluster credentials and live client-go list calls. It returns clusters, namespaces, workload resource settings, and HPA/KEDA/VPA presence. The default Helm scope restricts inventory to the release namespace. There are no informers, shared caches, remote-cluster clients, or stored inventory snapshots yet.

The optimizer is deterministic. It receives CPU and memory samples in the request, calculates percentiles, applies safety margins and bounded reductions, and emits confidence, risk, quality, and autoscaler warnings. It never mutates a Kubernetes object. Recommendation and audit history are bounded, process-local memory and are different in each replica.

## Dashboard

The React dashboard is built into the Go image and served as a same-origin single-page application. It calls only the Go API. The bearer token is supplied by the operator and retained in browser session storage. Pages backed by integrations that do not exist return and show explicit unavailable states.

## Forecasting service

The FastAPI service accepts a uniformly sampled CPU or memory series, validates time order and bounds, performs time-aware validation, and returns a forecast, intervals, quality signals, anomaly/seasonality/drift summaries, and fallback reasons. It can retain versioned local model artifacts when a request supplies `series_id` and `persist=true`.

The chart can deploy this service and restrict ingress to the Go pod. However, the Go API does not yet call it, and the chart does not provision durable model storage. Its default model directory is under `/tmp` and is lost with the pod.

## Deployment and trust boundaries

The Helm chart renders the Go deployment, service account, RBAC, service, probes, disruption budget, NetworkPolicy, optional ingress or edge-terminated OpenShift Route, optional ML deployment, and optional monitoring CRs. The primary and ML containers are configured as non-root with read-only root filesystems and writable bounded `/tmp` volumes.

The chart and all supplied profiles keep both services at one replica with HPAs disabled. This is a state-consistency constraint, not an HA claim: API recommendation/idempotency/audit records are process-local, and ML model files are pod-local. Horizontal scaling remains unsupported until those stores are external and shared.

TLS and external identity-token issuance are outside KubeRight. The ingress, gateway, Route, or service mesh is a trust boundary. The Kubernetes API is also a critical dependency because it authenticates users and, when inventory is enabled, supplies data.

## Present but not connected

- `KUBERIGHT_PROMETHEUS_URL` is validated but no Prometheus query client is wired to collection endpoints.
- No database repository implementation or database deployment configuration is supplied.
- The application has no apply handler or mutating provider; the chart rejects apply mode and grants no write RBAC.
- Approval, GitOps, history, forecast, and metrics-history collection routes intentionally return an unavailable status when no provider implements them.
- OpenCost and remote-cluster registration are not implemented.

These boundaries are release-significant; see [known limitations](known-limitations.md).
