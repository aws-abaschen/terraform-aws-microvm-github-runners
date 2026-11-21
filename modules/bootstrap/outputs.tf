output "platform_config_parameter" {
  description = "SSM parameter holding the platform configuration read by repo-runner."
  value       = aws_ssm_parameter.platform.name
}

output "kms_key_arn" {
  value = aws_kms_key.this.arn
}

output "artifacts_bucket" {
  value = aws_s3_bucket.this["artifacts"].id
}

output "tfstate_bucket" {
  value = aws_s3_bucket.this["tfstate"].id
}

output "network_connector_arn" {
  value = aws_lambdacore_network_connector.this.arn
}

output "security_group_id" {
  value = aws_security_group.runners.id
}

output "github_app_secret_arn" {
  description = "Put {\"app_id\":123,\"private_key\":\"-----BEGIN...\"} here out of band."
  value       = aws_secretsmanager_secret.app.arn
}

output "broker_function_arn" {
  value = aws_lambda_function.broker.arn
}

output "alarm_topic_arn" {
  value = local.alarm_topic_arn
}

output "repositories" {
  description = "Per repository: id, deploy role and the shared boundary. Store deploy_role_arn as the RUNNERS_DEPLOY_ROLE_ARN variable of the repository."
  value = {
    for repo, r in local.repos : repo => {
      id              = r.id
      deploy_role_arn = aws_iam_role.deploy[repo].arn
      boundary_arn    = aws_iam_policy.boundary.arn
      state_key       = "${r.id}/runners.tfstate"
    }
  }
}
