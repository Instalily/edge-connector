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

{{/* "true" when the connector enrolls, rather than using tls.existingSecret. */}}
{{- define "edge-connector.enrolls" -}}
{{- if and .Values.tls.existingSecret .Values.enrollment.tokenSecret -}}
{{- fail "set enrollment.tokenSecret or tls.existingSecret, not both" -}}
{{- end -}}
{{- if not .Values.tls.existingSecret }}true{{ end -}}
{{- end -}}

{{- define "edge-connector.config" -}}
hub:
  address: {{ required "hub.address is required" .Values.hub.address | quote }}
  {{- with .Values.hub.serverName }}
  server_name: {{ . | quote }}
  {{- end }}
  connections: {{ .Values.hub.connections }}
{{- if include "edge-connector.enrolls" . }}
state_dir: /var/lib/edge-connector
enrollment:
  token_file: /etc/edge-connector/enrollment/token
{{- else }}
tls:
  ca_file: /etc/edge-connector/tls/ca.crt
  cert_file: /etc/edge-connector/tls/tls.crt
  key_file: /etc/edge-connector/tls/tls.key
{{- end }}
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

{{/* The pod template both the Deployment and the StatefulSet run. */}}
{{- define "edge-connector.podTemplate" -}}
metadata:
  annotations:
    # Roll the pods when the allowlist or other configuration changes.
    checksum/config: {{ include "edge-connector.config" . | sha256sum }}
    {{- with .Values.podAnnotations }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
  labels:
    {{- include "edge-connector.selectorLabels" . | nindent 4 }}
    {{- with .Values.podLabels }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
spec:
  # The connector never calls the Kubernetes API.
  automountServiceAccountToken: false
  enableServiceLinks: false
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    runAsGroup: 65532
    # Makes the state volume writable by the connector.
    fsGroup: 65532
    seccompProfile:
      type: RuntimeDefault
  {{- with .Values.imagePullSecrets }}
  imagePullSecrets:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  containers:
    - name: connector
      {{- $tag := required "image.tag is required" .Values.image.tag }}
      image: "{{ required "image.repository is required" .Values.image.repository }}{{ if hasPrefix "sha256:" $tag }}@{{ else }}:{{ end }}{{ $tag }}"
      imagePullPolicy: {{ .Values.image.pullPolicy }}
      args: ["-config", "/etc/edge-connector/config.yaml"]
      {{- if .Values.proxy }}
      envFrom:
        - secretRef:
            name: {{ include "edge-connector.fullname" . }}-proxy
      {{- end }}
      ports:
        - name: health
          containerPort: 8080
      livenessProbe:
        httpGet:
          path: /healthz
          port: health
      readinessProbe:
        httpGet:
          path: /readyz
          port: health
        periodSeconds: 5
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities:
          drop: ["ALL"]
      resources:
        {{- toYaml .Values.resources | nindent 8 }}
      volumeMounts:
        - name: config
          mountPath: /etc/edge-connector/config.yaml
          subPath: config.yaml
          readOnly: true
        {{- if include "edge-connector.enrolls" . }}
        - name: state
          mountPath: /var/lib/edge-connector
        - name: enrollment
          mountPath: /etc/edge-connector/enrollment
          readOnly: true
        {{- else }}
        - name: tls
          mountPath: /etc/edge-connector/tls
          readOnly: true
        {{- end }}
  volumes:
    - name: config
      configMap:
        name: {{ include "edge-connector.fullname" . }}
    {{- if include "edge-connector.enrolls" . }}
    # Optional, so pods still start once the token Secret is deleted after
    # every replica has enrolled.
    - name: enrollment
      secret:
        secretName: {{ .Values.enrollment.tokenSecret | default (printf "%s-enrollment" (include "edge-connector.fullname" .)) }}
        optional: true
        defaultMode: 0440
        items:
          - key: token
            path: token
    {{- else }}
    - name: tls
      secret:
        secretName: {{ .Values.tls.existingSecret }}
        defaultMode: 0440
    {{- end }}
  topologySpreadConstraints:
    - maxSkew: 1
      topologyKey: kubernetes.io/hostname
      whenUnsatisfiable: ScheduleAnyway
      labelSelector:
        matchLabels:
          {{- include "edge-connector.selectorLabels" . | nindent 10 }}
  {{- with .Values.nodeSelector }}
  nodeSelector:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with .Values.tolerations }}
  tolerations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with .Values.affinity }}
  affinity:
    {{- toYaml . | nindent 4 }}
  {{- end }}
{{- end -}}
