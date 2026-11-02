# Installation

KubeRight is a development preview. The commands below describe the supplied packaging; they do not certify a production deployment. Before installing, identify a successful release, inspect its quality-gate run, verify the image signatures, and use the recorded immutable digests. Do not assume the chart's default tag exists.

## Prerequisites

- Kubernetes 1.35 or newer and `kubectl` access.
- Helm 3 for configurable installations.
- Pull access to the selected image registries.
- A CNI that enforces `NetworkPolicy` if network isolation is required.
- Prometheus with cAdvisor-compatible container metrics for history, generated recommendations, efficiency, signals, hotspots, and forecasts; kube-state-metrics adds restart/OOM/rollout evidence.
- External PostgreSQL for durable shared API state or any multi-replica API deployment. The chart does not install or operate PostgreSQL.
- Prometheus Operator CRDs only when `metrics.serviceMonitor.enabled` or `metrics.prometheusRule.enabled` is true.
- OpenShift Route API only when `route.enabled` is true.

Source image builds use digest-pinned official Node, Go, Python, and Ubuntu builders, a Google Distroless static backend runtime, and a Canonical chiselled Python ML runtime. The final images are shell-less and run as positive numeric non-root users. Treat base-digest changes as reviewed release changes and require both architecture gates before publication.

The default authentication mode also requires the KubeRight service account to create `TokenReview` and `SubjectAccessReview` objects. The chart grants those review permissions when `rbac.create=true` and `rbac.authReviews=true`.

## Install with Helm

Use a release digest where possible. Replace the examples with digests from the same verified release:

```sh
kubectl create namespace kuberight

helm upgrade --install kuberight charts/kuberight \
  --namespace kuberight \
  --set-string image.digest='sha256:<verified-backend-digest>' \
  --wait --timeout 5m
```

The default is namespace-scoped inventory. It reads workloads only in `kuberight`. To inspect another namespace, install the release there, or explicitly accept cluster-wide read access:

```sh
helm upgrade --install kuberight charts/kuberight \
  --namespace kuberight --create-namespace \
  -f charts/kuberight/profiles/cluster-wide.yaml \
  --set-string image.digest='sha256:<verified-backend-digest>' \
  --wait --timeout 5m
```

Review the rendered ClusterRole before using the cluster-wide profile. It enables read access; it does not enable recommendation application.

Check the deployment and keep the service private while evaluating it:

```sh
kubectl -n kuberight rollout status deployment/kuberight
kubectl -n kuberight port-forward service/kuberight 8080:8080
curl http://127.0.0.1:8080/healthz
curl http://127.0.0.1:8080/readyz
```

## External PostgreSQL and API high availability

Create a database and two least-privilege credentials through your PostgreSQL/provider workflow:

- a serving role allowed to connect and perform the required application DML; and
- a short-lived migration role allowed to create/alter the KubeRight schema and migration objects.

Use `sslmode=verify-full` with a trusted CA where the provider supports it. Store each URL in a Secret without putting it in shell history or a values file. For example, write each URL to a protected temporary file, create the Secret from that file, and securely remove the file through your operating system's approved procedure:

```sh
kubectl -n kuberight create secret generic kuberight-database \
  --from-file=url=/secure/path/serving-database-url

kubectl -n kuberight create secret generic kuberight-database-migrator \
  --from-file=url=/secure/path/migration-database-url
```

Then enable the database and migration hook:

```yaml
database:
  enabled: true
  existingSecret: kuberight-database
  migrate: true
  migrationJob:
    existingSecret: kuberight-database-migrator
```

The `pre-install,pre-upgrade` hook runs the target image's `migrate` command with a bounded deadline. Migrations are embedded, checksummed, transactional per version, and serialized with a PostgreSQL advisory lock. Serving pods receive the serving Secret, never run DDL, and fail startup unless the schema version/names/checksums exactly match the binary.

When `database.migrate=false`, the chart creates no hook. Run the same target image's `migrate` command through a controlled one-shot Job before the Helm operation, then verify the recorded schema and continue. Do not use a different image version to migrate the database.

## Grant API access

Kubernetes mode authenticates a bearer token and authorizes virtual resources in API group `kuberight.io`. Installation RBAC gives the service account permission to perform reviews; it intentionally does not grant a human or automation account access to the KubeRight API.

For example, a cluster administrator can define a read-only role and bind it to a selected subject:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kuberight-api-viewer
rules:
  - apiGroups: ["kuberight.io"]
    resources:
      - version
      - overview
      - recommendations
      - policies
      - audit
      - integrations
      - clusters
      - namespaces
      - workloads
      - metrics-history
      - hotspots
      - forecasts
      - efficiency
      - savings
      - capacity
      - autoscalers
      - approvals
      - gitops-changes
    verbs: ["get", "list"]
