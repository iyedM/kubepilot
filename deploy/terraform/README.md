# KubePilot — Terraform Infrastructure-as-Code Module (`deploy/terraform`)

This Terraform module declaratively provisions the **KubePilot** platform alongside its **DevSecOps** and **FinOps** Kubernetes governance boundaries on any conformant cluster (`kind`, `k3s`, AWS EKS, Azure AKS, or OVHcloud Managed Kubernetes Service).

## What This Module Provisions

1. **Pod Security Standards (`restricted`) Namespaces (`kubernetes_namespace_v1`):**
   - `kubepilot-system` and `genai` labeled with `pod-security.kubernetes.io/enforce: restricted` and `opencost.io/cost-center: genai-inference`.
2. **FinOps & DoS Compute Governance (`kubernetes_resource_quota_v1` & `kubernetes_limit_range_v1`):**
   - Enforces explicit CPU and memory requests/limits in the `genai` namespace to prevent unbound CFS starvation or node-level OOM cascades.
3. **Zero-Trust Network Isolation (`kubernetes_network_policy_v1`):**
   - Restricts `kubepilot` egress strictly to cluster DNS (`53`), the Kubernetes API server (`443`/`6443`), and the `observability` namespace telemetry services (Prometheus, Grafana Tempo, OpenCost).
4. **Declarative Helm Release (`helm_release.kubepilot`):**
   - Deploys the local [`charts/kubepilot`](../../charts/kubepilot) chart with strictly read-only RBAC (`verbs: ["get", "list", "watch"]`) and non-root security context (`runAsUser: 65532`, `readOnlyRootFilesystem: true`, `capabilities.drop: ["ALL"]`).

## Quickstart

```bash
cd deploy/terraform

# 1. Initialize providers and validate configuration
terraform init
terraform fmt -check
terraform validate

# 2. Preview the execution plan against your Kubernetes cluster
terraform plan -var="kube_context=kind-kubepilot-lab"

# 3. Apply the stack
terraform apply -var="kube_context=kind-kubepilot-lab"
```
