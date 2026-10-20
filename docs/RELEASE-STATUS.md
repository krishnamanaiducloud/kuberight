# Release status

- Candidate: `v0.1.0-rc.1`
- Validated code/image commit: `6a892ef0a329511dd5131751ed692e7c03f520c7`
- Release source commit: pending merge
- Status: **not published; not production ready**

## Evidence recorded on 2026-10-19

- Go formatting, module verification, vet, all unit/provider tests, a production entrypoint build, govulncheck, and a local static-auth health/readiness/API/dashboard smoke test passed; measured aggregate statement coverage is 49.1%.
- Python Ruff and mypy checks passed; 26 tests passed with 91.84% total measured coverage; the complete development dependency audit found no known vulnerabilities.
- Dashboard typecheck and zero-warning lint passed; 5 tests passed; the production bundle built; `npm audit --audit-level=high` reported zero vulnerabilities.
- Helm strict lint, supported-profile rendering, generated-manifest drift, negative-value checks, and Kubernetes 1.35 kubeconform validation passed.
- The checked-in OpenAPI contract passed `openapi-spec-validator` 0.7.2.

## CI evidence recorded on 2026-10-20

[CI run 39497500255](https://github.com/krishnamanaiducloud/kuberight/actions/runs/39497500255) on commit `6a892ef` passed Go race/build/coverage, Python, frontend, Helm/schema, dependency vulnerability, secret scanning, both amd64 image builds, positive numeric-user enforcement, ML runtime packaging-tool removal, Trivy 0.76.0 HIGH/CRITICAL scans, SPDX SBOM generation, read-only container smoke tests, and the disposable Kind deployment.

The run did not pass as a whole: Dependency Review failed because the repository Dependency Graph is disabled. This is a repository-owner setting, not a successful gate, and it must not be bypassed.

## Pending repository and registry evidence

- Dependency Graph enablement followed by a successful Dependency Review and complete pull-request workflow on the exact merge candidate.
- A successful `main` workflow after merge.
- Release-time arm64 build and validation; pull-request image validation currently covers amd64.
- Docker Hub tags and immutable digests for `mohankrishna999/kuberight:0.1.0-rc.1` and `mohankrishna999/kuberight-ml:0.1.0-rc.1`.
- Cosign signature/attestation verification and GitHub prerelease URL.
- A safe staging/promotion or resumable recovery design for failures after an immutable tag is first published.

No success field above may be inferred from the presence of a workflow or Dockerfile. Fill it only from the exact successful run. `latest` and stable `v0.1.0` publication are prohibited while production readiness remains blocked.
