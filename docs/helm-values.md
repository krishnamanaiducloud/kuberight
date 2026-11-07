# Helm values reference

`charts/kuberight/values.yaml` is the source of defaults and `values.schema.json` is the machine-validated contract. Run `helm lint --strict charts/kuberight` and render the exact values before an installation. Values marked as implemented only configure the behavior described here; they do not add integrations listed as unavailable.

## Core service

| Value | Default | Meaning |
| --- | --- | --- |
| `global.imagePullSecrets` | `[]` | Pull secrets shared by primary and ML pods. |
| `nameOverride`, `fullnameOverride` | `""` | Override generated resource names. |
| `namespace.create` | `false` | Render a Namespace resource; Helm users normally use `--create-namespace`. |
| `namespace.labels`, `namespace.annotations` | `{}` | Metadata for the optional Namespace. |
| `replicaCount` | `1` | Primary API replicas. Values above one require `database.enabled=true`; otherwise the chart fails rendering. |
| `image.repository` | `mohankrishna999/kuberight` | Primary image repository. |
| `image.tag` | `""` | Empty selects `Chart.appVersion`; this does not prove that tag exists. |
| `image.digest` | `""` | Preferred immutable `sha256:...` digest; takes precedence over tag. |
| `image.pullPolicy` | `IfNotPresent` | Kubernetes pull policy. |
| `imagePullSecrets` | `[]` | Additional primary-image pull secrets. |
| `operationMode` | `recommend` | `observe` or `recommend`. The chart rejects `apply` because no mutating provider exists. |
| `inventory.enabled` | `true` | Connect to the local in-cluster Kubernetes API. |
| `inventory.clusterID` | `""` | Stable inventory identity; empty uses the release fullname. |
| `inventory.resync` | `10m` | Accepted process setting; there is currently no informer/resync loop. |
| `inventory.qps`, `inventory.burst` | `10`, `20` | client-go rate limits. |
| `service.type`, `service.port` | `ClusterIP`, `8080` | Primary Service type and port. |
| `service.annotations`, `service.clusterIP` | `{}`, `""` | Service metadata and optional fixed cluster IP. |

## Identity and RBAC

| Value | Default | Meaning |
| --- | --- | --- |
| `serviceAccount.create` | `true` | Create the workload ServiceAccount. |
| `serviceAccount.name` | `""` | Existing or overridden ServiceAccount name. Required when `serviceAccount.create=false`; the chart refuses to use the namespace default service account implicitly. |
| `serviceAccount.automountServiceAccountToken` | `true` | Required for Kubernetes auth or inventory. |
| `serviceAccount.annotations` | `{}` | ServiceAccount metadata, including cloud-specific annotations if needed. |
| `rbac.create` | `true` | Render chart-managed RBAC. |
| `rbac.scope` | `namespace` | `namespace` or `cluster` inventory permissions. |
| `rbac.authReviews` | `true` | Grant the service account cluster-scoped TokenReview/SAR creation. |
| `rbac.monitoring.enabled` | `true` | Grant list-only access to pods, workload controllers, and autoscalers queried by inventory. Pod owner references provide exact hotspot attribution; cluster scope also lists nodes and namespaces. |
| `rbac.extraRules` | `[]` | Additional Role/ClusterRole rules; review carefully. |
| `auth.mode` | `kubernetes` | `kubernetes` or development-only `static`. |
| `auth.staticToken.existingSecret` | `""` | Required Secret name in static mode. |
| `auth.staticToken.key` | `token` | Secret data key. Token must be at least 32 characters. |

The chart's RBAC authorizes the workload. API callers separately need permissions on the virtual `kuberight.io` resources described in the [installation guide](installation.md#grant-api-access).

## Exposure and configuration

| Value | Default | Meaning |
| --- | --- | --- |
| `ingress.enabled` | `false` | Render a networking.k8s.io/v1 Ingress. |
| `ingress.className`, `ingress.annotations` | `""`, `{}` | Controller selection and annotations. |
| `ingress.hosts`, `ingress.tls` | example host, `[]` | Host/path rules and TLS secrets. Replace the example before enabling. |
| `route.enabled` | `false` | Render an OpenShift Route. |
| `route.host`, `route.annotations` | `""`, `{}` | Route host and metadata. |
| `route.tls.enabled` | `true` | Add Route TLS configuration. |
| `route.tls.termination` | `edge` | OpenShift termination mode. Only edge termination is supported because the backend listener is HTTP. |
| `route.tls.insecureEdgeTerminationPolicy` | `Redirect` | Plain HTTP handling. |
| `config.existingConfigMap` | `""` | Import environment entries from an existing ConfigMap. |
| `config.data` | `{}` | Create/import non-secret environment entries. |
| `secretEnv`, `extraEnv`, `extraEnvFrom` | empty | Advanced environment injection. Use `secretKeyRef` for secrets. |

