locals {
  common_env = {
    REPOSITORY      = local.repo
    RUNNER_LABELS   = join(",", local.runner_labels)
    REQUIRED_LABELS = join(",", var.required_labels)
    QUEUE_URL       = aws_sqs_queue.jobs.url
  }
  control_env = merge(local.common_env, {
    GITHUB_API_URL       = local.platform.github_api_url
    BROKER_FUNCTION      = local.platform.broker_function_arn
    TABLE_NAME           = aws_dynamodb_table.state.name
    IMAGE_ARN            = var.image_arn
    IMAGE_VERSION        = var.image_version == null ? "" : var.image_version
    RUNNER_ROLE_ARN      = aws_iam_role.runner.arn
    EGRESS_CONNECTORS    = local.platform.network_connector_arn
    INGRESS_CONNECTORS   = local.no_ingress
    KMS_KEY_ARN          = local.kms
    MICROVM_KMS_KEY_ARN  = local.kms
    MICROVM_LOG_GROUP    = aws_cloudwatch_log_group.runtime.name
    PARAM_PREFIX         = local.param_prefix
    MAX_DURATION_SECONDS = tostring(local.max_duration_s)
    MAX_RUNNERS          = tostring(var.max_runners)
    IDLE_TIMEOUT_SECONDS = tostring(var.idle_timeout_minutes * 60)
    RUNNER_GROUP_ID      = tostring(var.runner_group_id)
  })

  functions = {
    webhook = {
      role    = aws_iam_role.webhook.arn
      timeout = 10
      memory  = 256
      env     = merge(local.common_env, { WEBHOOK_SECRET_ARN = aws_secretsmanager_secret.webhook.arn })
      reserve = 20
    }
    dispatcher = {
      role    = aws_iam_role.control.arn
      timeout = 30
      memory  = 256
      env     = local.control_env
      reserve = 5
    }
    sweeper = {
      role    = aws_iam_role.control.arn
      timeout = 90
      memory  = 256
      env     = local.control_env
      reserve = 1
    }
  }
}

resource "aws_lambda_function" "fn" {
  # checkov:skip=CKV_AWS_116:Webhook is synchronous, dispatcher uses the SQS DLQ, sweeper retries on the next schedule.
  for_each                       = local.functions
  function_name                  = "${local.base}-${each.key}"
  description                    = "MicroVM GitHub runners ${each.key} for ${local.repo}"
  role                           = each.value.role
  filename                       = var.lambda_zip_path
  source_code_hash               = filebase64sha256(var.lambda_zip_path)
  handler                        = "bootstrap"
  runtime                        = "provided.al2023"
  architectures                  = ["arm64"]
  memory_size                    = each.value.memory
  timeout                        = each.value.timeout
  reserved_concurrent_executions = var.reserve_concurrency ? each.value.reserve : -1
  kms_key_arn                    = local.kms
  code_signing_config_arn        = var.code_signing_config_arn

  vpc_config {
    subnet_ids         = local.platform.subnet_ids
    security_group_ids = [local.platform.security_group_id]
  }
  tracing_config {
    mode = "Active"
  }
  logging_config {
    log_format = "JSON"
    log_group  = aws_cloudwatch_log_group.fn[each.key].name
  }
  environment {
    variables = merge(each.value.env, { FUNCTION_ROLE = each.key })
  }
  tags = local.tags

  depends_on = [
    aws_iam_role_policy.webhook,
    aws_iam_role_policy.control,
    aws_iam_role_policy_attachment.vpc,
  ]
}

# ---------------------------------------------------------------- Webhook endpoint

resource "aws_lambda_function_url" "webhook" {
  # checkov:skip=CKV_AWS_258:GitHub cannot sign SigV4. Every request is HMAC-verified; use AWS_IAM behind a central ingress where policy requires.
  function_name      = aws_lambda_function.fn["webhook"].function_name
  authorization_type = var.webhook_auth_type
}

resource "aws_lambda_permission" "url_public" {
  # checkov:skip=CKV_AWS_301:Public by design for GitHub webhooks; HMAC-verified.
  count                  = var.webhook_auth_type == "NONE" ? 1 : 0
  statement_id           = "GitHubWebhookUrl"
  action                 = "lambda:InvokeFunctionUrl"
  function_name          = aws_lambda_function.fn["webhook"].function_name
  principal              = "*"
  function_url_auth_type = "NONE"
}

resource "aws_lambda_permission" "url_public_invoke" {
  # checkov:skip=CKV_AWS_301:Public by design for GitHub webhooks; HMAC-verified.
  count                    = var.webhook_auth_type == "NONE" ? 1 : 0
  statement_id             = "GitHubWebhookInvoke"
  action                   = "lambda:InvokeFunction"
  function_name            = aws_lambda_function.fn["webhook"].function_name
  principal                = "*"
  invoked_via_function_url = true
}

resource "aws_lambda_permission" "url_iam" {
  for_each               = var.webhook_auth_type == "AWS_IAM" ? toset(var.webhook_invoker_principals) : toset([])
  statement_id           = "CentralIngress${substr(sha256(each.value), 0, 8)}"
  action                 = "lambda:InvokeFunctionUrl"
  function_name          = aws_lambda_function.fn["webhook"].function_name
  principal              = each.value
  function_url_auth_type = "AWS_IAM"
}

# ---------------------------------------------------------------- Dispatcher trigger

resource "aws_lambda_event_source_mapping" "jobs" {
  event_source_arn        = aws_sqs_queue.jobs.arn
  function_name           = aws_lambda_function.fn["dispatcher"].arn
  batch_size              = 1
  function_response_types = ["ReportBatchItemFailures"]
  scaling_config {
    maximum_concurrency = 2
  }
  tags = local.tags
}

# ---------------------------------------------------------------- Sweeper schedule

resource "aws_iam_role" "scheduler" {
  name                 = "${local.base}-scheduler"
  permissions_boundary = local.boundary
  tags                 = local.tags
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Action    = "sts:AssumeRole"
      Principal = { Service = "scheduler.amazonaws.com" }
      Condition = { StringEquals = { "aws:SourceAccount" = local.account } }
    }]
  })
}

resource "aws_iam_role_policy" "scheduler" {
  role = aws_iam_role.scheduler.id
  policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Action = "lambda:InvokeFunction", Resource = aws_lambda_function.fn["sweeper"].arn }]
  })
}

resource "aws_scheduler_schedule" "sweeper" {
  name                = "${local.base}-sweeper"
  schedule_expression = "rate(${var.sweep_interval_minutes} minutes)"
  kms_key_arn         = local.kms
  flexible_time_window {
    mode = "OFF"
  }
  target {
    arn      = aws_lambda_function.fn["sweeper"].arn
    role_arn = aws_iam_role.scheduler.arn
    retry_policy {
      maximum_retry_attempts       = 0 # the next sweep is the retry
      maximum_event_age_in_seconds = 60
    }
  }
}
