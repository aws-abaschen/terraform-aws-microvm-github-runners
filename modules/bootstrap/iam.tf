resource "aws_iam_openid_connect_provider" "github" {
  count          = var.create_github_oidc_provider ? 1 : 0
  url            = "https://${var.github_oidc_host}"
  client_id_list = ["sts.amazonaws.com"]
  tags           = local.tags
}

locals {
  oidc_provider_arn = "arn:${local.partition}:iam::${local.account}:oidc-provider/${var.github_oidc_host}"
  arn = {
    lambda = "arn:${local.partition}:lambda:${local.region}:${local.account}"
    logs   = "arn:${local.partition}:logs:${local.region}:${local.account}"
    iam    = "arn:${local.partition}:iam::${local.account}"
    ssm    = "arn:${local.partition}:ssm:${local.region}:${local.account}"
    sm     = "arn:${local.partition}:secretsmanager:${local.region}:${local.account}"
  }
}

# One policy per repository. It is BOTH the deploy role's identity policy and
# the permissions boundary of the deploy role and of every role the repository
# creates. A repository can therefore only ever touch resources named
# <prefix>-<repo-id>-*, its own SSM/Secrets paths and its own MicroVM image.
data "aws_iam_policy_document" "repo" {
  # checkov:skip=CKV_AWS_356:"*" only for actions without resource-level permissions or guarded by conditions.
  # checkov:skip=CKV_AWS_111:Same as above; every write is scoped by name or condition.
  for_each = local.repos

  statement {
    sid       = "OwnFunctions"
    actions   = ["lambda:*"]
    resources = ["${local.arn.lambda}:function:${each.value.base}-*"]
  }
  statement {
    sid       = "OwnEventSourceMappings"
    actions   = ["lambda:CreateEventSourceMapping", "lambda:UpdateEventSourceMapping", "lambda:DeleteEventSourceMapping", "lambda:GetEventSourceMapping", "lambda:TagResource", "lambda:ListTags"]
    resources = ["*"]
    condition {
      test     = "ArnLike"
      variable = "lambda:FunctionArn"
      values   = ["${local.arn.lambda}:function:${each.value.base}-*"]
    }
  }
  statement {
    sid       = "LambdaRead"
    actions   = ["lambda:ListEventSourceMappings", "lambda:ListFunctions", "lambda:GetAccountSettings", "lambda:ListMicrovms", "lambda:ListMicrovmImages", "lambda:ListManagedMicrovmImages", "lambda:ListManagedMicrovmImageVersions", "lambda:GetNetworkConnector", "lambda:PassNetworkConnector", "lambda:GetCodeSigningConfig"]
    resources = ["*"]
  }
  statement {
    sid       = "CreateOwnImage"
    actions   = ["lambda:CreateMicrovmImage", "lambda:TagResource"]
    resources = ["*"]
    condition {
      test     = "StringEquals"
      variable = "aws:RequestTag/gha-repo-id"
      values   = [each.value.id]
    }
  }
  statement {
    sid       = "OwnImageAndMicroVMs"
    actions   = ["lambda:*Microvm*", "lambda:TagResource", "lambda:UntagResource", "lambda:ListTags"]
    resources = ["${local.arn.lambda}:microvm-image:${each.value.base}*"]
  }
  statement {
    sid       = "CloudControlForAwscc"
    actions   = ["cloudformation:CreateResource", "cloudformation:GetResource", "cloudformation:UpdateResource", "cloudformation:DeleteResource", "cloudformation:GetResourceRequestStatus", "cloudformation:ListResources"]
    resources = ["*"]
  }
  statement {
    sid       = "TokenBroker"
    actions   = ["lambda:InvokeFunction"]
    resources = [aws_lambda_function.broker.arn]
  }
  statement {
    sid = "OwnRoles"
    actions = ["iam:GetRole", "iam:UpdateRole", "iam:DeleteRole", "iam:TagRole", "iam:UntagRole", "iam:PutRolePolicy", "iam:DeleteRolePolicy",
      "iam:GetRolePolicy", "iam:ListRolePolicies", "iam:AttachRolePolicy", "iam:DetachRolePolicy", "iam:ListAttachedRolePolicies",
    "iam:UpdateAssumeRolePolicy", "iam:ListInstanceProfilesForRole", "iam:PassRole", "iam:ListRoleTags"]
    resources = ["${local.arn.iam}:role/${each.value.base}-*"]
  }
  statement {
    sid       = "CreateOwnRolesWithBoundary"
    actions   = ["iam:CreateRole", "iam:PutRolePermissionsBoundary"]
    resources = ["${local.arn.iam}:role/${each.value.base}-*"]
    condition {
      test     = "StringEquals"
      variable = "iam:PermissionsBoundary"
      values   = ["${local.arn.iam}:policy/${local.prefix}-boundary-${each.value.id}"]
    }
  }
  statement {
    sid       = "ReadManagedPolicies"
    actions   = ["iam:GetPolicy", "iam:GetPolicyVersion", "iam:ListPolicyVersions"]
    resources = ["*"]
  }
  statement {
    sid       = "OwnQueues"
    actions   = ["sqs:*"]
    resources = ["arn:${local.partition}:sqs:${local.region}:${local.account}:${each.value.base}-*"]
  }
  statement {
    sid       = "OwnTables"
    actions   = ["dynamodb:*"]
    resources = ["arn:${local.partition}:dynamodb:${local.region}:${local.account}:table/${each.value.base}-*"]
  }
  statement {
    sid       = "OwnParameters"
    actions   = ["ssm:PutParameter", "ssm:GetParameter", "ssm:GetParameters", "ssm:GetParametersByPath", "ssm:DeleteParameter", "ssm:AddTagsToResource", "ssm:ListTagsForResource", "ssm:RemoveTagsFromResource"]
    resources = ["${local.arn.ssm}:parameter/${local.prefix}/${each.value.id}/*"]
  }
  statement {
    sid       = "JobParameters"
    actions   = ["ssm:GetParameter", "ssm:GetParameters", "ssm:GetParametersByPath"]
    resources = [for p in coalesce(each.value.job_parameter_paths, ["/gha/${each.key}/"]) : "${local.arn.ssm}:parameter${trimsuffix(p, "/")}/*"]
  }
  statement {
    sid       = "PlatformConfig"
    actions   = ["ssm:GetParameter"]
    resources = ["${local.arn.ssm}:parameter/${local.prefix}/platform/config"]
  }
  statement {
    sid       = "ParameterMeta"
    actions   = ["ssm:DescribeParameters"]
    resources = ["*"]
  }
  statement {
    sid       = "OwnSecrets"
    actions   = ["secretsmanager:*"]
    resources = ["${local.arn.sm}:secret:${local.prefix}/${each.value.id}/*"]
  }
  statement {
    sid       = "JobSecrets"
    actions   = ["secretsmanager:GetSecretValue", "secretsmanager:DescribeSecret"]
    resources = [for p in coalesce(each.value.job_secret_prefixes, ["gha/${each.key}/"]) : "${local.arn.sm}:secret:${trimsuffix(p, "/")}/*"]
  }
  statement {
    sid       = "SecretsRead"
    actions   = ["secretsmanager:GetRandomPassword"]
    resources = ["*"]
  }
  statement {
    sid     = "OwnLogs"
    actions = ["logs:*"]
    resources = [
      "${local.arn.logs}:log-group:/aws/lambda/${each.value.base}-*",
      "${local.arn.logs}:log-group:/aws/lambda-microvms/${each.value.base}*",
    ]
  }
  statement {
    sid       = "LogsRead"
    actions   = ["logs:DescribeLogGroups", "logs:ListTagsForResource"]
    resources = ["*"]
  }
  statement {
    sid       = "OwnArtifactsAndState"
    actions   = ["s3:GetObject", "s3:GetObjectVersion", "s3:PutObject", "s3:DeleteObject", "s3:GetObjectTagging", "s3:PutObjectTagging"]
    resources = [for b in aws_s3_bucket.this : "${b.arn}/${each.value.id}/*"]
  }
  statement {
    sid       = "ListOwnPrefix"
    actions   = ["s3:ListBucket"]
    resources = [for b in aws_s3_bucket.this : b.arn]
    condition {
      test     = "StringLike"
      variable = "s3:prefix"
      values   = ["${each.value.id}/*"]
    }
  }
  statement {
    sid       = "BucketMeta"
    actions   = ["s3:GetBucketLocation", "s3:GetBucketVersioning"]
    resources = [for b in aws_s3_bucket.this : b.arn]
  }
  statement {
    sid       = "PlatformKey"
    actions   = ["kms:Encrypt", "kms:Decrypt", "kms:ReEncrypt*", "kms:GenerateDataKey*", "kms:DescribeKey", "kms:CreateGrant"]
    resources = [aws_kms_key.this.arn]
  }
  statement {
    sid       = "OwnSchedules"
    actions   = ["scheduler:*"]
    resources = ["arn:${local.partition}:scheduler:${local.region}:${local.account}:schedule/default/${each.value.base}-*"]
  }
  statement {
    sid       = "OwnAlarms"
    actions   = ["cloudwatch:PutMetricAlarm", "cloudwatch:DeleteAlarms", "cloudwatch:DescribeAlarms", "cloudwatch:TagResource", "cloudwatch:UntagResource", "cloudwatch:ListTagsForResource"]
    resources = ["arn:${local.partition}:cloudwatch:${local.region}:${local.account}:alarm:${each.value.base}-*"]
  }
  statement {
    sid       = "Telemetry"
    actions   = ["cloudwatch:PutMetricData", "cloudwatch:DescribeAlarms", "xray:PutTraceSegments", "xray:PutTelemetryRecords", "sts:GetCallerIdentity"]
    resources = ["*"]
  }
  statement {
    sid = "VpcLambda"
    actions = ["ec2:CreateNetworkInterface", "ec2:DeleteNetworkInterface", "ec2:DescribeNetworkInterfaces", "ec2:AssignPrivateIpAddresses",
    "ec2:UnassignPrivateIpAddresses", "ec2:DescribeSubnets", "ec2:DescribeSecurityGroups", "ec2:DescribeVpcs"]
    resources = ["*"]
  }
  dynamic "statement" {
    for_each = length(each.value.additional_job_actions) > 0 ? [1] : []
    content {
      sid       = "AdditionalJobActions"
      actions   = each.value.additional_job_actions
      resources = ["*"]
    }
  }

  # Guard rails that hold even if an allow above is widened later.
  statement {
    sid       = "NoBoundaryEscape"
    effect    = "Deny"
    actions   = ["iam:DeleteRolePermissionsBoundary", "iam:CreatePolicyVersion", "iam:SetDefaultPolicyVersion", "iam:DeletePolicy", "iam:CreateUser", "iam:CreateAccessKey"]
    resources = ["*"]
  }
  statement {
    sid       = "NoPlatformSecret"
    effect    = "Deny"
    actions   = ["secretsmanager:*"]
    resources = [aws_secretsmanager_secret.app.arn]
  }
  statement {
    sid       = "NoShellIngress"
    effect    = "Deny"
    actions   = ["lambda:CreateMicrovmShellAuthToken", "lambda:CreateMicrovmAuthToken"]
    resources = ["*"]
  }
}