TLS is not implemented in the Go listener; terminate it at the Route, Ingress, gateway, or service mesh.

## Persistence, analysis, and integrations

| Value | Default | Meaning |
| --- | --- | --- |
| `database.enabled` | `false` | Use external PostgreSQL for shared recommendations, idempotency, reviews, and audit events. The chart does not deploy a database. |
| `database.existingSecret`, `database.key` | `""`, `url` | Serving credential Secret and key containing a `postgres://` URL. Prefer certificate verification and a role restricted to application DML. |
| `database.migrate` | `true` | Render a bounded `pre-install,pre-upgrade` hook Job that runs the embedded migrations. Serving pods always set migration off and verify the schema. |
| `database.migrationJob.existingSecret`, `.key` | `""`, `url` | Optional separate DDL credential. Empty reuses the serving Secret; production installations should normally separate these roles. |
| `database.migrationJob.activeDeadlineSeconds` | `300` | Hard Kubernetes deadline for the migration Job. The migration command also has a two-minute process context. |
| `database.migrationJob.backoffLimit` | `3` | Failed Job retries. Migrations are checksummed, transactional per version, and serialized by a PostgreSQL advisory lock. |
| `database.migrationJob.annotations` | `{}` | Additional hook Job annotations; Helm's required hook annotations take precedence. |
| `database.maxConns` | `10` | Maximum PostgreSQL pool connections per serving replica. Size the database for the HPA maximum. |
| `prometheus.url` | `""` | Prometheus base URL with cAdvisor-compatible container metrics. |
| `prometheus.allowHTTP` | `false` | Permit cleartext `http://` for an operator-controlled network. HTTPS is preferred. |
| `prometheus.timeout` | `10s` | Per-query client timeout. |
| `prometheus.maxInFlight` | `8` | Global Prometheus request limit per API pod across concurrent feature requests. |
| `recommendations.window`, `.step` | `168h`, `5m` | Default lookback and resolution for generated recommendations. |
| `forecasting.window`, `.step` | `336h`, `1h` | Default Prometheus input window and resolution sent to the optional forecasting service. |
| `approvals.requireDistinctApprover` | `""` | Empty selects secure automatic behavior: true for Kubernetes auth and false for single-identity static auth. |
| `cost.cpuCoreHour`, `cost.memoryGiBHour` | `""`, `""` | Operator-supplied request unit prices. Both must be set together; these are not billing data. |
| `cost.currency` | `USD` | Display currency code for configured-price estimates. |
| `latencyQuery` | `""` | One-line PromQL template in seconds using `$namespace`, `$workload`, or `$container` for signal correlation. |
| `webhook.existingSecret` | `""` | Secret containing the webhook URL and optional signing key. Delivery uses a volatile bounded in-memory queue. |
| `webhook.urlKey`, `.signingSecretKey`, `.format` | `url`, `""`, `json` | Secret keys and `json` or `slack` payload format. |
| `exportRecommendationMetrics` | `false` | Expose per-container recommendation/review/resource gauges. Disabled by default because `/metrics` is public. |

The migration hook applies every pending embedded migration and fails on changed checksums or a database newer than the image. Each serving pod verifies the exact schema before accepting traffic. Setting `database.migrate=false` skips the hook; an operator must run the same image's `migrate` command before installation or upgrade. Helm rollback does not run down migrations, and an older binary will reject a newer schema.

Migration 3 adds a trigger that rejects update, delete, and truncate operations against `audit_events`. This protects against accidental application mutation, but a privileged PostgreSQL administrator can alter database objects; it is not WORM storage.

## Availability, security, and scheduling

| Value | Default | Meaning |
| --- | --- | --- |
| `podAnnotations`, `podLabels`, `deploymentAnnotations` | `{}` | Workload metadata. |
| `podSecurityContext.runAsNonRoot` | `true` | Require a non-root primary process. |
| `podSecurityContext.seccompProfile.type` | `RuntimeDefault` | Pod seccomp profile. |
| `securityContext.allowPrivilegeEscalation` | `false` | Disable privilege escalation. |
| `securityContext.readOnlyRootFilesystem` | `true` | Keep the image filesystem read-only. |
| `securityContext.capabilities.drop` | `[ALL]` | Drop Linux capabilities. |
| `resources` | `100m/128Mi` requests; `1/512Mi` limits | Primary resource requests and limits. |
| `probes.startup`, `probes.readiness`, `probes.liveness` | enabled | Timing controls for `/healthz` and `/readyz`. |
| `terminationGracePeriodSeconds` | `30` | Pod shutdown grace period. |
| `podDisruptionBudget.enabled` | `true` | Render a primary PDB. |
| `podDisruptionBudget.minAvailable` | `1` | Minimum available replicas; mutually exclusive with `maxUnavailable`. With the singleton default, this protects against voluntary disruption but does not provide redundancy. |
| `autoscaling.enabled` | `false` | Render a CPU/memory HPA for the primary deployment. Requires `database.enabled=true`. |
| `autoscaling.minReplicas`, `maxReplicas` | `1`, `1` | API HPA bounds. Account for `database.maxConns` per replica and downstream query load before increasing them. |
| `autoscaling.targetCPUUtilizationPercentage` | `70` | CPU target; memory target and behavior are optional. |
| `topologySpreadConstraints.enabled` | `true` | Spread replicas using the configured key/skew/policy. |
| `nodeSelector`, `tolerations`, `affinity` | empty | Primary scheduling controls. |
| `priorityClassName`, `runtimeClassName` | `""` | Optional runtime scheduling classes. |
| `extraVolumes`, `extraVolumeMounts` | `[]` | Advanced primary pod mounts. |
| `commonLabels`, `commonAnnotations` | `{}` | Metadata shared by chart resources. |

