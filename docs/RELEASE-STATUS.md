# Release status

- Candidate: `v0.1.0-rc.1`
- Latest pushed source commit: `fd598cc121e2f353d04b456ab7c4ddd6e3823d1e`
- Historical code/image evidence commit: `8e7eebe288a4c751a1ce7255095b1fceb20392c6`
- Release source commit: pending merge
- Status: **not published; not production ready**

## Latest pushed CI evidence - 2026-11-01

[CI run 41331015001](https://github.com/krishnamanaiducloud/kuberight/actions/runs/41331015001) on commit `fd598cc` passed the Go/race/PostgreSQL jobs with 70.2% aggregate statement coverage, the Python gate with 91.84% measured coverage, and the dashboard typecheck/lint/24-test/production-build gate.

The run did **not** pass as a whole:

- Dependency Review failed because the repository Dependency Graph is disabled.
- Gitleaks failed on a known synthetic 16-character hexadecimal unit-test credential present in `internal/forecast/client_test.go` and `internal/notify/webhook_test.go` in the scanned history.
- Container builds, architecture tests, vulnerability scans, SBOMs, smoke tests, and Kind deployment were skipped downstream.

Local working-tree changes replace the literals at runtime and add a narrowly scoped scanner allowlist, but they have not passed CI. Dependency Graph enablement remains an owner-controlled repository setting. Neither failure may be relabeled as a successful gate.

## Historical image evidence - 2026-10-21

[CI run 39670162919](https://github.com/krishnamanaiducloud/kuberight/actions/runs/39670162919) on commit `8e7eebe` passed Go 1.27.3 race/build/coverage, Python, frontend, Helm/schema, dependency vulnerability, secret scanning, amd64 and arm64 builds of both images, numeric-user enforcement, final-runtime scientific imports, verified absence of pip/ensurepip/Pebble, Trivy 0.76.0 HIGH/CRITICAL scans, SPDX SBOM generation, amd64 and QEMU-arm64 read-only smoke tests, and an amd64 Kind deployment.

That run also failed overall because Dependency Review could not run with Dependency Graph disabled. More importantly, its image results apply only to `8e7eebe`; they do not validate the current PostgreSQL, migration-hook, provenance, hotspot, optimizer, or dashboard changes.

## Pending repository and registry evidence

- Dependency Graph enablement followed by a successful Dependency Review on the exact candidate.
- A successful Gitleaks scan over the full pull-request history.
- A complete required pull-request workflow and successful `main` workflow for the exact release source.
- Exact-artifact amd64/arm64 builds, vulnerability scans, SBOMs, signatures/attestations, read-only runtime smoke tests, and Kind deployment.
- Validation of the working-tree release path that loads the exact amd64/arm64 archives and runs read-only smoke/runtime-packaging checks before promotion.
- Protected Docker Hub release environment, approval, and immutable tags/digests for `mohankrishna999/kuberight:0.1.0-rc.1` and `mohankrishna999/kuberight-ml:0.1.0-rc.1`.
- Idempotent staged promotion/recovery evidence, Cosign verification, and a GitHub prerelease URL.
- A verified signed OCI Helm-chart publication path, or an explicit statement that the release distributes images only.

No success field may be inferred from the presence of a workflow, Dockerfile, chart, or uncommitted change. `latest` and stable `v0.1.0` publication are prohibited while production readiness remains blocked.
