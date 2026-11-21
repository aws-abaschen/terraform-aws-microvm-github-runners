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
  description = "Per repository: id, deploy role (null without per_repository_deploy_roles) and the shared boundary. Store deploy_role_arn as the RUNNERS_DEPLOY_ROLE_ARN variable of the repository."
  value = {
    for repo, r in local.repos : repo => {
      id              = r.id
      deploy_role_arn = try(aws_iam_role.deploy[repo].arn, null)
      boundary_arn    = aws_iam_policy.boundary.arn
      state_key       = "${r.id}/runners.tfstate"
    }
  }
}

output "tagger" {
  description = "Phase 2 deploy path, null when tagger_github_owners is empty. Repositories store entry_role_arn as RUNNERS_ENTRY_ROLE_ARN and function_name as RUNNERS_TAGGER_FUNCTION."
  value = local.tagger ? {
    function_name   = aws_lambda_function.tagger[0].function_name
    entry_role_arn  = aws_iam_role.deploy_entry[0].arn
    deploy_role_arn = aws_iam_role.deploy_shared[0].arn
    audience        = local.tagger_audience
  } : null
}