## Network policy and monitoring

| Value | Default | Meaning |
| --- | --- | --- |
| `networkPolicy.enabled` | `true` | Render primary NetworkPolicy. |
| `networkPolicy.ingressFromSameNamespace` | `true` | Allow port 8080 from same-namespace pods. |
| `networkPolicy.ingress` | `[]` | Additional raw ingress rules. |
| `networkPolicy.monitoring.namespaceSelector` | namespace `monitoring` | Namespace selector automatically allowed to scrape when a ServiceMonitor is enabled. |
| `networkPolicy.monitoring.podSelector` | `{}` | Optional Prometheus pod selector within the selected monitoring namespace. |
| `networkPolicy.egressIsolation` | `false` | Add egress policy. Enabling with no rules denies all egress and breaks Kubernetes auth/inventory. |
| `networkPolicy.egress` | `[]` | Explicit raw egress rules; include DNS and Kubernetes API as needed. |
| `metrics.enabled` | `true` | Expose `/metrics` on the Service. The endpoint itself is unauthenticated. |
| `metrics.path` | `/metrics` | Service annotation/monitor path. |
| `metrics.serviceMonitor.enabled` | `false` | Render Prometheus Operator ServiceMonitor. |
| `metrics.serviceMonitor.*` | interval `30s`, timeout `10s` | Labels and scrape/relabel controls. |
| `metrics.prometheusRule.enabled` | `false` | Render the bundled availability and HTTP-error PrometheusRule. |
| `metrics.prometheusRule.labels` | `{}` | Operator discovery labels. |

The HTTP-error alert selects the emitted `status` label. Verify both bundled rules against your Prometheus and kube-state-metrics versions before enabling paging.
The production profile leaves both monitoring CRs disabled, so it does not require Prometheus Operator CRDs unless you explicitly enable them. Change the monitoring NetworkPolicy selectors when Prometheus runs outside the default `monitoring` namespace.

## Optional ML service

`ml.enabled` defaults to `false`. `ml.image` follows the same repository/tag/digest/pull-policy rules as the primary image. The Go API calls the service for `POST /api/v1/forecasts` when ML and Prometheus are configured. `ml.replicaCount` defaults to `1`, and `ml.autoscaling.enabled` defaults to `false` with a `1`/`1` range. The chart rejects multiple ML replicas and ML autoscaling because the supplied deployment stores models on each pod's ephemeral local filesystem. Resources, pod labels/annotations, pod and container security contexts, scheduling controls, image pull secrets, PDB, Service port, and NetworkPolicy are independently configurable under `ml.*`.

Use `ml.apiKey.existingSecret` and `ml.apiKey.key` to share an API key between the Go and ML pods. `ml.extraEnv` supplies other bounded ML settings. The pod has a 256 MiB `/tmp` `emptyDir` and no supplied persistent/shared model volume.

## Profiles

Profiles under `charts/kuberight/profiles` are overlays, not independent charts:

```sh
helm template kuberight charts/kuberight -n kuberight \
  -f charts/kuberight/profiles/development.yaml
```

Always combine the selected profile with environment-specific immutable digests, secrets, TLS, monitoring labels, and network rules.

The default, development, OpenShift, and cluster-wide profiles keep the API at one replica unless combined with another overlay. `profiles/production.yaml` is different: it requires the pre-created `kuberight-database` Secret, enables external PostgreSQL, starts two API replicas, allows an HPA range of 2-6, uses `maxUnavailable: 1`, and spreads across zones with `DoNotSchedule`. It is only a production-shaped baseline. Supply a separate migration credential where possible, size PostgreSQL for the maximum pool count, verify at least two eligible topology domains, and add environment-specific backup/restore, TLS, ingress, egress, monitoring, and capacity settings.

Every profile keeps the ML service at one replica with its HPA disabled. PostgreSQL makes API records shareable; it does not make pod-local ML model files or webhook queues durable.
