# Standalone manifests

These files are deterministic output from `charts/kuberight`; do not edit them by hand.

Regenerate them after a chart change:

```sh
scripts/render-manifests.sh
scripts/render-manifests.sh --check
```

Apply a profile with server-side validation first:

```sh
kubectl apply --server-side --dry-run=server -f deploy/manifests/kuberight.yaml
kubectl apply --server-side -f deploy/manifests/kuberight.yaml
```

`kuberight-production.yaml` leaves the optional `ServiceMonitor` and `PrometheusRule` disabled, so it does not require Prometheus Operator CRDs. `kuberight-openshift.yaml` requires the OpenShift `Route` API. The generated Namespace is named `kuberight`; use Helm when a different namespace or other values are required.

All generated variants intentionally run one API replica with no HPA. The API stores recommendation, idempotency, and audit records in process-local memory, so horizontal scaling is unsupported until an external shared durable store exists. The optional ML deployment follows the same singleton rule because its supplied model store is pod-local and ephemeral.

Generated manifests reference the chart's `appVersion`. Confirm that immutable image tag exists before applying a release artifact.
