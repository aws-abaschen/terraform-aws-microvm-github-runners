output "webhook_url" {
  description = "Payload URL of the repository's workflow_job webhook (content type application/json)."
  value       = aws_lambda_function_url.webhook.function_url
}

output "webhook_secret_arn" {
  description = "Read the value once and paste it as the webhook secret: aws secretsmanager get-secret-value --secret-id <arn>"
  value       = aws_secretsmanager_secret.webhook.arn
}

output "runner_labels" {
  description = "Use these in runs-on. Jobs must ask for every required label."
  value       = local.runner_labels
}

output "runner_role_arn" {
  description = "Ambient role of every job."
  value       = aws_iam_role.runner.arn
}

output "control_role_arn" {
  value = aws_iam_role.control.arn
}

output "queue_arn" {
  value = aws_sqs_queue.jobs.arn
}

output "dlq_arn" {
  value = aws_sqs_queue.dlq.arn
}

output "state_table" {
  value = aws_dynamodb_table.state.name
}

output "repository_id" {
  description = "Short id used in every resource name."
  value       = local.id
}

output "deploy_environment" {
  description = "GitHub Environment the repository deploys from, as onboarded in the registry."
  value       = nonsensitive(jsondecode(data.aws_ssm_parameter.registry.value).environment)
}
