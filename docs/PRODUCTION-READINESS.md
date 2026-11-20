# Production readiness

Status on 2026-11-19: **release candidate**, with the evidence below. It reflects branch `feat/standalone-read-only`, which makes KubeRight read-only and one application Deployment with built-in forecasting, and has not yet run in CI. The previous version of this document (2026-11-18) is superseded.

Readiness is scored out of 100. Only evidence from a test or a live run earns points. A row's deduction names exactly what is still missing.

## Scorecard

| Category | Max | Score | Evidence | Still missing |
| --- | ---: | ---: | --- | --- |
| Metric attribution and data correctness | 15 | 14 | Attribution replayed against a real Prometheus in CI. On a live kind cluster, scale-downs, rollouts, and recreated StatefulSet pods were attributed with 97–99% replica-time coverage. Four live-only defects were fixed. | Runs on larger clusters. |
| Optimizer safety and accuracy | 15 | 13 | 945 production VMs, 2,809 week-ahead evaluations, 95% bootstrap intervals: KubeRight regressions 1.9% (1.5–2.4) against 18.3–86.9% for percentile baselines. A memory-headroom defect was found and fixed, and the fix was confirmed on held-out weeks. | Container traces from a Kubernetes cluster. The trace's memory baseline is assumed. |
| Fleet, policy, and verification | 10 | 10 | PostgreSQL contract suite under the race detector. Three replicas: 224 requests/s; idempotent creates across replicas. Live post-change verification `verified` from exact pod-UID evidence. Early checks flag OOM kills, restarts, and severe throttling or pressure within minutes. On kind, a default scan created at startup produced 19 recommendations without anyone queueing it, and the change window (unit-tested). | — |
| Forecasting | 8 | 6 | Built into the API process (no separate service). Model chosen per series by rolling-origin validation against seasonal naive; conformal intervals. On 188 Bitbrains VMs, 90% intervals cover 92.3% (91.0–93.6) for CPU and 92.4% (91.2–93.5) for memory; it beats seasonal naive on 60% (CPU) and 81% (memory) of VMs. Ran in-cluster on kind against real Prometheus, reporting low confidence and no held-out coverage on 7 hours of history. | CPU median WAPE (14.6%) is above seasonal naive (12.9%). No container traces from a Kubernetes cluster. 30- and 90-day horizons have not run against long-retention Prometheus. |
| Security and authorization | 12 | 11 | Read-only: the ServiceAccount cannot patch, update, or delete workloads, evict pods, or read Secrets (checked with `kubectl auth can-i` on kind). SubjectAccessReview on every request; OpenShift sign-in verifies the forwarded OpenShift token with TokenReview; OIDC single sign-on (live, forged identity headers ignored); separation of duties; Prometheus bearer tokens only over HTTPS. Gitleaks, govulncheck, pip-audit, and Trivy clean on the last CI run. | An external penetration test. |
| Dashboard and accessibility | 8 | 8 | WCAG 2.1 AA audit (axe) on all 20 views at desktop and mobile, live and in CI; a route error boundary. | — |
| Database and operations | 8 | 8 | Upgrade from every earlier schema with data; full down/up round trip; a live `pg_dump`/`pg_restore` identical on every table and served by a second release; Prometheus alerts unit-tested in promtool; Grafana dashboard. | — |
| Kubernetes and OpenShift | 8 | 7 | One application Deployment; live read-only install on kind with recommendations, workload health diagnostics against deliberately broken pods, and in-cluster forecasts. Pod Security `restricted`, arbitrary UIDs with GID 0, read-only root filesystems. OpenShift manifests schema-checked and dry-run against Kubernetes 1.35; OpenShift monitoring discovery and OAuth sign-in unit-tested. A separate debug image with a shell. | A run on an OpenShift cluster: monitoring access, the OAuth login, and the oauth-proxy ImageStream have not run there. |
| GitOps and FinOps | 6 | 5 | Pull requests on GitHub and Gitea/Forgejo and merge requests on GitLab, with in-place manifest edits and Helm values edits (live pull request in Gitea; GitHub and GitLab contract-tested); OpenCost per-namespace prices (live). Node impact separates request reduction, feasible node savings, and unmeasured billing. | A live run against github.com or gitlab.com. |
| Tests, documentation, and release | 10 | 8 | [CI run 43940804651](https://github.com/krishnamanaiducloud/kuberight/actions/runs/43940804651) passed every mandatory job on the candidate, including Dependency Review, both architecture image gates, scans, SBOMs, Kind, and the live browser test. A local multi-architecture image was signed, attested, and verified (a wrong key was rejected). | This branch has not run in CI. A tagged keyless-signed release and verification of its published digests. |
| **Total** | **100** | **90** | | |

The live evidence is summarized in the README's [Live validation](../README.md#live-validation) section, and the accuracy method in [Benchmarks](../README.md#benchmarks).

## Open release gates

1. Run the full quality-gates workflow on `feat/standalone-read-only` and merge it into `main`.
2. With owner approval, tag the merged main commit as `v0.1.0-rc.5`. The release workflow builds once, scans, and keyless-signs the published multi-architecture images and packaged Helm chart. It also attaches SPDX attestations and records the digests in the GitHub release.
3. Verify the published Docker Hub digests, image signatures/attestations, chart checksum/signature bundle, and GitHub prerelease. The immutable `v0.1.0-rc.3` tag remains as a failed, non-publishing startup attempt.

## Scope

KubeRight runs in one cluster per installation. It does not ingest cloud billing or keep a FinOps ledger. It never changes workloads: approved recommendations are applied through GitOps pull requests or your own process, then verified, and a regression is reported, not rolled back.
