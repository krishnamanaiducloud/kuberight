# Security policy

KubeRight is a release candidate and currently has no production-supported release line. The default branch receives security fixes; a version is supported only if a signed release explicitly says so.

## Reporting a vulnerability

Do not open a public issue for a suspected vulnerability. Use the repository's private **Security → Advisories → Report a vulnerability** flow:

<https://github.com/krishnamanaiducloud/kuberight/security/advisories/new>

Include the affected commit or tag, deployment mode, reproduction steps, impact, and any proposed mitigation. Do not include live credentials, cluster data, or other people's personal data. Maintainers will acknowledge the report through GitHub; no response-time SLA is currently offered.

## Security model

- `/api/*` requires an authenticated identity. Kubernetes mode uses `TokenReview`, then sends every API operation through `SubjectAccessReview`. OIDC mode places an oauth2-proxy sidecar in front of a loopback-only API listener and still authorizes the verified identity with `SubjectAccessReview`. Static-token mode grants full application access and is intended only for isolated development; apply mode rejects it.
- `/healthz`, `/readyz`, `/metrics`, and dashboard assets are unauthenticated. Restrict reachability with a NetworkPolicy, ingress policy, or service mesh as appropriate.
- TLS is terminated outside the application. Configure an ingress, OpenShift Route, gateway, or service mesh and do not expose the plain HTTP service directly.
- The chart defaults to a non-root process, dropped Linux capabilities, `RuntimeDefault` seccomp, a read-only root filesystem, resource limits, and an ingress NetworkPolicy.
- API bodies, headers, request duration, and bearer-token length are bounded. Errors do not include dependency internals. Logs include the authenticated subject but must not contain bearer tokens.
- In token mode, the dashboard keeps its bearer token in browser session storage. OIDC mode uses an `HttpOnly`, `Secure`, `SameSite=Lax` session cookie and rejects unauthenticated API requests instead of redirecting them.
- Recommendation creation and approval can enforce separation of duties. Apply mode is disabled by default; when enabled, it requires a namespace opt-in label, fresh approved evidence, live workload identity checks, a resource-version precondition, restricted patch RBAC, audit records, post-change verification, and rollback on detected regression.
- Persisted ML artifacts require an operator-supplied signing key. Their HMAC is verified before deserialization; an invalid artifact is refused and retrained.

## Operator responsibilities

- Use immutable image digests from a successful signed release; do not infer trust from a mutable or locally built tag.
- Grant the KubeRight service account and API users only the RBAC they need. Keep `rbac.scope=namespace` unless cluster-wide inventory is required. Enable cluster-scoped mutation only with `operationMode=apply`, reviewed policies, and namespace opt-in labels.
- Keep the API at one replica while using the in-process repository. Multiple replicas and the API HPA require external PostgreSQL. The ML service remains a singleton because its model store is single-writer, even when backed by a persistent volume.
- Store static tokens, ML API keys, database passwords, and registry credentials in Kubernetes Secrets or an external secret manager. Never commit them to values files.
- Use separate PostgreSQL migration and serving credentials where possible, rehearse backup/restore, and do not assume Helm rollback reverses schema migrations.
- Restrict `/healthz`, `/readyz`, and `/metrics`; optional recommendation metrics expose workload labels. Configure explicit egress rules for the Kubernetes API, PostgreSQL, Prometheus, OpenCost, OIDC, Git providers, webhooks, and DNS before enabling `networkPolicy.egressIsolation`.
- Set `KUBERIGHT_TOKEN_AUDIENCE` where issued tokens have a dedicated audience.
- Treat Git-provider and webhook tokens as write credentials, restrict their repositories/endpoints, and rotate them through Secrets.
- Review [known limitations](docs/known-limitations.md), especially single-cluster scope, external TLS/rate limiting, non-WORM audit storage, and unverified platform environments.

The release workflow is designed to run tests, vulnerability checks, secret scanning, image scans, SBOM generation, multi-architecture execution, chart packaging, keyless signing, attestation, and digest verification before publishing. Those controls are properties of a successful workflow run, not of an arbitrary checkout.
