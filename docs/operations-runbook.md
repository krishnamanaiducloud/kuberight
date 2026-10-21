# Operations runbook

This runbook covers the supplied development-preview deployment. No availability SLO, RPO, RTO, or production support commitment has been established.

## First response

Capture the release revision, exact image digest, pod state, events, readiness response, and recent logs before restarting anything:

```sh
helm status kuberight -n kuberight
kubectl -n kuberight get deploy,pod,svc,pdb,networkpolicy -o wide
kubectl -n kuberight describe pod -l app.kubernetes.io/instance=kuberight
kubectl -n kuberight logs deploy/kuberight --all-containers --since=30m
kubectl -n kuberight port-forward service/kuberight 8080:8080
curl -i http://127.0.0.1:8080/healthz
curl -i http://127.0.0.1:8080/readyz
```

Logs are JSON and include request ID, route, status, duration, remote address, and authenticated subject where available. They should not include bearer tokens. Use the response `X-Request-ID` to correlate a client failure.

## Health and readiness

- `/healthz` checks only that the process can serve HTTP. A healthy response does not prove auth, Kubernetes inventory, data freshness, or downstream availability.
- `/readyz` checks the authentication dependency and a configured inventory provider. HTTP 503 with `authentication: unavailable` usually means Kubernetes discovery/review connectivity is failing. `dataProvider: unavailable` points to inventory Kubernetes API access. `not_configured` is expected when inventory is disabled.
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

Optional KEDA or VPA API absence appears as a warning in collection status and should not erase successful core inventory. Namespace scope intentionally does not list nodes or all namespaces. If all records are missing, inspect API audit logs, NetworkPolicy egress, DNS, service-account token mounting, and client-go QPS/burst settings.

KubeRight currently reads inventory on requests; there is no informer cache to repair or resync despite the accepted resync configuration value.

## Metrics and alerts

Scrape `/metrics` only on a trusted network. Useful series are:

- `kuberight_http_requests_in_flight`
- `kuberight_http_requests_total{method,route,status}`
- `kuberight_http_request_duration_seconds_sum{method,route,status}`
- `kuberight_recommendations_total{confidence,risk}`

The duration metric exports a sum but no histogram buckets. A matching `kuberight_http_requests_total` rate can be used for a mean, but latency quantiles are unavailable. The bundled `KubeRightHighHTTPErrorRate` rule selects the emitted `status` label. The deployment-unavailable rule depends on kube-state-metrics; verify both rules against your Prometheus stack before relying on paging behavior.

## Capacity and overload

The Go API bounds request bodies and request duration, but has no cross-replica rate limiter. Watch CPU/memory, throttling, OOM events, in-flight requests, and 429/5xx responses. Do not increase the API replica count or enable its HPA in this preview: recommendation, idempotency, and audit records are process-local, so adding replicas distributes requests across independent stores and breaks coherent replay/history behavior. Horizontal scaling requires an external shared durable store.

The ML service has a bounded concurrency semaphore and can return 429 with `Retry-After: 1`. It can fall back to a deterministic forecast; inspect `fallbackUsed`, `fallbackReasons`, confidence, and warnings rather than treating every 200 response as an ML-model success. Do not increase its replica count or enable its HPA: each pod uses an independent ephemeral model store in the supplied chart.

## Upgrade and rollback

1. Confirm all release gates for the target tag and record both image digests.
2. Back up the current values and render the change.
3. Run API-server dry-run against the target cluster.
4. Upgrade with immutable digests and wait for rollout.
5. Check health, readiness, logs, inventory availability, authentication, and a low-impact recommendation request.

```sh
helm get values kuberight -n kuberight -a > values-before-upgrade.yaml
helm template kuberight charts/kuberight -n kuberight -f values-next.yaml > rendered.yaml
kubectl apply --server-side --dry-run=server -f rendered.yaml
helm upgrade kuberight charts/kuberight -n kuberight -f values-next.yaml --wait --timeout 5m
```

Rollback the chart if validation fails:

```sh
helm history kuberight -n kuberight
helm rollback kuberight <previous-revision> -n kuberight --wait --timeout 5m
```

There is no database schema migration today. API compatibility is still preview-level, so test clients against the target contract. Rollback does not recover process-local history.

## Backup and restore

There is no application backup/restore mechanism. Recommendation, idempotency, and audit history are memory-only and cannot be recovered after a restart. Inventory can be read again from Kubernetes.

ML models are files under `KUBERIGHT_ML_MODEL_DIR`, but the supplied chart stores them in an ephemeral `/tmp` volume. If a custom deployment mounts durable storage, quiesce writes and use the storage provider's consistent snapshot procedure; that topology is not tested by this repository.

The chart does not deploy or configure a database. Durable recommendation and audit recovery will require an application persistence implementation in a future release.

## Credential rotation

- Kubernetes-mode client tokens are issued and rotated by the cluster identity system.
- To rotate a static token or ML API key, update the referenced Secret and restart the affected deployment; the process reads it at startup.
- Rotate registry credentials independently and confirm new pods can pull by digest before revoking old credentials.

Never print secret data in an incident transcript. Kubernetes Secret updates injected as environment variables do not update a running process.

## Escalation evidence

When filing a non-security issue, include sanitized command output, timestamps/time zone, cluster distribution/version, chart revision, image digest, selected profile, readiness JSON, request IDs, and minimal reproduction. Remove bearer tokens, Secret data, kubeconfigs, tenant names, workload samples, and other sensitive identifiers. Report security issues privately according to [SECURITY.md](../SECURITY.md).
