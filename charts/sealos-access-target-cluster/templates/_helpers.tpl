{{- define "sealos-access-target-cluster.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "sealos-access-target-cluster.labels" -}}
helm.sh/chart: {{ include "sealos-access-target-cluster.chart" . }}
app.kubernetes.io/part-of: sealos-access
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{- define "sealos-access-target-cluster.controllerSelectorLabels" -}}
app.kubernetes.io/name: {{ .Values.controller.name }}
{{- end -}}

{{- define "sealos-access-target-cluster.admissionSelectorLabels" -}}
app.kubernetes.io/name: {{ .Values.admission.name }}
{{- end -}}

{{- define "sealos-access-target-cluster.controllerTLSIssuerName" -}}
{{- .Values.controller.tls.issuerName -}}
{{- end -}}

{{- define "sealos-access-target-cluster.admissionTLSIssuerName" -}}
{{- .Values.admission.tls.issuerName -}}
{{- end -}}
