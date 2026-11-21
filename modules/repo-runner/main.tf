data "aws_caller_identity" "this" {}
data "aws_region" "this" {}
data "aws_partition" "this" {}

data "aws_ssm_parameter" "platform" {
  name = var.platform_config_parameter
}

locals {
  platform  = jsondecode(data.aws_ssm_parameter.platform.value)
  prefix    = local.platform.name_prefix
  repo      = lower(var.repository)
  id        = substr(sha256(local.repo), 0, 10) # == internal/broker.RepoID
  base      = "${local.prefix}-${local.id}"
  account   = data.aws_caller_identity.this.account_id
  region    = data.aws_region.this.region
  partition = data.aws_partition.this.partition
  boundary  = local.platform.boundary_arn
  kms       = local.platform.kms_key_arn
  retention = local.platform.log_retention_days
  tags      = merge(var.tags, { "gha-repo" = local.repo, "gha-repo-id" = local.id, "managed-by" = "terraform" })

  arch_label     = var.architecture == "X86_64" ? "x64" : "arm64"
  runner_labels  = coalesce(var.runner_labels, ["self-hosted", "linux", local.arch_label, "microvm"])
  max_duration_s = (var.max_job_minutes + 5) * 60
  param_prefix   = "/${local.prefix}/${local.id}"
  no_ingress     = "arn:${local.partition}:lambda:${local.region}:aws:network-connector:aws-network-connector:NO_INGRESS"
}

# Fails when the repository is not onboarded by bootstrap in this account.
data "aws_ssm_parameter" "registry" {
  name = "${local.platform.registry_path}/${local.id}"
  lifecycle {
    postcondition {
      condition     = nonsensitive(jsondecode(self.value).repository) == local.repo
      error_message = "The registry entry of id ${local.id} belongs to another repository (hash prefix collision)."
    }
  }
}

# ---------------------------------------------------------------- Webhook secret (never in state)

ephemeral "random_password" "webhook" {
  length  = 48
  special = false
}

resource "aws_secretsmanager_secret" "webhook" {
  # checkov:skip=CKV2_AWS_57:Rotated by bumping webhook_secret_version and updating the GitHub webhook.
  name                    = "${local.prefix}/${local.id}/webhook-secret"
  description             = "HMAC secret of the GitHub workflow_job webhook for ${local.repo}"
  kms_key_id              = local.kms
  recovery_window_in_days = var.secret_recovery_window_days
  tags                    = local.tags
}

resource "aws_secretsmanager_secret_version" "webhook" {
  secret_id                = aws_secretsmanager_secret.webhook.id
  secret_string_wo         = ephemeral.random_password.webhook.result
  secret_string_wo_version = var.webhook_secret_version
}

# ---------------------------------------------------------------- Queue and state

resource "aws_sqs_queue" "dlq" {
  name                              = "${local.base}-dlq"
  message_retention_seconds         = 1209600
  kms_master_key_id                 = local.kms
  kms_data_key_reuse_period_seconds = 3600
  tags                              = local.tags
}

resource "aws_sqs_queue" "jobs" {
  name                              = "${local.base}-jobs"
  visibility_timeout_seconds        = 180   # 6x dispatcher timeout
  message_retention_seconds         = 86400 # GitHub fails jobs queued > 24 h
  kms_master_key_id                 = local.kms
  kms_data_key_reuse_period_seconds = 3600
  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.dlq.arn
    maxReceiveCount     = 20 # throttled jobs retry for about an hour
  })
  tags = local.tags
}

resource "aws_dynamodb_table" "state" {
  name                        = "${local.base}-state"
  billing_mode                = "PAY_PER_REQUEST"
  hash_key                    = "pk"
  deletion_protection_enabled = var.deletion_protection

  attribute {
    name = "pk"
    type = "S"
  }
  attribute {
    name = "active"
    type = "S"
  }
  attribute {
    name = "started_at"
    type = "N"
  }
  global_secondary_index {
    name            = "active-index"
    projection_type = "ALL"
    key_schema {
      attribute_name = "active"
      key_type       = "HASH"
    }
    key_schema {
      attribute_name = "started_at"
      key_type       = "RANGE"
    }
  }
  ttl {
    attribute_name = "ttl"
    enabled        = true
  }
  point_in_time_recovery {
    enabled = true
  }
  server_side_encryption {
    enabled     = true
    kms_key_arn = local.kms
  }
  tags = local.tags
}

# ---------------------------------------------------------------- Logs

resource "aws_cloudwatch_log_group" "fn" {
  for_each          = toset(["webhook", "dispatcher", "sweeper"])
  name              = "/aws/lambda/${local.base}-${each.key}"
  retention_in_days = local.retention
  kms_key_id        = local.kms
  tags              = local.tags
}

resource "aws_cloudwatch_log_group" "runtime" {
  name              = "/aws/lambda-microvms/${local.base}-runtime"
  retention_in_days = local.retention
  kms_key_id        = local.kms
  tags              = local.tags
}
