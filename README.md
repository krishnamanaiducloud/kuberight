# KubeRight

KubeRight is a Kubernetes resource-inventory and right-sizing project. It combines a Go API, a React dashboard, an optional Python forecasting service, and a Helm chart.

> **Development preview:** this repository is not a verified production release. A source checkout, chart version, or Dockerfile is not evidence that an image has been published or passed the required release gates. Verify a signed release and its digest before deploying.

## What works today

- Namespace-scoped or cluster-scoped, read-only inventory of Kubernetes workloads and autoscalers.
- Deterministic CPU and memory recommendations from caller-supplied samples, including confidence, risk, data-quality, burst, HPA, VPA, KEDA, restart, and OOM warnings.
- Bearer authentication using Kubernetes `TokenReview` plus per-request `SubjectAccessReview`, or a development-only static token mode.
- A dashboard backed by the versioned Go API. Views whose integrations are unavailable display that state instead of fabricated data.
- A separately deployable forecasting API with deterministic fallback behavior, time-aware validation, confidence intervals, and optional local model persistence.
- Secure-by-default workload settings in a Helm chart, plus development, production-shaped, OpenShift, and cluster-wide rendering profiles.

The Go API does **not** yet query Prometheus or OpenCost, call the ML service, persist to PostgreSQL, register remote clusters, create GitOps changes, or apply recommendations. See [known limitations](docs/known-limitations.md) before evaluating the project.

## Quick start from source

Prerequisites are Go 1.27, Node.js 24, and npm. This local mode disables in-cluster inventory and uses a static token; it is for development only.

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

For Kubernetes installation, required RBAC, immutable-image guidance, and platform notes, use the [installation guide](docs/installation.md).

## Repository map

| Path | Purpose |
| --- | --- |
| `cmd/kuberight`, `internal/` | Go API, authentication, inventory, recommendations, and in-process history |
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
