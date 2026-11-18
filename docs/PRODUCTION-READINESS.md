# Production readiness

Status on 2026-11-18: **release candidate**, with the evidence below. The previous version of this document (2026-11-14) is superseded.

Readiness is scored out of 100. Only evidence from a test or a live run earns points. A row's deduction names exactly what is still missing.

## Scorecard

| Category | Max | Score | Evidence | Still missing |
| --- | ---: | ---: | --- | --- |
| Metric attribution and data correctness | 15 | 14 | Attribution replayed against a real Prometheus in CI. On a live kind cluster, scale-downs, rollouts, and recreated StatefulSet pods were attributed with 97–99% replica-time coverage. Four live-only defects were fixed. | Runs on larger clusters. |
| Optimizer safety and accuracy | 15 | 13 | 945 production VMs, 2,809 week-ahead evaluations, 95% bootstrap intervals: KubeRight regressions 1.9% (1.5–2.4) against 18.3–86.9% for percentile baselines. A memory-headroom defect was found and fixed, and the fix was confirmed on held-out weeks. | Container traces from a Kubernetes cluster. The trace's memory baseline is assumed. |
| Fleet, policy, and verification | 10 | 10 | PostgreSQL contract suite under the race detector. Three replicas: 224 requests/s; idempotent creates across replicas; a singleton automation pass under concurrent runs. Live post-change verification `verified` from exact pod-UID evidence. Early checks flag OOM kills, restarts, and severe throttling or pressure within minutes, and rollbacks bypass the change window (unit-tested). | — |
| ML quality and persistence | 8 | 6 | Signed model files verified before unpickling (a live tamper was refused); persistence across pod replacement; 93.9% test coverage. On 200 production VMs, 90% intervals cover 91.4% (89.7–93.2) for CPU, up from 89.6% on the same VMs, and 93.1% for memory. A quantile benchmark against seasonal-naive and LightGBM on the same VMs. | A CPU coverage interval that clears 90%. P95 forecasts that beat seasonal-naive: the service's pinball loss is higher on 57–59% of VMs. |
| Security and authorization | 12 | 11 | SubjectAccessReview on every request; browser single sign-on with any OIDC provider (live, forged identity headers ignored); separation of duties; signed ML models; Gitleaks, govulncheck, pip-audit, and Trivy clean. | An external penetration test. |
| Dashboard and accessibility | 8 | 8 | WCAG 2.1 AA audit (axe) on all 20 views at desktop and mobile, live and in CI; a route error boundary. | — |
| Database and operations | 8 | 8 | Upgrade from every earlier schema with data; full down/up round trip; a live `pg_dump`/`pg_restore` identical on every table and served by a second release; Prometheus alerts unit-tested in promtool; Grafana dashboard. | — |
| Kubernetes and OpenShift | 8 | 7 | Live installs in apply mode. Pod Security `restricted`, arbitrary UIDs with GID 0, read-only root filesystems, and OpenShift's Route CRD. | A run on an OpenShift cluster. |
| GitOps and FinOps | 6 | 5 | Pull requests on GitHub and Gitea/Forgejo and merge requests on GitLab, with in-place manifest edits and Helm values edits (live pull request in Gitea; GitHub and GitLab contract-tested); OpenCost per-namespace prices (live). Node impact separates request reduction, feasible node savings, and unmeasured billing. | A live run against github.com or gitlab.com. |
| Tests, documentation, and release | 10 | 9 | [CI run 43940804651](https://github.com/krishnamanaiducloud/kuberight/actions/runs/43940804651) passed every mandatory job on the candidate, including Dependency Review, both architecture image gates, scans, SBOMs, Kind, and the live browser test. A local multi-architecture image was signed, attested, and verified (a wrong key was rejected). | A tagged keyless-signed release and verification of its published digests. |
| **Total** | **100** | **91** | | |

The live evidence is summarized in the README's [Live validation](../README.md#live-validation) section, and the accuracy method in [Benchmarks](../README.md#benchmarks).

## Open release gates

1. Run the full quality-gates workflow on the `rc.4` release-permission/version correction and merge it into `main`.
2. With owner approval, tag the corrected main commit as `v0.1.0-rc.4`. The release workflow builds once, scans, and keyless-signs the published multi-architecture images and packaged Helm chart. It also attaches SPDX attestations and records the digests in the GitHub release.
3. Verify the published Docker Hub digests, image signatures/attestations, chart checksum/signature bundle, and GitHub prerelease. The immutable `v0.1.0-rc.3` tag remains as a failed, non-publishing startup attempt.

## Scope

KubeRight runs in one cluster per installation. It does not ingest cloud billing or keep a FinOps ledger. Changes are applied only in `apply` mode, only to approved, fresh recommendations, and only in namespaces labeled `kuberight.io/automation=enabled`. Each applied change is verified afterwards and rolled back on regression.
