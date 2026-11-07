# Operations runbook

This runbook covers the supplied development-preview deployment. No availability SLO, RPO, RTO, or production support commitment has been established.

## First response

Capture the release revision, exact image digest, pod state, events, readiness response, and recent logs before restarting anything:

```sh
helm status kuberight -n kuberight
kubectl -n kuberight get deploy,pod,svc,pdb,networkpolicy -o wide
kubectl -n kuberight get job -l app.kubernetes.io/component=migration
kubectl -n kuberight describe pod -l app.kubernetes.io/instance=kuberight
kubectl -n kuberight logs deploy/kuberight --all-containers --since=30m
kubectl -n kuberight port-forward service/kuberight 8080:8080
curl -i http://127.0.0.1:8080/healthz
curl -i http://127.0.0.1:8080/readyz
```

Logs are JSON and include request ID, route, status, duration, remote address, and authenticated subject where available. They should not include bearer tokens. Use the response `X-Request-ID` to correlate a client failure.

## Health and readiness

- `/healthz` checks only that the process can serve HTTP. A healthy response does not prove auth, Kubernetes inventory, data freshness, or downstream availability.
- `/readyz` treats authentication, the recommendation repository, and inventory when configured as critical. HTTP 503 with `authentication: unavailable` usually means Kubernetes discovery/review connectivity is failing. `repository: unavailable` points to PostgreSQL/in-process repository failure. `dataProvider: unavailable` points to inventory Kubernetes API access. Prometheus and forecasting are optional and report `degraded` without failing base readiness; their feature routes can still fail.
- A crash before HTTP startup often means invalid environment configuration or failure to construct an in-cluster client. Inspect logs and the rendered deployment.

Do not weaken authentication or readiness to make a rollout pass. Correct the dependency, service-account token, RBAC, network path, or configuration.

## Authorization failures

For a client receiving 401, confirm it sends one non-expired bearer token accepted by the cluster. For 403, evaluate the same virtual resource KubeRight requests:

```sh
kubectl auth can-i list workloads.kuberight.io --as='<username>' -n '<namespace>'
kubectl auth can-i get overview.kuberight.io --as='<username>' --all-namespaces
```

For service-account review failures, verify the chart binding and service-account identity:

```sh
kubectl auth can-i create tokenreviews.authentication.k8s.io \
  --as=system:serviceaccount:kuberight:kuberight
kubectl auth can-i create subjectaccessreviews.authorization.k8s.io \
  --as=system:serviceaccount:kuberight:kuberight
```

Adjust names for an overridden release or namespace. A 503, rather than 401/403, means the review could not be evaluated and should be handled as a dependency incident.

## Inventory failures or partial data

Check the release scope and service-account rights:

```sh
kubectl auth can-i list deployments.apps -n kuberight \
  --as=system:serviceaccount:kuberight:kuberight
kubectl auth can-i list horizontalpodautoscalers.autoscaling -n kuberight \
  --as=system:serviceaccount:kuberight:kuberight
```

Optional KEDA or VPA API absence appears as a warning in collection status and should not erase successful core inventory. Namespace scope intentionally does not list nodes or all namespaces. Hotspot attribution additionally lists pods and follows their owner references; missing pod-list RBAC makes that route unavailable rather than guessing from pod names. If all records are missing, inspect API audit logs, NetworkPolicy egress, DNS, service-account token mounting, and client-go QPS/burst settings.

KubeRight currently reads inventory on requests; there is no informer cache to repair or resync despite the accepted resync configuration value.

## PostgreSQL and migrations

When PostgreSQL is enabled, confirm the serving and migration Secret references point to the intended database. Production deployments should use distinct DML and DDL roles; if the migration setting intentionally reuses the serving Secret, record and review that privilege exception. Never print or paste Secret values into an incident transcript.

The chart's `pre-install,pre-upgrade` hook runs the target backend image with the `migrate` argument. The Job has a Kubernetes deadline and the process has its own two-minute migration context. It acquires a PostgreSQL advisory lock, verifies all previously recorded names/checksums, and applies each pending version in a transaction. A successful hook is removed; a failed hook normally remains for inspection:

```sh
kubectl -n kuberight get job -l app.kubernetes.io/component=migration
kubectl -n kuberight logs job/kuberight-migrate
kubectl -n kuberight describe job kuberight-migrate
```

Adjust the Job name for an overridden release/fullname. Check Secret keys, DNS/TLS, NetworkPolicy egress, database connection limits, lock contention, and DDL privileges. Do not bypass a failed hook with `--no-hooks`.

Serving pods always set migration off and perform read-only schema verification. They fail startup if a migration is missing, modified, non-contiguous, or newer than the binary. Standalone processes also default `KUBERIGHT_DATABASE_MIGRATE=false`; initialize or advance the schema with a controlled one-shot `kuberight migrate` command using the exact target image.

Migration 3 installs an append-only trigger on `audit_events` that rejects update, delete, and truncate statements. Treat the database administrator boundary separately: a privileged operator can change triggers, so export audit data to an external immutable system when policy requires tamper resistance or long retention.

## Metrics and alerts

Scrape `/metrics` only on a trusted network. Useful series are:

