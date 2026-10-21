# Production readiness — blocked

Status on 2026-10-21: **development preview, not production ready**. The current candidate is `0.1.0-rc.1`. Do not publish `latest`, create a stable `v0.1.0` tag, or claim Kubernetes-distribution certification.

## Acceptance evidence

| Criterion | Implementation or evidence | Status |
| --- | --- | --- |
| Go API compiles and unit tests pass | Formatting, module verification, `go vet`, all unit tests, the race detector, coverage enforcement, and `go build ./cmd/kuberight` passed in CI with Go 1.27.3; measured aggregate statement coverage is 49.1% | Verified on commit `8e7eebe` in CI run `39670162919` |
| Authentication fails closed | Kubernetes TokenReview/SubjectAccessReview and static-development mode tests in `internal/httpapi` | Locally verified; live-cluster test pending |
| Read-only in-cluster inventory | `internal/provider/kubernetes.go` and fake-client tests | Locally verified; Kind/OpenShift runtime pending |
| Recommendation engine | Deterministic optimizer tests cover sparse, bursty, HPA, and safety behavior | Locally verified |
| ML quality gates | Ruff, mypy, 26 tests, and 91.84% measured Python coverage | Locally verified |
| Dashboard quality gates | Typecheck, zero-warning lint, 5 tests, production build, and high-severity npm audit | Locally verified |
| API contract | `api/openapi.yaml` validated with `openapi-spec-validator` 0.7.2 | Locally verified |
| Helm packaging | Strict lint, default/profile renders, standalone-manifest drift, negative-value tests, and Kubernetes 1.35 kubeconform schemas | Locally verified |
| Container build and runtime | CI built amd64 and arm64 variants of both images from the replacement pinned bases, enforced positive numeric users, imported the final scientific runtime on both architectures, verified pip, ensurepip, and Pebble are absent, and passed read-only smoke tests; arm64 execution used QEMU | Verified on commit `8e7eebe` in CI run `39670162919` |
| Image vulnerability policy | Trivy 0.76.0 reported no HIGH/CRITICAL findings in either image on amd64 or arm64; govulncheck, Python dependency audit, and npm audit also passed | Verified on commit `8e7eebe` in CI run `39670162919` |
| SBOM and signing | CI generated SPDX JSON SBOMs for both amd64 images and verified that the copied Ubuntu `libstdc++6` runtime is package-attributed | SBOM generation verified on commit `8e7eebe`; release architecture-specific SBOMs and signing pending |
| Kubernetes integration | CI installed both locally built images with Helm and completed both rollouts in disposable Kubernetes 1.35 Kind | Basic deployment verified; workload behavior scenarios remain pending |
| OpenShift compatibility | Restricted settings and Route profile render | **No OpenShift end-to-end evidence** |
| Durable persistence and recovery | No Go database adapter, migration, backup, or restore path | **Not implemented** |
| Horizontal scaling and HA | Chart profiles remain singleton with HPAs disabled because API and ML state is local to each process or pod | **Not implemented** |
| Metrics/cost ingestion | Prometheus URL validation exists | **Prometheus ingestion and OpenCost not implemented** |
| Forecast integration | Standalone ML API exists | **Go-to-ML integration not implemented** |
| Apply, approval, and GitOps flow | Chart rejects apply mode and grants no write RBAC | **Mutation, approval, PR, and rollback flows not implemented** |
| Multi-cluster operation | Local in-cluster inventory only | **Not implemented** |

## Release blockers

1. Required GitHub quality gates must pass on the exact commit. On commit `8e7eebe`, every code, amd64/arm64 image, scan, SBOM, smoke, and Kind job passed, but Dependency Review failed because the repository Dependency Graph is disabled. An owner must enable it and the exact release candidate must then pass the complete workflow.
2. Docker Hub release environment secrets and approval must be present; image tags, digests, signatures, and attestations must be recorded after publication.
3. A live OpenShift test is required before any OpenShift certification claim.
4. The unimplemented production capabilities listed above must either be delivered and verified or remain explicit exclusions from the product scope.
5. The release workflow currently publishes the immutable image tags before its signing and final verification steps. It must gain a verified staging/promotion or resumable recovery design before a release tag is created, so a later failure cannot strand an incomplete immutable release.

## Publishing rule

Only immutable `0.1.0-rc.1` candidate tags may be attempted from this state, and only after every mandatory repository gate succeeds. The release workflow refuses replacement of an existing tag and does not publish `latest`.
