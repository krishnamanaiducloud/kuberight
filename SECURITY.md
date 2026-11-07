# Security policy

KubeRight is a development preview and currently has no production-supported release line. The default branch receives security fixes; a version is supported only if a release explicitly says so.

## Reporting a vulnerability

Do not open a public issue for a suspected vulnerability. Use the repository's private **Security → Advisories → Report a vulnerability** flow:

<https://github.com/krishnamanaiducloud/kuberight/security/advisories/new>

Include the affected commit or tag, deployment mode, reproduction steps, impact, and any proposed mitigation. Do not include live credentials, cluster data, or other people's personal data. Maintainers will acknowledge the report through GitHub; no response-time SLA is currently offered.

## Security model

- `/api/*` requires a bearer token. The default Kubernetes mode uses `TokenReview`, then sends every API operation through `SubjectAccessReview`. Static-token mode grants full application access and is intended only for isolated development.
- `/healthz`, `/readyz`, `/metrics`, and dashboard assets are unauthenticated. Restrict reachability with a NetworkPolicy, ingress policy, or service mesh as appropriate.
- TLS is terminated outside the application. Configure an ingress, OpenShift Route, gateway, or service mesh and do not expose the plain HTTP service directly.
- The chart defaults to a non-root process, dropped Linux capabilities, `RuntimeDefault` seccomp, a read-only root filesystem, resource limits, and an ingress NetworkPolicy.
- API bodies, headers, request duration, and bearer-token length are bounded. Errors do not include dependency internals. Logs include the authenticated subject but must not contain bearer tokens.
- The dashboard keeps its bearer token in browser session storage. Treat a browser session and any script executing in the origin as credential-bearing.

## Operator responsibilities

- Use immutable image digests from a successful signed release; do not infer trust from a mutable or locally built tag.
- Grant the KubeRight service account and API users only the RBAC they need. Keep `rbac.scope=namespace` unless cluster-wide read-only inventory is explicitly required; the chart has no apply RBAC or mutating mode.
- Keep the API and optional ML service at one replica with both HPAs disabled. Horizontal scaling is unsupported until recommendation, idempotency, audit, and model state use shared durable storage.
- Store static tokens, ML API keys, database passwords, and registry credentials in Kubernetes Secrets or an external secret manager. Never commit them to values files.
- Restrict `/metrics`; it exposes operational labels and counts. Configure explicit egress rules before enabling `networkPolicy.egressIsolation`.
- Set `KUBERIGHT_TOKEN_AUDIENCE` where issued tokens have a dedicated audience.
- Review [known limitations](docs/known-limitations.md). In particular, application history is in memory and the current API has no apply or GitOps implementation.

The release workflow is designed to run tests, vulnerability checks, secret scanning, image scans, SBOM generation, signing, and digest verification before publishing. Those controls are properties of a successful workflow run, not of an arbitrary checkout.