- `kuberight_http_requests_in_flight`
- `kuberight_http_requests_total{method,route,status}`
- `kuberight_http_request_duration_seconds_sum{method,route,status}`
- `kuberight_recommendations_total{confidence,risk}`
- `kuberight_webhook_events_total{outcome}` when webhooks are configured

Optional `kuberight_recommendation_*` gauges expose current/proposed requests and limits, review status, confidence/risk/source, and configured-price savings per container. They are disabled by default because workload labels appear on an unauthenticated endpoint.

The duration metric exports a sum but no histogram buckets. A matching `kuberight_http_requests_total` rate can be used for a mean, but latency quantiles are unavailable. The bundled `KubeRightHighHTTPErrorRate` rule selects the emitted `status` label. The deployment-unavailable rule depends on kube-state-metrics; verify both rules against your Prometheus stack before relying on paging behavior.

Prometheus feature queries are bounded globally within each API pod by `prometheus.maxInFlight` (default 8), in addition to query point, range, response-size, and timeout limits. Increasing API replicas multiplies both this downstream concurrency and the possible database pool size.

## Capacity and overload

The Go API bounds request bodies and request duration, but has no cross-replica client rate limiter. Watch CPU/memory, throttling, OOM events, in-flight requests, PostgreSQL saturation, Prometheus concurrency, and 429/5xx responses. Do not increase API replicas while using the in-process store. With external PostgreSQL, the chart allows scaling and the production-shaped profile uses 2-6 replicas; size `database.maxConns × maxReplicas`, downstream query capacity, topology domains, and PDB behavior before enabling it. No tested scale envelope or availability SLO exists.

The ML service has a bounded concurrency semaphore and can return 429 with `Retry-After: 1`. It can fall back to a deterministic forecast; inspect `fallbackUsed`, `fallbackReasons`, confidence, and warnings rather than treating every 200 response as an ML-model success. Do not increase its replica count or enable its HPA: each pod uses an independent ephemeral model store in the supplied chart.

## Upgrade and rollback

1. Confirm all release gates for the target tag and record both image digests.
2. Back up the current values. If PostgreSQL is enabled, take and test a consistent pre-upgrade backup and record the current schema versions.
3. Render the exact change, including the migration hook, and run API-server dry-run against the target cluster.
4. Upgrade with immutable digests and hooks enabled. The pre-upgrade Job must succeed before the serving rollout.
5. Check hook logs, health, readiness, repository status, authentication, inventory, Prometheus features, and a low-impact dry-run recommendation.

```sh
helm get values kuberight -n kuberight -a > values-before-upgrade.yaml
helm template kuberight charts/kuberight -n kuberight -f values-next.yaml > rendered.yaml
kubectl apply --server-side --dry-run=server -f rendered.yaml
helm upgrade kuberight charts/kuberight -n kuberight -f values-next.yaml --wait --timeout 5m
```

If validation fails before any database migration, a normal chart rollback may be sufficient:

```sh
helm history kuberight -n kuberight
helm rollback kuberight <previous-revision> -n kuberight --wait --timeout 5m
```

After a migration succeeds, prefer rolling forward to a corrected compatible image. Helm rollback does not execute down migrations, and an older binary intentionally refuses a newer schema. If a binary rollback is unavoidable, quiesce writes and follow a rehearsed procedure that restores the pre-upgrade backup to a separate/controlled database endpoint before reverting the application. Do not execute embedded down SQL manually against production data as an emergency shortcut. API compatibility is still preview-level, so test clients against the target contract.

## Backup and restore

KubeRight does not schedule or manage backups. When PostgreSQL is configured, use the database/provider's consistent backup mechanism for both schema and data, encrypt the backup, record the KubeRight image/schema version, and regularly test a restore into an isolated database with the matching image. Recommendations, idempotency claims, reviews, and audit events are all part of that database. The audit trigger is restored as schema; an external immutable audit export remains an operator responsibility.

In in-process repository mode, recommendation, idempotency, review, and audit history cannot be backed up or recovered after restart. Inventory can be read again from Kubernetes and metrics remain in Prometheus.

ML models are files under `KUBERIGHT_ML_MODEL_DIR`, but the supplied chart stores them in an ephemeral `/tmp` volume. If a custom deployment mounts durable storage, quiesce writes and use the storage provider's consistent snapshot procedure; that topology is not tested by this repository.

Webhook events waiting in the in-memory queue are not part of a database backup and have no replay API. Monitor delivered/failed/dropped counters and make receivers idempotent using the delivery identifier.

## Credential rotation

- Kubernetes-mode client tokens are issued and rotated by the cluster identity system.
- To rotate a static token or ML API key, update the referenced Secret and restart the affected deployment; the process reads it at startup.
- Rotate PostgreSQL and webhook Secrets with the external provider, update the Kubernetes Secret, then roll the API pods. A migration credential need not be available to serving pods.
- Rotate registry credentials independently and confirm new pods can pull by digest before revoking old credentials.

Never print secret data in an incident transcript. Kubernetes Secret updates injected as environment variables do not update a running process.

## Escalation evidence

When filing a non-security issue, include sanitized command output, timestamps/time zone, cluster distribution/version, chart revision, image digest, selected profile, readiness JSON, request IDs, and minimal reproduction. Remove bearer tokens, Secret data, kubeconfigs, tenant names, workload samples, and other sensitive identifiers. Report security issues privately according to [SECURITY.md](../SECURITY.md).
