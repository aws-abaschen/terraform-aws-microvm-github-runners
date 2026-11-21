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

# ---------------------------------------------------------------- Webhook

resource "aws_iam_role" "webhook" {
  name                 = "${local.base}-webhook"
  assume_role_policy   = data.aws_iam_policy_document.lambda_trust.json
  permissions_boundary = local.boundary
  tags                 = local.tags
}

data "aws_iam_policy_document" "webhook" {
  statement {
    actions   = ["secretsmanager:GetSecretValue"]
    resources = [aws_secretsmanager_secret.webhook.arn]
  }
  statement {
    actions   = ["sqs:SendMessage"]
    resources = [aws_sqs_queue.jobs.arn]
  }
  statement {
    actions   = ["kms:Decrypt", "kms:GenerateDataKey"]
    resources = [local.kms]
  }
  statement {
    actions   = ["logs:CreateLogStream", "logs:PutLogEvents"]
    resources = ["${aws_cloudwatch_log_group.fn["webhook"].arn}:*"]
  }
}

resource "aws_iam_role_policy" "webhook" {
  role   = aws_iam_role.webhook.id
  policy = data.aws_iam_policy_document.webhook.json
}

# ---------------------------------------------------------------- Control (dispatcher + sweeper)
# The broker only vends tokens to a role named <prefix>-<repo-id>-control.

resource "aws_iam_role" "control" {
  name                 = "${local.base}-control"
  assume_role_policy   = data.aws_iam_policy_document.lambda_trust.json
  permissions_boundary = local.boundary
  tags                 = local.tags
}

data "aws_iam_policy_document" "control" {
  statement {
    sid       = "State"
    actions   = ["dynamodb:GetItem", "dynamodb:PutItem", "dynamodb:UpdateItem", "dynamodb:Query"]
    resources = [aws_dynamodb_table.state.arn, "${aws_dynamodb_table.state.arn}/index/*"]
  }
  statement {
    sid       = "Queue"
    actions   = ["sqs:ReceiveMessage", "sqs:DeleteMessage", "sqs:GetQueueAttributes", "sqs:ChangeMessageVisibility", "sqs:SendMessage"]
    resources = [aws_sqs_queue.jobs.arn]
  }
  statement {
    sid       = "JitParameters"
    actions   = ["ssm:PutParameter", "ssm:DeleteParameter", "ssm:AddTagsToResource"]
    resources = ["arn:${local.partition}:ssm:${local.region}:${local.account}:parameter${local.param_prefix}/jit/*"]
  }
  statement {
    sid       = "Key"
    actions   = ["kms:Encrypt", "kms:Decrypt", "kms:GenerateDataKey*", "kms:ReEncrypt*", "kms:DescribeKey", "kms:CreateGrant"]
    resources = [local.kms]
  }
  statement {
    sid       = "OwnMicroVMs"
    actions   = ["lambda:RunMicrovm", "lambda:GetMicrovm", "lambda:TerminateMicrovm"]
    resources = [var.image_arn]
  }
  statement {
    sid       = "Connectors"
    actions   = ["lambda:PassNetworkConnector"]
    resources = ["*"]
  }
  statement {
    sid       = "PassRunnerRole"
    actions   = ["iam:PassRole"]
    resources = [aws_iam_role.runner.arn]
    condition {
      test     = "StringEquals"
      variable = "iam:PassedToService"
      values   = ["lambda.amazonaws.com"]
    }
  }
  statement {
    sid       = "TokenBroker"
    actions   = ["lambda:InvokeFunction"]
    resources = [local.platform.broker_function_arn]
  }
  statement {
    sid       = "Logs"
    actions   = ["logs:CreateLogStream", "logs:PutLogEvents"]
    resources = ["${aws_cloudwatch_log_group.fn["dispatcher"].arn}:*", "${aws_cloudwatch_log_group.fn["sweeper"].arn}:*"]
  }
}

resource "aws_iam_role_policy" "control" {
  role   = aws_iam_role.control.id
  policy = data.aws_iam_policy_document.control.json
}

# ---------------------------------------------------------------- Runner (ambient credentials of every job)

resource "aws_iam_role" "runner" {
  name                 = "${local.base}-runner"
  description          = "Ambient role of MicroVM jobs of ${local.repo}. Every job gets it, including pull requests."
  assume_role_policy   = data.aws_iam_policy_document.lambda_trust.json
  permissions_boundary = local.boundary
  max_session_duration = 3600
  tags                 = local.tags
}

data "aws_iam_policy_document" "runner" {
  statement {
    sid       = "OwnJitConfigOnce"
    actions   = ["ssm:GetParameter", "ssm:DeleteParameter"]
    resources = ["arn:${local.partition}:ssm:${local.region}:${local.account}:parameter${local.param_prefix}/jit/*"]
  }
  statement {
    sid     = "JobData"
    actions = ["secretsmanager:GetSecretValue", "secretsmanager:DescribeSecret", "ssm:GetParameter", "ssm:GetParameters", "ssm:GetParametersByPath"]
    resources = [
      "arn:${local.partition}:secretsmanager:${local.region}:${local.account}:secret:gha/${local.repo}/*",
      "arn:${local.partition}:ssm:${local.region}:${local.account}:parameter/gha/${local.repo}/*",
    ]
  }
  # Secrets and parameters elsewhere are shared with this repository's jobs by
  # tagging them gha-repo-id=<id> (only this repository and administrators may set that tag).
  statement {
    sid       = "TaggedJobData"
    actions   = ["secretsmanager:GetSecretValue", "secretsmanager:DescribeSecret", "ssm:GetParameter", "ssm:GetParameters"]
    resources = ["*"]
    condition {
      test     = "StringEquals"
      variable = "aws:ResourceTag/gha-repo-id"
      values   = [local.id]
    }
  }
  # The webhook secret also carries the repository tag: jobs must not read it.
  statement {
    sid       = "NoPlatformSecrets"
    effect    = "Deny"
    actions   = ["secretsmanager:*"]
    resources = ["arn:${local.partition}:secretsmanager:${local.region}:${local.account}:secret:${local.prefix}/*"]
  }
  statement {
    sid       = "Decrypt"
    actions   = ["kms:Decrypt"]
    resources = [local.kms]
    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = ["ssm.${local.region}.amazonaws.com", "secretsmanager.${local.region}.amazonaws.com"]
    }
  }
  statement {
    sid       = "RuntimeLogs"
    actions   = ["logs:CreateLogStream", "logs:PutLogEvents"]
    resources = ["${aws_cloudwatch_log_group.runtime.arn}:*"]
  }
}

resource "aws_iam_role_policy" "runner" {
  role   = aws_iam_role.runner.id
  policy = data.aws_iam_policy_document.runner.json
}

resource "aws_iam_role_policy" "runner_additional" {
  count  = var.additional_runner_policy_json == null ? 0 : 1
  name   = "additional"
  role   = aws_iam_role.runner.id
  policy = var.additional_runner_policy_json
}

# ---------------------------------------------------------------- Managed policies for VPC Lambdas

resource "aws_iam_role_policy_attachment" "vpc" {
  for_each   = { webhook = aws_iam_role.webhook.name, control = aws_iam_role.control.name }
  role       = each.value
  policy_arn = "arn:${local.partition}:iam::aws:policy/service-role/AWSLambdaVPCAccessExecutionRole"
}

resource "aws_iam_role_policy_attachment" "xray" {
  for_each   = { webhook = aws_iam_role.webhook.name, control = aws_iam_role.control.name }
  role       = each.value
  policy_arn = "arn:${local.partition}:iam::aws:policy/AWSXRayDaemonWriteAccess"
}
