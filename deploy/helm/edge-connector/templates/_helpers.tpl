{{- define "edge-connector.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "edge-connector.selectorLabels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "edge-connector.labels" -}}
{{ include "edge-connector.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "edge-connector.config" -}}
hub:
  address: {{ required "hub.address is required" .Values.hub.address | quote }}
  {{- with .Values.hub.serverName }}
  server_name: {{ . | quote }}
  {{- end }}
  connections: {{ .Values.hub.connections }}
tls:
  ca_file: /etc/edge-connector/tls/ca.crt
  cert_file: /etc/edge-connector/tls/tls.crt
  key_file: /etc/edge-connector/tls/tls.key
allow:
{{- if not .Values.allow }}
{{- fail "allow must list at least one target" }}
{{- end }}
{{ toYaml .Values.allow | indent 2 }}
limits:
  max_streams: {{ .Values.limits.maxStreams }}
  dial_timeout: {{ .Values.limits.dialTimeout }}
health_listen: ":8080"
{{- end -}}