resource "aws_iam_policy" "repo" {
  for_each    = local.repos
  name        = "${local.prefix}-boundary-${each.value.id}"
  description = "Deploy policy and permissions boundary for ${each.key}"
  policy      = data.aws_iam_policy_document.repo[each.key].json
  tags        = merge(local.tags, { "gha-repo" = each.key, "gha-repo-id" = each.value.id })
}

data "aws_iam_policy_document" "deploy_trust" {
  # checkov:skip=CKV_AWS_358:aud and sub use StringEquals on one exact repository and environment.
  for_each = local.repos
  statement {
    actions = ["sts:AssumeRoleWithWebIdentity"]
    principals {
      type        = "Federated"
      identifiers = [local.oidc_provider_arn]
    }
    condition {
      test     = "StringEquals"
      variable = "${var.github_oidc_host}:aud"
      values   = ["sts.amazonaws.com"]
    }
    condition {
      test     = "StringEquals"
      variable = "${var.github_oidc_host}:sub"
      values   = ["repo:${each.key}:environment:${each.value.environment}"]
    }
  }
}

resource "aws_iam_role" "deploy" {
  for_each             = local.repos
  name                 = "${local.prefix}-deploy-${each.value.id}"
  description          = "Deploys MicroVM runners for ${each.key} from its ${each.value.environment} environment"
  assume_role_policy   = data.aws_iam_policy_document.deploy_trust[each.key].json
  permissions_boundary = aws_iam_policy.repo[each.key].arn
  max_session_duration = 3600
  tags                 = merge(local.tags, { "gha-repo" = each.key, "gha-repo-id" = each.value.id })
}

resource "aws_iam_role_policy_attachment" "deploy" {
  for_each   = local.repos
  role       = aws_iam_role.deploy[each.key].name
  policy_arn = aws_iam_policy.repo[each.key].arn
}
