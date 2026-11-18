# Release status

- Candidate: `v0.1.0-rc.4` (pending)
- Branch: `fix/release-rc4`
- Status: release candidate; see [production readiness](PRODUCTION-READINESS.md) (91/100)

## CI on the candidate pull request

[Run 43940804651](https://github.com/krishnamanaiducloud/kuberight/actions/runs/43940804651) on `914a498` (2026-11-16) passed every mandatory job:

- Go (race tests with PostgreSQL and the attribution replay)
- Dependency vulnerabilities
- Python
- Frontend
- Helm and Kubernetes schemas
- Secret scanning
- Build, scan, SBOM, smoke, and Kind (including the live browser accessibility test)
- ARM64 build, scan, and execution
- Dependency Review
- The aggregate `All mandatory gates passed` job

The Dependency graph is enabled and Dependency Review passed. The final source commit `fa1caf4` passed every mandatory pull-request gate in [run 44361712715](https://github.com/krishnamanaiducloud/kuberight/actions/runs/44361712715) and was merged as `c85ebb4`.

The immutable `v0.1.0-rc.3` tag is retained as a failed release attempt. Its release workflow stopped at startup before any job or publication because the reusable quality-gate workflow requested `pull-requests: read` while the release caller granted only `contents: read`. The `rc.4` candidate grants the caller the same read permission; no `rc.3` Docker image or GitHub release was published.

## Local reproduction of the required gates (2026-11-15)

| Gate | Result |
| --- | --- |
| Go formatting, vet, race tests with PostgreSQL 18.7 (schema version 7) | Pass on `223a8df` (2026-11-16), 17 packages; statement coverage last measured at 75.3% on `141f0cc` (floor 45%) |
| In-place resize on kind (Kubernetes 1.35.1), run by hand | Staged resize, halt and rollback, and rollback after a regressed verification at `838522c`; migration 7 through the Helm hook at `538c6f4`; not rerun on later commits |
| Attribution replay against Prometheus | 13/13 |
| Python ruff, mypy, pytest | Pass; 50 tests, 94.1% coverage (floor 90%) |
| Dashboard lint, types, unit tests, build | Pass; 56 tests |
| Browser contract and accessibility tests | 48/48, axe on all 20 views at two widths |
| OpenAPI contract | 52 operations in parity; spec valid; `scripts/check-openapi-compat.py` finds no breaking `/api/v1` change since `7822425` |
| Helm lint, profiles, unsafe-value rejection, standalone manifests | Pass |
| Alert rules and dashboard queries in promtool | Pass |
| govulncheck | No vulnerabilities (after moving to Go 1.27.4 for GO-2026-6859) |
| pip-audit | No known vulnerabilities |
| npm audit (high) | Pass; nine moderate advisories in lint-only development dependencies |
| Gitleaks over the full history | No leaks |
| Trivy (HIGH/CRITICAL) on backend, ML, and oauth2-proxy images | 0 findings |
| Multi-architecture build, SBOM, cosign sign/attest/verify (local registry) | Pass |

## Pending repository evidence

- An all-green quality-gates run for the `rc.4` permission/version correction, followed by merge to `main`.
- The release workflow on the `v0.1.0-rc.4` tag: published digests, signatures, and attestations.
- The packaged Helm chart, checksum, and Sigstore bundle attached to the GitHub prerelease.
