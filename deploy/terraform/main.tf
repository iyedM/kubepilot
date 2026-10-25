locals {
  common_labels = {
    "app.kubernetes.io/managed-by" = "terraform"
    "app.kubernetes.io/part-of"    = "kubepilot-platform"
  }

  pss_labels = var.enforce_pss_restricted ? {
    "pod-security.kubernetes.io/enforce"         = "restricted"
    "pod-security.kubernetes.io/enforce-version" = "latest"
    "pod-security.kubernetes.io/audit"           = "restricted"
    "pod-security.kubernetes.io/warn"            = "restricted"
  } : {}
}

# 1. Dedicated KubePilot Control-Plane Namespace (PSS-Restricted)
resource "kubernetes_namespace_v1" "kubepilot" {
  metadata {
    name   = var.kubepilot_namespace
    labels = merge(local.common_labels, local.pss_labels)
  }
}

# 2. Target GenAI Workload Namespace with FinOps Cost-Allocation & PSS Labels
resource "kubernetes_namespace_v1" "genai" {
  metadata {
    name = var.target_namespace
    labels = merge(local.common_labels, local.pss_labels, {
      "opencost.io/cost-center" = "genai-inference"
      "kubepilot.io/monitored"  = "true"
    })
  }
}

# 3. FinOps & DoS Governance: ResourceQuota & Default LimitRange on Target Namespace
resource "kubernetes_resource_quota_v1" "genai_quota" {
  count = var.enable_resource_governance ? 1 : 0

  metadata {
    name      = "genai-compute-quota"
    namespace = kubernetes_namespace_v1.genai.metadata[0].name
    labels    = local.common_labels
  }

  spec {
    hard = {
      "requests.cpu"    = "4"
      "limits.cpu"      = var.genai_limits_cpu_quota
      "requests.memory" = "8Gi"
      "limits.memory"   = var.genai_limits_memory_quota
      "pods"            = "20"
    }
  }
}

resource "kubernetes_limit_range_v1" "genai_default_limits" {
  count = var.enable_resource_governance ? 1 : 0

  metadata {
    name      = "genai-default-container-limits"
    namespace = kubernetes_namespace_v1.genai.metadata[0].name
    labels    = local.common_labels
  }

  spec {
    limit {
      type = "Container"
      default = {
        cpu    = "1000m"
        memory = "1Gi"
      }
      default_request = {
        cpu    = "250m"
        memory = "256Mi"
      }
    }
  }
}

# 4. DevSecOps Zero-Trust NetworkPolicy for KubePilot Server
resource "kubernetes_network_policy_v1" "kubepilot_zero_trust" {
  count = var.enable_network_policy ? 1 : 0

  metadata {
    name      = "kubepilot-zero-trust"
    namespace = kubernetes_namespace_v1.kubepilot.metadata[0].name
    labels    = local.common_labels
  }

  spec {
    pod_selector {
      match_labels = {
        "app.kubernetes.io/name" = "kubepilot"
      }
    }

    policy_types = ["Ingress", "Egress"]

    ingress {
      ports {
        protocol = "TCP"
        port     = "8090"
      }
    }

    # Allow DNS resolution to kube-system
    egress {
      ports {
        protocol = "UDP"
        port     = "53"
      }
      ports {
        protocol = "TCP"
        port     = "53"
      }
    }

    # Allow read-only telemetry queries to observability namespace (Prometheus, Tempo, OpenCost)
    egress {
      to {
        namespace_selector {
          match_labels = {
            "kubernetes.io/metadata.name" = var.observability_namespace
          }
        }
      }
      ports {
        protocol = "TCP"
        port     = "80"
      }
      ports {
        protocol = "TCP"
        port     = "3200"
      }
      ports {
        protocol = "TCP"
        port     = "9003"
      }
      ports {
        protocol = "TCP"
        port     = "9090"
      }
    }

    # Allow read-only HTTPS queries to Kubernetes API Server
    egress {
      ports {
        protocol = "TCP"
        port     = "443"
      }
      ports {
        protocol = "TCP"
        port     = "6443"
      }
    }
  }
}

# 5. Declarative Helm Release for Read-Only KubePilot Server & Web UI
resource "helm_release" "kubepilot" {
  name             = "kubepilot"
  chart            = "${path.module}/../../charts/kubepilot"
  namespace        = kubernetes_namespace_v1.kubepilot.metadata[0].name
  create_namespace = false
  atomic           = true
  cleanup_on_fail  = true
  timeout          = 180

  set {
    name  = "image.repository"
    value = var.image_repository
  }

  set {
    name  = "image.tag"
    value = var.image_tag
  }

  set {
    name  = "targetNamespace"
    value = kubernetes_namespace_v1.genai.metadata[0].name
  }

  set {
    name  = "observabilityNamespace"
    value = var.observability_namespace
  }

  set {
    name  = "prometheusURL"
    value = var.prometheus_url
  }

  set {
    name  = "tempoURL"
    value = var.tempo_url
  }

  set {
    name  = "openCostURL"
    value = var.opencost_url
  }

  set {
    name  = "pollInterval"
    value = var.poll_interval
  }
}
