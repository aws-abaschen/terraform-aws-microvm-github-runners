# Phase 2 (docs/iam.html): one shared deploy role for every repository.
#
#   workflow --OIDC (aud sts.amazonaws.com)--> <prefix>-deploy-entry
#            --invoke with a second OIDC token (aud <prefix>-tagger)--> tagger
#   tagger   --sts:AssumeRole + session tags gha-repo-id, gha-repo--> <prefix>-deploy
#
# The shared deploy role gets its repository only from session tags, and only
# the tagger can set them (trust policy in policy/trust.tf, DenyRepoSessionTags in the SCP).
# Onboarding then means writing a registry entry: no role, no policy change.

locals {
  tagger          = length(var.tagger_github_owners) > 0
  tagger_audience = "${local.prefix}-tagger"
  tagger_n        = local.tagger ? 1 : 0
  deploy_shared   = "arn:${local.partition}:iam::${local.account}:role/${local.prefix}-deploy"
}

# ---------------------------------------------------------------- Shared deploy role

resource "aws_iam_role" "deploy_shared" {
  count                = local.tagger_n
  name                 = "${local.prefix}-deploy"
  description          = "Shared deploy role; the tagger sets the repository as session tags"
  assume_role_policy   = module.policy.deploy_shared_trust_json
  permissions_boundary = aws_iam_policy.boundary.arn
  max_session_duration = 3600
  # No gha-* role tags: the session tags are the only source of the repository.
  tags       = local.tags
  depends_on = [aws_iam_role.tagger]
}

resource "aws_iam_role_policy_attachment" "deploy_shared" {
  count      = local.tagger_n
  role       = aws_iam_role.deploy_shared[0].name
  policy_arn = aws_iam_policy.boundary.arn
}

# ---------------------------------------------------------------- Entry role

resource "aws_iam_role" "deploy_entry" {
  count                = local.tagger_n
  name                 = "${local.prefix}-deploy-entry"
  description          = "First hop of repository deploys: may only invoke the tagger"
  assume_role_policy   = module.policy.deploy_entry_trust_json
  permissions_boundary = var.permissions_boundary_arn
  max_session_duration = 3600
  tags                 = local.tags
}

data "aws_iam_policy_document" "deploy_entry" {
  count = local.tagger_n
  statement {
    actions   = ["lambda:InvokeFunction"]
    resources = [aws_lambda_function.tagger[0].arn]
  }
}

resource "aws_iam_role_policy" "deploy_entry" {
  count  = local.tagger_n
  role   = aws_iam_role.deploy_entry[0].id
  policy = data.aws_iam_policy_document.deploy_entry[0].json
}

# ---------------------------------------------------------------- Tagger

resource "aws_dynamodb_table" "tagger_jti" {
  count                       = local.tagger_n
  name                        = "${local.prefix}-tagger-jti"
  billing_mode                = "PAY_PER_REQUEST"
  hash_key                    = "jti"
  deletion_protection_enabled = var.tagger_deletion_protection

  attribute {
    name = "jti"
    type = "S"
  }
  ttl {
    attribute_name = "expires_at"
    enabled        = true
  }
  server_side_encryption {
    enabled     = true
    kms_key_arn = aws_kms_key.this.arn
  }
  point_in_time_recovery {
    enabled = true
  }
  tags = local.tags
}

resource "aws_iam_role" "tagger" {
  count                = local.tagger_n
  name                 = "${local.prefix}-tagger"
  assume_role_policy   = data.aws_iam_policy_document.lambda_trust.json
  permissions_boundary = var.permissions_boundary_arn
  tags                 = local.tags
}

