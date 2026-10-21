# Release status

- Candidate: `v0.1.0-rc.1`
- Validated code/image commit: `37ad507e92e3281a9ecbd9dd79366987a11b1799`
- Release source commit: pending merge
- Status: **not published; not production ready**

## Evidence recorded on 2026-10-19

- Go formatting, module verification, vet, all unit/provider tests, a production entrypoint build, govulncheck, and a local static-auth health/readiness/API/dashboard smoke test passed; measured aggregate statement coverage is 49.1%.
- Python Ruff and mypy checks passed; 26 tests passed with 91.84% total measured coverage; the complete development dependency audit found no known vulnerabilities.
- Dashboard typecheck and zero-warning lint passed; 5 tests passed; the production bundle built; `npm audit --audit-level=high` reported zero vulnerabilities.
- Helm strict lint, supported-profile rendering, generated-manifest drift, negative-value checks, and Kubernetes 1.35 kubeconform validation passed.
- The checked-in OpenAPI contract passed `openapi-spec-validator` 0.7.2.

## CI evidence recorded on 2026-10-21

[CI run 39612829056](https://github.com/krishnamanaiducloud/kuberight/actions/runs/39612829056) on commit `37ad507` passed Go 1.27.3 race/build/coverage, Python, frontend, Helm/schema, dependency vulnerability, secret scanning, both amd64 image builds, positive numeric-user enforcement, final-runtime scientific-package imports, verified absence of pip, ensurepip, and the unused Pebble supervisor, Trivy 0.76.0 HIGH/CRITICAL scans, SPDX SBOM generation, read-only container smoke tests, and the disposable Kind deployment. This run validates the migration away from the retiring `reg.mini.dev` registry to digest-pinned maintained builders and shell-less runtime images.

The run did not pass as a whole: Dependency Review failed because the repository Dependency Graph is disabled. This is a repository-owner setting, not a successful gate, and it must not be bypassed.

## Pending repository and registry evidence

- Dependency Graph enablement followed by a successful Dependency Review and complete pull-request workflow on the exact merge candidate.
- A successful `main` workflow after merge.
- A successful required arm64 build, scan, native-library import, and read-only QEMU smoke gate on the exact merge candidate. The gate is checked in but has not yet produced passing evidence.
- Docker Hub tags and immutable digests for `mohankrishna999/kuberight:0.1.0-rc.1` and `mohankrishna999/kuberight-ml:0.1.0-rc.1`.
- Cosign signature/attestation verification and GitHub prerelease URL.
- A safe staging/promotion or resumable recovery design for failures after an immutable tag is first published.

No success field above may be inferred from the presence of a workflow or Dockerfile. Fill it only from the exact successful run. `latest` and stable `v0.1.0` publication are prohibited while production readiness remains blocked.
