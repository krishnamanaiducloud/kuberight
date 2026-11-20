{{/* Expand the chart name. */}}
{{- define "kuberight.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Create a DNS-safe release name. */}}
{{- define "kuberight.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "kuberight.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Namespace-unique name for cluster-scoped resources. */}}
{{- define "kuberight.clusterScopedName" -}}
{{- $fullname := include "kuberight.fullname" . -}}
{{- $base := $fullname | trunc 38 | trimSuffix "-" -}}
{{- $identityHash := sha256sum (printf "%s/%s" .Release.Namespace $fullname) | trunc 8 -}}
{{- printf "%s-%s" $base $identityHash -}}
{{- end }}

{{- define "kuberight.commonLabels" -}}
helm.sh/chart: {{ include "kuberight.chart" . }}
app.kubernetes.io/name: {{ include "kuberight.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{- define "kuberight.labels" -}}
{{ include "kuberight.commonLabels" . }}
app.kubernetes.io/component: server
{{- end }}

{{- define "kuberight.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kuberight.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: server
{{- end }}

{{- define "kuberight.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "kuberight.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "kuberight.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}
{{- end }}

{{- define "kuberight.annotations" -}}
{{- with .Values.commonAnnotations }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{- define "kuberight.validateValues" -}}
{{- if and .Values.prometheus.bearerTokenFile (hasPrefix "http://" (lower .Values.prometheus.url)) -}}
{{- fail "prometheus.bearerTokenFile requires an https prometheus.url; a token is never sent in cleartext" -}}
{{- end -}}
{{- if and (not .Values.prometheus.url) (or .Values.prometheus.bearerTokenFile .Values.prometheus.caFile .Values.prometheus.tenantNamespace) -}}
{{- fail "prometheus.bearerTokenFile, caFile and tenantNamespace require prometheus.url; leave them empty to use discovery" -}}
{{- end -}}
{{- if and .Values.gitops.provider (not .Values.gitops.token.existingSecret) -}}
{{- fail "gitops.token.existingSecret is required when gitops.provider is set" -}}
{{- end -}}
{{- if and (eq .Values.gitops.provider "gitea") (not .Values.gitops.apiURL) -}}
{{- fail "gitops.apiURL is required for gitea, for example https://git.example.com/api/v1" -}}
{{- end -}}
{{- if and .Values.gitops.apiURL (hasPrefix "http://" .Values.gitops.apiURL) (not .Values.gitops.allowHTTP) -}}
{{- fail "gitops.apiURL uses http; set gitops.allowHTTP=true only for a trusted in-cluster Git server" -}}
{{- end -}}
{{- if and .Values.autoscaling.enabled (not (or .Values.autoscaling.targetCPUUtilizationPercentage .Values.autoscaling.targetMemoryUtilizationPercentage)) -}}
{{- fail "at least one autoscaling CPU or memory utilization target is required when autoscaling.enabled=true" -}}
{{- end -}}
{{- if and .Values.autoscaling.enabled (lt (int .Values.autoscaling.maxReplicas) (int .Values.autoscaling.minReplicas)) -}}
{{- fail "autoscaling.maxReplicas must be greater than or equal to autoscaling.minReplicas" -}}
{{- end -}}
{{- if and (not .Values.serviceAccount.create) (empty .Values.serviceAccount.name) -}}
{{- fail "serviceAccount.name is required when serviceAccount.create=false; refusing to bind KubeRight to the namespace default service account" -}}
{{- end -}}
{{- if and (not .Values.serviceAccount.automountServiceAccountToken) .Values.inventory.enabled -}}
{{- fail "serviceAccount.automountServiceAccountToken must be true when Kubernetes inventory is enabled" -}}
{{- end -}}
{{- if and .Values.route.enabled .Values.route.tls.enabled (ne .Values.route.tls.termination "edge") -}}
{{- fail "route.tls.termination must be edge because KubeRight serves HTTP behind the Route" -}}
{{- end -}}
{{- if and .Values.podDisruptionBudget.minAvailable .Values.podDisruptionBudget.maxUnavailable -}}
{{- fail "set only one of podDisruptionBudget.minAvailable or podDisruptionBudget.maxUnavailable" -}}
{{- end -}}
{{- if and (or (gt (int .Values.replicaCount) 1) .Values.autoscaling.enabled) (not .Values.database.enabled) -}}
{{- fail "replicaCount > 1 or autoscaling.enabled requires database.enabled; the in-process store is not shared between replicas" -}}
{{- end -}}
{{- if and .Values.database.enabled (not .Values.database.existingSecret) -}}
{{- fail "database.existingSecret is required when database.enabled=true" -}}
{{- end -}}
{{- if and .Values.prometheus.url (hasPrefix "http://" .Values.prometheus.url) (not .Values.prometheus.allowHTTP) -}}
{{- fail "prometheus.allowHTTP=true is required for a cleartext http:// Prometheus URL" -}}
{{- end -}}
{{- if ne (empty .Values.cost.cpuCoreHour) (empty .Values.cost.memoryGiBHour) -}}
{{- fail "cost.cpuCoreHour and cost.memoryGiBHour must be set together" -}}
{{- end -}}
{{- end }}

{{/* Converts a Go duration of hours and minutes, such as 1h30m or 15m, to seconds. */}}
{{- define "kuberight.durationSeconds" -}}
{{- $value := toString . -}}
{{- $hours := 0 -}}
{{- $minutes := 0 -}}
{{- with regexFind "[0-9]+h" $value }}{{ $hours = trimSuffix "h" . | int }}{{ end -}}
{{- with regexFind "[0-9]+m" $value }}{{ $minutes = trimSuffix "m" . | int }}{{ end -}}
{{- add (mul $hours 3600) (mul $minutes 60) -}}
{{- end -}}
