# Release status

- Candidate: `v0.1.0-rc.1`
- Source commit: pending commit and merge
- Status: **not published; not production ready**

## Evidence recorded on 2026-10-19

- Go formatting, module verification, vet, all unit/provider tests, a production entrypoint build, govulncheck, and a local static-auth health/readiness/API/dashboard smoke test passed; measured aggregate statement coverage is 49.1%.
- Python Ruff and mypy checks passed; 26 tests passed with 91.84% total measured coverage; the complete development dependency audit found no known vulnerabilities.
- Dashboard typecheck and zero-warning lint passed; 5 tests passed; the production bundle built; `npm audit --audit-level=high` reported zero vulnerabilities.
- Helm strict lint, supported-profile rendering, generated-manifest drift, negative-value checks, and Kubernetes 1.35 kubeconform validation passed.
- The checked-in OpenAPI contract passed `openapi-spec-validator` 0.7.2.

## Pending repository and registry evidence

- Pull-request and `main` GitHub Actions results.
- Go race detector, CI govulncheck confirmation, dependency review, and secret scan results.
- Backend/dashboard and ML Docker builds, non-root inspection, Trivy scans, SBOMs, container smoke tests, and Kind rollout.
- Docker Hub tags and immutable digests for `mohankrishna999/kuberight:0.1.0-rc.1` and `mohankrishna999/kuberight-ml:0.1.0-rc.1`.
- Cosign signature/attestation verification and GitHub prerelease URL.
- A safe staging/promotion or resumable recovery design for failures after an immutable tag is first published.

No success field above may be inferred from the presence of a workflow or Dockerfile. Fill it only from the exact successful run. `latest` and stable `v0.1.0` publication are prohibited while production readiness remains blocked.
