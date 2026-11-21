# The token broker is the only principal that can read the GitHub App key.
# Repositories never get the key; they get one-hour tokens scoped to themselves.

resource "aws_secretsmanager_secret" "app" {
  # checkov:skip=CKV2_AWS_57:GitHub App keys are rotated in GitHub; see the rotation runbook.
  name                    = "${local.prefix}/platform/github-app"
  description             = "GitHub App credentials {app_id, private_key}. Set the value out of band; it never enters Terraform state."
  kms_key_id              = aws_kms_key.this.arn
  recovery_window_in_days = var.secret_recovery_window_days
  tags                    = local.tags
}

data "aws_iam_policy_document" "app_secret" {
  statement {
    sid       = "OnlyBrokerReads"
    effect    = "Deny"
    actions   = ["secretsmanager:GetSecretValue"]
    resources = ["*"]
    principals {
      type        = "*"
      identifiers = ["*"]
    }
    condition {
      test     = "StringNotEquals"
      variable = "aws:PrincipalArn"
      values   = [aws_iam_role.broker.arn]
    }
  }
}

resource "aws_secretsmanager_secret_policy" "app" {
  secret_arn = aws_secretsmanager_secret.app.arn
  policy     = data.aws_iam_policy_document.app_secret.json
}

data "aws_iam_policy_document" "lambda_trust" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [local.account]
    }
  }
}

resource "aws_iam_role" "broker" {
  name                 = "${local.prefix}-broker"
  assume_role_policy   = data.aws_iam_policy_document.lambda_trust.json
  permissions_boundary = var.permissions_boundary_arn
  tags                 = local.tags
}

data "aws_iam_policy_document" "broker" {
  statement {
    actions   = ["secretsmanager:GetSecretValue"]
    resources = [aws_secretsmanager_secret.app.arn]
  }
  statement {
    actions   = ["kms:Decrypt"]
    resources = [aws_kms_key.this.arn]
  }
  statement {
    actions   = ["logs:CreateLogStream", "logs:PutLogEvents"]
    resources = ["${aws_cloudwatch_log_group.broker.arn}:*"]
  }
}

resource "aws_iam_role_policy" "broker" {
  role   = aws_iam_role.broker.id
  policy = data.aws_iam_policy_document.broker.json
}

resource "aws_iam_role_policy_attachment" "broker_vpc" {
  role       = aws_iam_role.broker.name
  policy_arn = "arn:${local.partition}:iam::aws:policy/service-role/AWSLambdaVPCAccessExecutionRole"
}

resource "aws_iam_role_policy_attachment" "broker_xray" {
  role       = aws_iam_role.broker.name
  policy_arn = "arn:${local.partition}:iam::aws:policy/AWSXRayDaemonWriteAccess"
}

resource "aws_cloudwatch_log_group" "broker" {
  name              = "/aws/lambda/${local.prefix}-broker"
  retention_in_days = var.log_retention_days
  kms_key_id        = aws_kms_key.this.arn
  tags              = local.tags
}

resource "aws_lambda_function" "broker" {
  # checkov:skip=CKV_AWS_116:Invoked synchronously; callers retry.
  function_name                  = "${local.prefix}-broker"
  description                    = "Vends repository-scoped GitHub installation tokens"
  role                           = aws_iam_role.broker.arn
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
    log_group  = aws_cloudwatch_log_group.broker.name
  }
  environment {
    variables = {
      FUNCTION_ROLE        = "broker"
      APP_SECRET_ARN       = aws_secretsmanager_secret.app.arn
      GITHUB_API_URL       = var.github_api_url
      NAME_PREFIX          = local.prefix
      ACCOUNT_ID           = local.account
      PARTITION            = local.partition
      ALLOWED_REPOSITORIES = join(",", keys(local.repos))
    }
  }
  tags = local.tags
  depends_on = [
    aws_iam_role_policy.broker,
    aws_iam_role_policy_attachment.broker_vpc,
  ]
}

resource "aws_cloudwatch_metric_alarm" "broker_errors" {
  alarm_name          = "${local.prefix}-broker-errors"
  alarm_description   = "Token broker failures: runners cannot register. Runbook: docs/OPERATIONS.md#broker-errors"
  namespace           = "AWS/Lambda"
  metric_name         = "Errors"
  dimensions          = { FunctionName = aws_lambda_function.broker.function_name }
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
