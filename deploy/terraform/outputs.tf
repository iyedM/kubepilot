output "kubepilot_namespace" {
  description = "Namespace where the KubePilot control-plane service is deployed."
  value       = kubernetes_namespace_v1.kubepilot.metadata[0].name
}

output "target_namespace" {
  description = "Monitored GenAI workload namespace with PSS-Restricted and FinOps labels."
  value       = kubernetes_namespace_v1.genai.metadata[0].name
}

output "helm_release_status" {
  description = "Deployment status of the KubePilot Helm release."
  value       = helm_release.kubepilot.status
}

output "service_endpoint_in_cluster" {
  description = "In-cluster HTTP endpoint for the KubePilot API and Web UI."
  value       = "http://kubepilot.${kubernetes_namespace_v1.kubepilot.metadata[0].name}.svc.cluster.local:8090"
}

output "port_forward_command" {
  description = "Read-only kubectl port-forward command to open the KubePilot Web Console locally."
  value       = "kubectl -n ${kubernetes_namespace_v1.kubepilot.metadata[0].name} port-forward svc/kubepilot 8090:8090"
}
