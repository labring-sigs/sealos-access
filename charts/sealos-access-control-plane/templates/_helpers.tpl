{{- define "sealos-access-control-plane.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "sealos-access-control-plane.labels" -}}
helm.sh/chart: {{ include "sealos-access-control-plane.chart" . }}
app.kubernetes.io/part-of: sealos-access
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{- define "sealos-access-control-plane.brokerSelectorLabels" -}}
app.kubernetes.io/name: {{ .Values.broker.name }}
{{- end -}}

{{- define "sealos-access-control-plane.adapterSelectorLabels" -}}
app.kubernetes.io/name: {{ .Values.adapter.name }}
{{- end -}}

{{- define "sealos-access-control-plane.brokerTLSIssuerName" -}}
{{- .Values.broker.tls.issuerName -}}
{{- end -}}

{{- define "sealos-access-control-plane.adapterTLSIssuerName" -}}
{{- .Values.adapter.tls.issuerName -}}
{{- end -}}