data "aws_iam_policy_document" "tagger" {
  count = local.tagger_n
  statement {
    sid       = "TagSharedDeployRole"
    actions   = ["sts:AssumeRole", "sts:TagSession", "sts:SetSourceIdentity"]
    resources = [local.deploy_shared]
  }
  statement {
    sid       = "Registry"
    actions   = ["ssm:GetParameter"]
    resources = ["arn:${local.partition}:ssm:${local.region}:${local.account}:parameter/${local.prefix}/registry/*"]
  }
  statement {
    sid       = "ReplayStore"
    actions   = ["dynamodb:PutItem"]
    resources = [aws_dynamodb_table.tagger_jti[0].arn]
  }
  statement {
    sid       = "PlatformKey"
    actions   = ["kms:Decrypt", "kms:GenerateDataKey", "kms:DescribeKey"]
    resources = [aws_kms_key.this.arn]
  }
  statement {
    sid       = "Logs"
    actions   = ["logs:CreateLogStream", "logs:PutLogEvents"]
    resources = ["${aws_cloudwatch_log_group.tagger[0].arn}:*"]
  }
}

resource "aws_iam_role_policy" "tagger" {
  count  = local.tagger_n
  role   = aws_iam_role.tagger[0].id
  policy = data.aws_iam_policy_document.tagger[0].json
}

resource "aws_iam_role_policy_attachment" "tagger_vpc" {
  count      = local.tagger_n
  role       = aws_iam_role.tagger[0].name
  policy_arn = "arn:${local.partition}:iam::aws:policy/service-role/AWSLambdaVPCAccessExecutionRole"
}

resource "aws_iam_role_policy_attachment" "tagger_xray" {
  count      = local.tagger_n
  role       = aws_iam_role.tagger[0].name
  policy_arn = "arn:${local.partition}:iam::aws:policy/AWSXRayDaemonWriteAccess"
}

resource "aws_cloudwatch_log_group" "tagger" {
  count             = local.tagger_n
  name              = "/aws/lambda/${local.prefix}-tagger"
  retention_in_days = var.log_retention_days
  kms_key_id        = aws_kms_key.this.arn
  tags              = local.tags
}

resource "aws_lambda_function" "tagger" {
  # checkov:skip=CKV_AWS_116:Invoked synchronously; the workflow retries.
  count                          = local.tagger_n
  function_name                  = "${local.prefix}-tagger"
  description                    = "Exchanges a GitHub OIDC token for a tagged session of the shared deploy role"
  role                           = aws_iam_role.tagger[0].arn
  filename                       = var.lambda_zip_path
  source_code_hash               = filebase64sha256(var.lambda_zip_path)
  handler                        = "bootstrap"
  runtime                        = "provided.al2023"
  architectures                  = ["arm64"]
  memory_size                    = 256
  timeout                        = 15
  reserved_concurrent_executions = 10
  kms_key_arn                    = aws_kms_key.this.arn
  code_signing_config_arn        = var.code_signing_config_arn

  vpc_config {
    subnet_ids         = var.subnet_ids
    security_group_ids = [aws_security_group.runners.id]
  }
  tracing_config {
    mode = "Active"
  }
  logging_config {
    log_format = "JSON"
    log_group  = aws_cloudwatch_log_group.tagger[0].name
  }
  environment {
    variables = {
      FUNCTION_ROLE   = "tagger"
      OIDC_ISSUER     = "https://${var.github_oidc_host}"
      TAGGER_AUDIENCE = local.tagger_audience
      ALLOWED_OWNERS  = lower(join(",", var.tagger_github_owners))
      REGISTRY_PREFIX = "/${local.prefix}/registry"
      JTI_TABLE       = aws_dynamodb_table.tagger_jti[0].name
      DEPLOY_ROLE_ARN = local.deploy_shared
      SESSION_SECONDS = "3600"
    }
  }
  tags = local.tags
  depends_on = [
    aws_iam_role_policy.tagger,
    aws_iam_role_policy_attachment.tagger_vpc,
  ]
}

resource "aws_cloudwatch_metric_alarm" "tagger_errors" {
  count               = local.tagger_n
  alarm_name          = "${local.prefix}-tagger-errors"
  alarm_description   = "Tagger failures: repository deploys cannot start. Runbook: docs/OPERATIONS.md#tagger-errors"
  namespace           = "AWS/Lambda"
  metric_name         = "Errors"
  dimensions          = { FunctionName = aws_lambda_function.tagger[0].function_name }
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 5
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [local.alarm_topic_arn]
  ok_actions          = [local.alarm_topic_arn]
  tags                = local.tags
}
