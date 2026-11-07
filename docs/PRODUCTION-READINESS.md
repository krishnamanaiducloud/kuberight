# Production readiness - blocked

Status on 2026-11-01: **development preview, not production ready**. The current candidate is `0.1.0-rc.1`. Do not publish `latest`, create a stable `v0.1.0` tag, or claim Kubernetes-distribution certification.

Implementation in the current working tree is not release evidence. It must be committed and every required gate must pass on the exact candidate before any row can be promoted to release-verified.

## Acceptance evidence

| Criterion | Implementation or evidence | Status |
| --- | --- | --- |
| Go API quality gates | CI run `41331015001` on pushed commit `fd598cc` passed Go formatting/module/vet/tests, race and PostgreSQL jobs with 70.2% aggregate statement coverage | Passed for that commit; current working tree differs |
| Authentication fails closed | Kubernetes TokenReview/SubjectAccessReview and static-development mode tests in `internal/httpapi` | Local/unit evidence; live-cluster identity test pending |
| Read-only in-cluster inventory | Local provider with live lists/gets and exact pod-owner resolution; no mutating RBAC | Unit/fake-client evidence; current changes need exact-candidate CI |
| Recommendation engine | Deterministic CPU/memory sizing, backtests, trends, spike/throttling/OOM/autoscaler safeguards, provenance and expiry | Implemented in source; exact-candidate verification pending |
| Approval and audit | Terminal approve/reject decisions, optional distinct approver, live freshness checks, transactional audit, PostgreSQL append-only trigger | Implemented in source; no apply/rollback automation; exact-candidate verification pending |
| Durable persistence | PostgreSQL repository, idempotency/review/audit state, embedded checksummed migrations, advisory lock, Helm migration hook, runtime schema verification | Implemented in source; backup/restore and failover evidence pending |
| API HA topology | Production profile requires PostgreSQL and configures 2-6 API replicas, PDB, HPA, and zone spreading | Configuration present; no availability SLO, load, failover, or recovery test |
| Prometheus ingestion | History, generated recommendations, efficiency, signals, forecasts, capacity evidence, and hotspots use the bounded Prometheus client | Implemented in source; exact-candidate integration evidence pending |
| Cost/FinOps | Request-based estimates use operator-supplied unit prices | Limited feature; OpenCost/billing and a full FinOps ledger are not implemented |
| Forecast integration | Go API reads Prometheus and calls the optional ML service; model service remains experimental | Implemented; model storage is pod-local and HA is unsupported |
| ML quality gates | Latest pushed CI passed the Python gate with 91.84% measured coverage | Passed for `fd598cc`; current working tree differs |
| Dashboard quality gates | Latest pushed CI passed typecheck, zero-warning lint, 24 tests, and production build | Passed for `fd598cc`; current working tree differs |
| Dashboard product UX | Evidence/freshness/risk views, workload detail, policy previews, signals, hotspots, approvals, and explicit error/unavailable states | Implemented in source; usability/accessibility validation on the exact candidate pending |
| API contract | Checked-in `api/openapi.yaml` is validated in quality gates | Exact-candidate drift check pending |
| Helm packaging | Strict lint/render/schema/negative-value/kubeconform gates exist, including migration and HA value guards | Current chart changes need exact-candidate CI and live upgrade evidence |
| Container build and runtime | Historical run `39670162919` built/scanned/smoked amd64 and QEMU-arm64 images and completed an amd64 Kind rollout for commit `8e7eebe`; the working-tree release workflow now imports exact architecture archives and runs read-only/runtime packaging checks before promotion | Historical evidence only; the new exact-archive path is unverified until CI |
| Image vulnerability policy | Historical run `39670162919` reported no HIGH/CRITICAL image findings for its exact images | Must be repeated for exact release artifacts |
| SBOM and signing | Historical CI generated SPDX SBOMs for older test images | Release architecture SBOMs, signatures, and attestations pending |
| OpenShift compatibility | Restricted settings and Route profile render | **No OpenShift end-to-end evidence** |
| Apply/rollback automation | Chart rejects apply mode and grants no write RBAC | Deliberately not implemented; approvals are advisory only |
| Multi-cluster operation | Local in-cluster inventory only | **Not implemented** |

## Current mandatory-gate state

[CI run 41331015001](https://github.com/krishnamanaiducloud/kuberight/actions/runs/41331015001) did **not** pass as a whole:

- Dependency Review failed because the repository Dependency Graph is disabled.
- Gitleaks failed on known synthetic credential literals in two test files.
- Downstream container, architecture, scan, SBOM, smoke, and Kind jobs were skipped, so this run provides no image evidence.

The working tree contains a narrow secret-scanner remediation, but that is not evidence until it is committed and the complete workflow passes. Dependency Graph enablement is a repository-owner action and must not be bypassed.

## Release blockers

1. Enable GitHub Dependency Graph and require a successful Dependency Review on the exact merge/release candidate.
2. Commit the secret-scanner remediation and obtain a successful Gitleaks result over the full pull-request history; do not treat a local allowlist as a passed gate.
3. Obtain one complete required workflow on the exact candidate, including code, PostgreSQL, chart/schema, dependency, secret, amd64/arm64 build, scan, SBOM, runtime smoke, and Kind jobs.
4. Build once and promote the exact tested multi-architecture artifacts. The working-tree release workflow now executes read-only smoke and runtime-packaging checks against the exact amd64/arm64 archives before promotion, but that path is unverified. A complete run must prove the promoted digests, scans, SBOMs, signatures, and attestations correspond to those archives, with Kind evidence for the candidate deployment.
5. Configure the protected Docker Hub environment, secrets, human approval, and immutable-tag policy. Record final digests, signatures, attestations, and prerelease URL after publication; no such evidence exists yet.
6. Make release retry/recovery idempotent and verify that a failed GitHub-release or promotion step cannot leave a misleading partial candidate. Decide and verify signed OCI Helm-chart publication rather than implying that image publication also publishes the chart.
7. Complete a live OpenShift test before any OpenShift certification claim.
8. Keep the product exclusions explicit: single in-cluster provider, no apply/automatic rollback, no durable ML store or webhook outbox, and no complete FinOps ledger. Any claim beyond those boundaries requires implementation and verification.

## Publishing rule

Only immutable `0.1.0-rc.1` candidate tags may be attempted from this state, and only after every mandatory repository gate succeeds. `latest` and stable `v0.1.0` publication remain prohibited while production readiness is blocked.
