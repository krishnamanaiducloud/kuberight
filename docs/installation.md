# Installation

KubeRight is a development preview. The commands below describe the supplied packaging; they do not certify a production deployment. Before installing, identify a successful release, inspect its quality-gate run, verify the image signatures, and use the recorded immutable digests. Do not assume the chart's default tag exists.

## Prerequisites

- Kubernetes 1.35 or newer and `kubectl` access.
- Helm 3 for configurable installations.
- Pull access to the selected image registries.
- A CNI that enforces `NetworkPolicy` if network isolation is required.
- Prometheus Operator CRDs only when `metrics.serviceMonitor.enabled` or `metrics.prometheusRule.enabled` is true.
- OpenShift Route API only when `route.enabled` is true.

Source image builds currently depend on digest-pinned bases from `reg.mini.dev`. [Minimus says](https://www.minimus.io/) that registry is scheduled to cease operation on 2026-10-22; mirror or migrate those bases through a reviewed change before attempting later rebuilds. This does not add a runtime dependency to already-published KubeRight application images.

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
      - forecasts
      - autoscalers
      - approvals
      - gitops-changes
    verbs: ["get", "list"]
```

Bind this role only to the intended user, group, or service account. Add `create` on `recommendations` separately for identities that may calculate and store recommendations. A namespaced Role can authorize namespace-qualified requests, while routes without a namespace require cluster-scoped authorization.

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

The chart can deploy the forecasting service, but the Go API and dashboard do not call it yet. Enabling it is useful only for direct, in-cluster API evaluation.

```sh
helm upgrade --install kuberight charts/kuberight \
  --namespace kuberight \
  --set ml.enabled=true \
  --set-string image.digest='sha256:<verified-backend-digest>' \
  --set-string ml.image.digest='sha256:<verified-ml-digest>'
```

Set `KUBERIGHT_ML_API_KEY` through `ml.extraEnv` with a `secretKeyRef` before allowing additional network clients. Chart storage for model artifacts is ephemeral.

## Profiles and platforms

- `profiles/development.yaml` reduces protections for a disposable cluster.
- `profiles/production.yaml` is a production-shaped hardening baseline, not a production certification or an HA topology. It intentionally keeps one API replica, one ML replica when enabled, and both HPAs disabled because their state is process-local. Its monitoring CRs remain disabled; enable them only after installing Prometheus Operator CRDs and configuring the monitoring NetworkPolicy selectors. Environment-specific ingress, secrets, egress, and capacity decisions are still required.
- `profiles/openshift.yaml` renders an edge-terminated Route and non-root settings. Confirm the image and chart against the target SCC and cluster version before use.
- `profiles/cluster-wide.yaml` grants cluster-wide read-only inventory permissions.

All supplied profiles are singleton configurations. Do not increase `replicaCount`, `ml.replicaCount`, or enable either HPA until recommendation, idempotency, audit, and model state use external shared durable storage. Multiple replicas would route requests to independent stores rather than provide coherent application-level high availability.

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

Render and review changes first, pin new digests, then upgrade:

```sh
helm template kuberight charts/kuberight -n kuberight -f current-values.yaml > rendered.yaml
kubectl apply --server-side --dry-run=server -f rendered.yaml
helm upgrade kuberight charts/kuberight -n kuberight -f current-values.yaml --wait
```

Use `helm history kuberight -n kuberight` and `helm rollback kuberight <revision> -n kuberight --wait` for chart rollback. A rollback cannot recover in-memory recommendation or audit history. `helm uninstall kuberight -n kuberight` removes chart-managed resources; inspect PVCs and externally managed secrets separately.

See the [operations runbook](operations-runbook.md) for failure diagnosis and [Helm values](helm-values.md) for configuration details.