```

Bind this role only to the intended user, group, or service account. Add `create` on `recommendations` for identities that may calculate/store recommendations, `create` on `forecasts` for forecast callers, and `update` on `recommendations/approval` for reviewers. A namespaced Role can authorize namespace-qualified requests, while routes without a namespace require cluster-scoped authorization.

Obtain a suitable token from the cluster's supported identity flow. A short-lived service-account token can be created for testing:

```sh
TOKEN=$(kubectl -n <client-namespace> create token <client-service-account>)
curl -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:8080/api/v1/overview
```

OpenShift OAuth and cluster-configured OIDC tokens can work when the Kubernetes API accepts them through `TokenReview`; KubeRight does not implement a separate OIDC verifier.

## Development-only static authentication

Create the secret before enabling static mode. This one token grants all application permissions and has no built-in expiry.

```sh
openssl rand -hex 32 | kubectl -n kuberight create secret generic kuberight-static-auth \
  --from-file=token=/dev/stdin

helm upgrade --install kuberight charts/kuberight \
  --namespace kuberight \
  --set auth.mode=static \
  --set auth.staticToken.existingSecret=kuberight-static-auth \
  --set-string image.digest='sha256:<verified-backend-digest>'
```

Do not place the token in a values file or command-line `--set` argument.

## Optional ML service

The chart can deploy the forecasting service. When both ML and Prometheus are configured, the Go API queries history and calls it through `POST /api/v1/forecasts`.

```sh
helm upgrade --install kuberight charts/kuberight \
  --namespace kuberight \
  --set ml.enabled=true \
  --set-string image.digest='sha256:<verified-backend-digest>' \
  --set-string ml.image.digest='sha256:<verified-ml-digest>'
```

Set `ml.apiKey.existingSecret` before allowing additional network clients. The chart injects the same key into the Go and ML pods. Chart storage for model artifacts is ephemeral, so the chart rejects multiple ML replicas and ML autoscaling.

## Profiles and platforms

- `profiles/development.yaml` reduces protections for a disposable cluster.
- `profiles/production.yaml` is an HA-shaped API baseline, not production certification. It requires the pre-created `kuberight-database` Secret, enables external PostgreSQL and its migration hook, starts two API replicas, configures an HPA range of 2-6, permits one unavailable replica, and requires zone spreading. Supply a separate migration Secret where possible and verify PostgreSQL capacity, at least two eligible topology domains, backups/restores, ingress/TLS, egress, monitoring, and downstream limits. ML remains a singleton.
- `profiles/openshift.yaml` renders an edge-terminated Route and non-root settings. Confirm the image and chart against the target SCC and cluster version before use.
- `profiles/cluster-wide.yaml` grants cluster-wide read-only inventory permissions.

The chart rejects `replicaCount > 1` or API autoscaling unless `database.enabled=true`. PostgreSQL shares recommendation, idempotency, review, and audit state, but it does not add an inventory cache, a durable webhook outbox, or shared ML model storage. The chart always rejects `ml.replicaCount > 1` and ML autoscaling in the supplied topology.

Example OpenShift render and install:

```sh
helm template kuberight charts/kuberight -n kuberight \
  -f charts/kuberight/profiles/openshift.yaml > /tmp/kuberight-openshift.yaml
oc apply --server-side --dry-run=server -f /tmp/kuberight-openshift.yaml
helm upgrade --install kuberight charts/kuberight -n kuberight --create-namespace \
  -f charts/kuberight/profiles/openshift.yaml \
  --set-string image.digest='sha256:<verified-backend-digest>'
```

EKS, AKS, and GKE use the standard chart; there are no provider-specific integrations or completed provider certification tests. Supply the platform's ingress class, DNS/TLS, storage class, registry authentication, NetworkPolicy rules, and monitoring labels. Cloud workload identity is not a replacement for the in-cluster service-account token used with the Kubernetes API.

## Standalone manifests

`deploy/manifests` is generated from the chart and fixes the namespace and image reference at generation time. Confirm those images exist before applying. Use Helm when customization is needed.

```sh
kubectl apply --server-side --dry-run=server -f deploy/manifests/kuberight.yaml
kubectl apply --server-side -f deploy/manifests/kuberight.yaml
```

The production manifest does not include Prometheus Operator resources by default. The OpenShift manifest needs the Route API.

## Upgrade, rollback, and removal

Render and review changes first, pin new digests, back up external PostgreSQL, and test the backup restore before upgrading. Do not use `--no-hooks` when `database.migrate=true`:

```sh
helm template kuberight charts/kuberight -n kuberight -f current-values.yaml > rendered.yaml
kubectl apply --server-side --dry-run=server -f rendered.yaml
helm upgrade kuberight charts/kuberight -n kuberight -f current-values.yaml --wait
```

The pre-upgrade hook migrates PostgreSQL before new serving pods start. Verify the hook Job, serving-pod schema checks, readiness, authentication, inventory, and a low-impact analysis before accepting the deployment.

Prefer rolling forward if application validation fails after a schema migration. Helm rollback changes Kubernetes resources but does **not** run migration down scripts, and the older binary rejects a newer schema. If a binary rollback is unavoidable, quiesce writes and follow a tested database procedure that restores the pre-upgrade backup to a separate/controlled database endpoint before reverting the application. Do not run embedded down SQL manually against production data without a separately verified recovery plan.

Without PostgreSQL, a rollback or restart cannot recover in-memory recommendation, idempotency, review, or audit history. `helm uninstall kuberight -n kuberight` removes chart-managed resources but not the external database or externally managed Secrets; inventory can be read again from Kubernetes.

See the [operations runbook](operations-runbook.md) for failure diagnosis and [Helm values](helm-values.md) for configuration details.
