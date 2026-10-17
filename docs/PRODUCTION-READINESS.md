# Production readiness — BLOCKED

Status: **not production ready**. Do not publish `latest`, a stable `v1.0.0` image, or claim cross-distribution certification.

## Verified on 2026-10-17

- The previous initial repository push is incomplete.
- The prepared source archive exists but is not fully imported to GitHub.
- `go test ./...` fails before compilation because `go.sum` is missing.
- `go mod tidy` cannot download Kubernetes dependencies in the available execution environment (`proxy.golang.org` DNS/network failure).
- Helm and Docker CLIs are not available in the current execution environment.
- No live Kubernetes/OpenShift end-to-end test or container image build has passed.

## Required release gates

1. Import the entire reviewed source tree, including Go APIs, tests, Helm templates, Kubernetes manifests, Apache-2.0 LICENSE, Python ML code, and workflows.
2. Run `go mod tidy` with network access and commit a verified `go.sum`.
3. Pass `go test -race ./...`, `go vet ./...`, `go build ./cmd/kuberight`, Python tests, and strict Helm lint/render.
4. Verify authentication and namespace-scoped authorization (TokenReview / SubjectAccessReview), fail-closed access, and recommendation-only behavior.
5. Test a disposable upstream Kubernetes cluster and an OpenShift cluster; verify restricted SCC and non-root images.
6. Scan container images for vulnerabilities, generate SBOMs, and ensure Docker Hub credentials are stored as GitHub Actions environment secrets.
7. Build candidate images without pushing; publish release-candidate images only after all gates pass and explicit release approval.

## Current publishing status

Docker Hub namespace: `mohankrishna999`. **No image published or verified by this process.**
