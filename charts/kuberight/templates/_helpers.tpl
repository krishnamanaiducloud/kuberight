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

{{- define "kuberight.mlLabels" -}}
{{ include "kuberight.commonLabels" . }}
app.kubernetes.io/component: ml
{{- end }}

{{- define "kuberight.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kuberight.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: server
{{- end }}

{{- define "kuberight.mlSelectorLabels" -}}
app.kubernetes.io/name: {{ include "kuberight.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: ml
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

{{- define "kuberight.mlImage" -}}
{{- $tag := default .Chart.AppVersion .Values.ml.image.tag -}}
{{- if .Values.ml.image.digest -}}
{{- printf "%s@%s" .Values.ml.image.repository .Values.ml.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.ml.image.repository $tag -}}
{{- end -}}
{{- end }}

{{- define "kuberight.annotations" -}}
{{- with .Values.commonAnnotations }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{- define "kuberight.validateValues" -}}
{{- if and (eq .Values.auth.mode "static") (not .Values.auth.staticToken.existingSecret) -}}
{{- fail "auth.staticToken.existingSecret is required when auth.mode=static" -}}
{{- end -}}
{{- if eq .Values.operationMode "apply" -}}
{{- fail "operationMode=apply is not supported by this chart because KubeRight has no mutating provider" -}}
{{- end -}}
{{- if and .Values.autoscaling.enabled (not (or .Values.autoscaling.targetCPUUtilizationPercentage .Values.autoscaling.targetMemoryUtilizationPercentage)) -}}
{{- fail "at least one autoscaling CPU or memory utilization target is required when autoscaling.enabled=true" -}}
{{- end -}}
{{- if and .Values.autoscaling.enabled (lt (int .Values.autoscaling.maxReplicas) (int .Values.autoscaling.minReplicas)) -}}
{{- fail "autoscaling.maxReplicas must be greater than or equal to autoscaling.minReplicas" -}}
{{- end -}}
{{- if and .Values.ml.enabled .Values.ml.autoscaling.enabled (not .Values.ml.autoscaling.targetCPUUtilizationPercentage) -}}
{{- fail "ml.autoscaling.targetCPUUtilizationPercentage is required when ml.autoscaling.enabled=true" -}}
{{- end -}}
{{- if and .Values.ml.enabled .Values.ml.autoscaling.enabled (lt (int .Values.ml.autoscaling.maxReplicas) (int .Values.ml.autoscaling.minReplicas)) -}}
{{- fail "ml.autoscaling.maxReplicas must be greater than or equal to ml.autoscaling.minReplicas" -}}
{{- end -}}
{{- if and (not .Values.serviceAccount.create) (empty .Values.serviceAccount.name) -}}
{{- fail "serviceAccount.name is required when serviceAccount.create=false; refusing to bind KubeRight to the namespace default service account" -}}
{{- end -}}
{{- if and (not .Values.serviceAccount.automountServiceAccountToken) (or .Values.inventory.enabled (eq .Values.auth.mode "kubernetes")) -}}
{{- fail "serviceAccount.automountServiceAccountToken must be true when Kubernetes inventory or authentication is enabled" -}}
{{- end -}}
{{- if and .Values.ml.enabled (or (gt (int .Values.ml.replicaCount) 1) .Values.ml.autoscaling.enabled) -}}
{{- fail "ml.replicaCount > 1 and ml.autoscaling.enabled are unsupported while model storage is pod-local" -}}
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
{{- if and .Values.ml.enabled .Values.ml.apiKey.existingSecret (not .Values.ml.apiKey.key) -}}
{{- fail "ml.apiKey.key is required when ml.apiKey.existingSecret is set" -}}
{{- end -}}
{{- if ne (empty .Values.cost.cpuCoreHour) (empty .Values.cost.memoryGiBHour) -}}
{{- fail "cost.cpuCoreHour and cost.memoryGiBHour must be set together" -}}
{{- end -}}
{{- end }}
