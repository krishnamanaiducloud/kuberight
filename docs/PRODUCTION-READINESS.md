# Production readiness — blocked

Status on 2026-10-19: **development preview, not production ready**. The current candidate is `0.1.0-rc.1`. Do not publish `latest`, create a stable `v0.1.0` tag, or claim Kubernetes-distribution certification.

## Acceptance evidence

| Criterion | Implementation or evidence | Status |
| --- | --- | --- |
| Go API compiles and unit tests pass | Formatting, module verification, `go vet`, all unit tests, and `go build ./cmd/kuberight` passed locally with Go 1.27.2; measured aggregate statement coverage is 49.1% | Locally verified; CI race test pending |
| Authentication fails closed | Kubernetes TokenReview/SubjectAccessReview and static-development mode tests in `internal/httpapi` | Locally verified; live-cluster test pending |
| Read-only in-cluster inventory | `internal/provider/kubernetes.go` and fake-client tests | Locally verified; Kind/OpenShift runtime pending |
| Recommendation engine | Deterministic optimizer tests cover sparse, bursty, HPA, and safety behavior | Locally verified |
| ML quality gates | Ruff, mypy, 26 tests, and 91.84% measured Python coverage | Locally verified |
| Dashboard quality gates | Typecheck, zero-warning lint, 5 tests, production build, and high-severity npm audit | Locally verified |
| API contract | `api/openapi.yaml` validated with `openapi-spec-validator` 0.7.2 | Locally verified |
| Helm packaging | Strict lint, default/profile renders, standalone-manifest drift, negative-value tests, and Kubernetes 1.35 kubeconform schemas | Locally verified |
| Container build and runtime | Digest-pinned, non-root Minimus Dockerfiles and CI smoke jobs exist | **Unverified: no local Docker runtime** |
| Image vulnerability policy | CI blocks HIGH/CRITICAL Trivy results; govulncheck, Python development-dependency audit, and npm audit passed locally | Container scan pending |
| SBOM and signing | Release workflow creates SPDX SBOMs and keyless Cosign signatures | Pending release workflow |
| Kubernetes integration | CI defines disposable Kind install and rollout checks | Pending first CI run |
| OpenShift compatibility | Restricted settings and Route profile render | **No OpenShift end-to-end evidence** |
| Durable persistence and recovery | No Go database adapter, migration, backup, or restore path | **Not implemented** |
| Horizontal scaling and HA | Chart profiles remain singleton with HPAs disabled because API and ML state is local to each process or pod | **Not implemented** |
| Metrics/cost ingestion | Prometheus URL validation exists | **Prometheus ingestion and OpenCost not implemented** |
| Forecast integration | Standalone ML API exists | **Go-to-ML integration not implemented** |
| Apply, approval, and GitOps flow | Chart rejects apply mode and grants no write RBAC | **Mutation, approval, PR, and rollback flows not implemented** |
| Multi-cluster operation | Local in-cluster inventory only | **Not implemented** |

## Release blockers

1. Required GitHub quality gates must pass on the exact commit, including race tests, dependency scans, Docker builds, Trivy scans, SBOM generation, non-root checks, smoke tests, and Kind rollout.
2. Docker Hub release environment secrets and approval must be present; image tags, digests, signatures, and attestations must be recorded after publication.
3. A live OpenShift test is required before any OpenShift certification claim.
4. The unimplemented production capabilities listed above must either be delivered and verified or remain explicit exclusions from the product scope.
5. Minimus announced that `reg.mini.dev`, which hosts the digest-pinned build bases, will cease operation on 2026-10-22. A reviewed mirror or base migration is required for reproducible builds after that date.
6. The release workflow currently publishes the immutable image tags before its signing and final verification steps. It must gain a verified staging/promotion or resumable recovery design before a release tag is created, so a later failure cannot strand an incomplete immutable release.

## Publishing rule

Only immutable `0.1.0-rc.1` candidate tags may be attempted from this state, and only after every mandatory repository gate succeeds. The release workflow refuses replacement of an existing tag and does not publish `latest`.
