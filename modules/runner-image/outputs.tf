output "image_arn" {
  value = awscc_lambda_microvm_image.this.image_arn
}

output "image_name" {
  value = awscc_lambda_microvm_image.this.name
}

output "image_version" {
  description = "Latest active version. Pin it in repo-runner to roll forward deliberately."
  value       = awscc_lambda_microvm_image.this.latest_active_image_version
}

output "architecture" {
  value = var.architecture
}
