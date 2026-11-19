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
{{- if and (eq .Values.operationMode "apply") (not (has .Values.auth.mode (list "kubernetes" "oidc"))) -}}
{{- fail "operationMode=apply requires auth.mode=kubernetes or oidc so every approval and change has an audited identity" -}}
{{- end -}}
{{- if and (eq .Values.operationMode "apply") (ne .Values.rbac.scope "cluster") -}}
{{- fail "operationMode=apply requires rbac.scope=cluster because automation reads the kuberight.io/automation namespace opt-in label" -}}
{{- end -}}
{{- if and (eq .Values.operationMode "apply") (not .Values.rbac.monitoring.enabled) -}}
{{- fail "operationMode=apply requires rbac.monitoring.enabled=true" -}}
{{- end -}}
{{- if eq .Values.auth.mode "oidc" -}}
{{- $oidc := .Values.auth.oidc -}}
{{- if not (and $oidc.issuerURL $oidc.clientID $oidc.existingSecret $oidc.redirectURL) -}}
{{- fail "auth.mode=oidc requires auth.oidc.issuerURL, clientID, existingSecret, and redirectURL" -}}
{{- end -}}
{{- if and (not $oidc.insecureHTTP) (or (hasPrefix "http://" $oidc.issuerURL) (hasPrefix "http://" $oidc.redirectURL)) -}}
{{- fail "auth.oidc URLs must use https; auth.oidc.insecureHTTP=true is for local development only" -}}
{{- end -}}
{{- end -}}
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
{{- if and .Values.ml.enabled .Values.ml.persistence.enabled (not .Values.ml.persistence.signingKey.existingSecret) -}}
{{- fail "ml.persistence.signingKey.existingSecret is required when ml.persistence.enabled=true: persisted models are pickle files and are loaded only after their HMAC is verified" -}}
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
{{- fail "ml.replicaCount > 1 and ml.autoscaling.enabled are unsupported while the model store is single-writer" -}}
{{- end -}}
{{- if and .Values.ml.persistence.enabled (empty .Values.ml.persistence.accessModes) -}}
{{- fail "ml.persistence.accessModes must contain at least one access mode when persistence is enabled" -}}
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

{{/* Converts a Go duration of hours and minutes, such as 1h30m or 15m, to seconds. */}}
{{- define "kuberight.durationSeconds" -}}
{{- $value := toString . -}}
{{- $hours := 0 -}}
{{- $minutes := 0 -}}
{{- with regexFind "[0-9]+h" $value }}{{ $hours = trimSuffix "h" . | int }}{{ end -}}
{{- with regexFind "[0-9]+m" $value }}{{ $minutes = trimSuffix "m" . | int }}{{ end -}}
{{- add (mul $hours 3600) (mul $minutes 60) -}}
{{- end -}}

{{/* The port probes use: the API itself, or the oauth2-proxy in oidc mode. */}}
{{- define "kuberight.probePort" -}}
{{- if eq .Values.auth.mode "oidc" }}4180{{ else }}http{{ end -}}
{{- end }}

{{/* The port the Service and NetworkPolicy expose. */}}
{{- define "kuberight.httpPort" -}}
{{- if eq .Values.auth.mode "oidc" }}4180{{ else }}8080{{ end -}}
{{- end }}

{{/*
oauth2-proxy sidecar for auth.mode=oidc. It performs the OIDC login, keeps
the session in an encrypted cookie, and forwards authenticated requests to
the API on loopback with X-Forwarded-User, -Email, and -Groups.
*/}}
{{- define "kuberight.oauth2ProxyContainer" -}}
{{- $oidc := .Values.auth.oidc -}}
- name: oauth2-proxy
  image: {{ printf "%s%s" $oidc.image.repository (ternary (printf "@%s" $oidc.image.digest) (printf ":%s" $oidc.image.tag) (ne $oidc.image.digest "")) | quote }}
  imagePullPolicy: {{ $oidc.image.pullPolicy }}
  securityContext:
    {{- toYaml .Values.securityContext | nindent 4 }}
  args:
    - --http-address=0.0.0.0:4180
    - --upstream=http://127.0.0.1:8080/
    - --provider=oidc
    - --oidc-issuer-url={{ $oidc.issuerURL }}
    - --client-id={{ $oidc.clientID }}
    - --redirect-url={{ $oidc.redirectURL }}
    - --scope={{ $oidc.scopes }}
    - --oidc-groups-claim={{ $oidc.groupsClaim }}
    {{- range $oidc.emailDomains }}
    - --email-domain={{ . }}
    {{- end }}
    - --pass-user-headers=true
    - --skip-provider-button=true
    # Ask for consent once, not on every login.
    - --approval-prompt=auto
    - --skip-auth-route=^/(healthz|readyz|metrics)$
    - --api-route=^/api/
    - --cookie-name=_kuberight
    - --cookie-samesite=lax
    - --cookie-secure={{ not $oidc.insecureHTTP }}
    - --cookie-expire=8h
    - --cookie-refresh=1h
    - --silence-ping-logging=true
    {{- with $oidc.extraArgs }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
  env:
    - name: OAUTH2_PROXY_CLIENT_SECRET
      valueFrom:
        secretKeyRef:
          name: {{ $oidc.existingSecret | quote }}
          key: {{ $oidc.clientSecretKey | quote }}
    - name: OAUTH2_PROXY_COOKIE_SECRET
      valueFrom:
        secretKeyRef:
          name: {{ $oidc.existingSecret | quote }}
          key: {{ $oidc.cookieSecretKey | quote }}
  ports:
    - name: http
      containerPort: 4180
      protocol: TCP
  readinessProbe:
    httpGet:
      path: /ping
      port: 4180
    periodSeconds: 10
  resources:
    {{- toYaml $oidc.resources | nindent 4 }}
{{- end }}
