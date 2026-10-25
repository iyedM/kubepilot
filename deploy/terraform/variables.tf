variable "kubeconfig_path" {
  description = "Path to the kubeconfig file for the target Kubernetes cluster (kind, k3s, EKS, AKS, or OVHcloud MKS)."
  type        = string
  default     = "~/.kube/config"
}

variable "kube_context" {
  description = "Optional kubeconfig context name (empty string uses current-context)."
  type        = string
  default     = ""
}

variable "kubepilot_namespace" {
  description = "Dedicated namespace where the read-only KubePilot server and UI are deployed."
  type        = string
  default     = "kubepilot-system"
}

variable "target_namespace" {
  description = "Target Kubernetes namespace hosting the monitored GenAI inference workloads."
  type        = string
  default     = "genai"
}

variable "observability_namespace" {
  description = "Namespace hosting Prometheus, Grafana Tempo, and OpenCost services."
  type        = string
  default     = "observability"
}

variable "image_repository" {
  description = "Container image repository for KubePilot."
  type        = string
  default     = "ghcr.io/iyedm/kubepilot"
}

variable "image_tag" {
  description = "Immutable container image tag for KubePilot."
  type        = string
  default     = "0.2.1"
}

variable "prometheus_url" {
  description = "In-cluster Prometheus HTTP API base URL."
  type        = string
  default     = "http://prometheus-server.observability.svc.cluster.local:80"
}

variable "tempo_url" {
  description = "In-cluster Grafana Tempo HTTP API base URL."
  type        = string
  default     = "http://tempo.observability.svc.cluster.local:3200"
}

variable "opencost_url" {
  description = "Optional in-cluster OpenCost allocation API base URL for FinOps 2.0 unit economics."
  type        = string
  default     = "http://opencost.observability.svc.cluster.local:9003"
}

variable "poll_interval" {
  description = "Interval between automatic read-only cluster diagnosis passes."
  type        = string
  default     = "60s"
}

variable "enforce_pss_restricted" {
  description = "Enforce Kubernetes Pod Security Standards (restricted profile) labels on managed namespaces."
  type        = bool
  default     = true
}

variable "enable_network_policy" {
  description = "Provision a Zero-Trust Kubernetes NetworkPolicy isolating KubePilot ingress and egress."
  type        = bool
  default     = true
}

variable "enable_resource_governance" {
  description = "Provision a ResourceQuota and LimitRange in the target GenAI namespace to prevent node starvation."
  type        = bool
  default     = true
}

variable "genai_limits_cpu_quota" {
  description = "Maximum aggregate CPU limits allowed in the target GenAI namespace."
  type        = string
  default     = "8"
}

variable "genai_limits_memory_quota" {
  description = "Maximum aggregate Memory limits allowed in the target GenAI namespace."
  type        = string
  default     = "16Gi"
}
