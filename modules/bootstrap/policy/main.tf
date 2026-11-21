# The generic repository policy: one per account, the same for every
# repository. It is BOTH the deploy role's identity policy and the permissions
# boundary of every repository role. The caller's gha-repo-id and gha-repo
# principal tags pick the repository through policy variables in resource
# ARNs, so the document never grows with the number of repositories.
# Design: docs/iam.html.

terraform {
  required_version = ">= 1.11"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 6.73, < 7.0"
    }
  }
}

variable "name_prefix" { type = string }
variable "partition" { type = string }
variable "region" { type = string }
variable "account" { type = string }
variable "bucket_names" { type = list(string) }
variable "kms_key_arn" { type = string }
variable "broker_function_arn" { type = string }
variable "app_secret_arn" { type = string }
variable "additional_job_actions" {
  type    = list(string)
  default = []
}

locals {
  # Literal IAM policy variables ($$ escapes Terraform interpolation).
  id   = "$${aws:PrincipalTag/gha-repo-id}"
  repo = "$${aws:PrincipalTag/gha-repo}"

  prefix = var.name_prefix
  base   = "${local.prefix}-${local.id}"
  arn = {
    lambda = "arn:${var.partition}:lambda:${var.region}:${var.account}"
    logs   = "arn:${var.partition}:logs:${var.region}:${var.account}"
    iam    = "arn:${var.partition}:iam::${var.account}"
    ssm    = "arn:${var.partition}:ssm:${var.region}:${var.account}"
    sm     = "arn:${var.partition}:secretsmanager:${var.region}:${var.account}"
  }
  policy_name  = "${local.prefix}-boundary"
  boundary_arn = "${local.arn.iam}:policy/${local.policy_name}"
  bucket_arns  = [for b in var.bucket_names : "arn:${var.partition}:s3:::${b}"]
  repo_tags    = ["gha-repo-id", "gha-repo"]
}

data "aws_iam_policy_document" "this" {
  # checkov:skip=CKV_AWS_356:"*" only for actions without resource-level permissions or guarded by conditions.
  # checkov:skip=CKV_AWS_111:Same as above; every write is scoped by name or condition.
  # Everything named after the repository, in one statement to stay under
  # 6,144 characters. An action only matches resources of its own type, so
  # sqs:* applies to the queues, ssm:* to the parameters, and so on.
  statement {
    # Own
    actions = ["lambda:*", "sqs:*", "dynamodb:*", "ssm:*", "secretsmanager:*", "logs:*", "scheduler:*", "cloudwatch:*"]
    resources = [
      "${local.arn.lambda}:function:${local.base}-*",
      "${local.arn.lambda}:microvm-image:${local.base}*",
      "arn:${var.partition}:sqs:${var.region}:${var.account}:${local.base}-*",
      "arn:${var.partition}:dynamodb:${var.region}:${var.account}:table/${local.base}-*",
      "${local.arn.ssm}:parameter/${local.prefix}/${local.id}/*",
      "${local.arn.sm}:secret:${local.prefix}/${local.id}/*",
      "${local.arn.logs}:log-group:/aws/lambda/${local.base}-*",
      "${local.arn.logs}:log-group:/aws/lambda-microvms/${local.base}*",
      "arn:${var.partition}:scheduler:${var.region}:${var.account}:schedule/default/${local.base}-*",
      "arn:${var.partition}:cloudwatch:${var.region}:${var.account}:alarm:${local.base}-*",
    ]
  }
  statement {
    # OwnEventSourceMappings
    actions   = ["lambda:*EventSourceMapping"]
    resources = ["*"]
    condition {
      test     = "ArnLike"
      variable = "lambda:FunctionArn"
      values   = ["${local.arn.lambda}:function:${local.base}-*"]
    }
  }
  statement {
    # CreateOwnImage
    actions   = ["lambda:CreateMicrovmImage"]
    resources = ["*"]
    condition {
      test     = "StringEquals"
      variable = "aws:RequestTag/gha-repo-id"
      values   = [local.id]
    }
  }
  statement {
    # TokenBroker
    actions   = ["lambda:InvokeFunction"]
    resources = [var.broker_function_arn]
  }
  statement {
    # OwnRoles
    actions = [
      "iam:GetRole", "iam:UpdateRole", "iam:DeleteRole", "iam:TagRole", "iam:UntagRole", "iam:PassRole",
      "iam:*RolePolicy", # Put, Get, Delete, Attach, Detach, UpdateAssume
      "iam:List*",       # role policies, attached policies, instance profiles, tags of own roles
    ]
    resources = ["${local.arn.iam}:role/${local.base}-*"]
  }
  # A repository can only create roles of its own: its tags, this boundary.
  statement {
    # CreateOwnRoles
    actions   = ["iam:CreateRole", "iam:PutRolePermissionsBoundary"]
    resources = ["${local.arn.iam}:role/${local.base}-*"]
    condition {
      test     = "StringEquals"
      variable = "iam:PermissionsBoundary"
      values   = [local.boundary_arn]
    }
  }
  statement {
    sid       = "TagOwnRoles"
    effect    = "Deny"
    actions   = ["iam:CreateRole"]
    resources = ["*"]
    condition {
      test     = "StringNotEquals"
      variable = "aws:RequestTag/gha-repo"
      values   = [local.repo]
    }
  }
  statement {
    # PlatformConfig
    actions = ["ssm:GetParameter"]
    resources = [
      "${local.arn.ssm}:parameter/${local.prefix}/platform/config",
      "${local.arn.ssm}:parameter/${local.prefix}/registry/${local.id}",
    ]
  }
  # Job data by convention: gha/<owner>/<repo>/ in Secrets Manager and SSM.
  statement {
    # JobData
    actions = ["secretsmanager:GetSecretValue", "secretsmanager:DescribeSecret", "ssm:GetParameter*"]
    resources = [
      "${local.arn.sm}:secret:gha/${local.repo}/*",
      "${local.arn.ssm}:parameter/gha/${local.repo}/*",
    ]
  }
  # Job data shared by tagging it gha-repo-id=<id>. The SCP lets only that
  # repository and exempt administrators set the tag.
  statement {
    # TaggedJobData
    actions   = ["secretsmanager:GetSecretValue", "secretsmanager:DescribeSecret", "ssm:GetParameter*"]
    resources = ["*"]
    condition {
      test     = "StringEquals"
      variable = "aws:ResourceTag/gha-repo-id"
      values   = [local.id]
    }
  }
  statement {
    # OwnArtifactsAndState
    actions   = ["s3:*Object", "s3:GetObjectVersion", "s3:*ObjectTagging"]
    resources = [for b in local.bucket_arns : "${b}/${local.id}/*"]
  }
  statement {
    # ListOwnPrefix
    actions   = ["s3:ListBucket"]
    resources = local.bucket_arns
    condition {
      test     = "StringLike"
      variable = "s3:prefix"
      values   = ["${local.id}/*"]
    }
  }
  statement {
    # BucketMeta
    actions   = ["s3:GetBucketLocation", "s3:GetBucketVersioning"]
    resources = local.bucket_arns
  }
  statement {
    # PlatformKey
    actions   = ["kms:Encrypt", "kms:Decrypt", "kms:ReEncrypt*", "kms:GenerateDataKey*", "kms:DescribeKey", "kms:CreateGrant"]
    resources = [var.kms_key_arn]
  }
  # Actions without resource-level permissions, or read-only.
  statement {
    # Unscoped
    actions = [
      "lambda:List*", "lambda:GetAccountSettings", "lambda:GetNetworkConnector", "lambda:PassNetworkConnector", "lambda:GetCodeSigningConfig",
      "cloudformation:*Resource*", # Cloud Control (awscc provider) and read-only stack resource calls
      "iam:GetPolicy*", "iam:ListPolicyVersions",
      "ssm:DescribeParameters", "secretsmanager:GetRandomPassword", "logs:DescribeLogGroups", "logs:ListTagsForResource",
      "cloudwatch:PutMetricData", "cloudwatch:DescribeAlarms", "xray:PutT*", "sts:GetCallerIdentity",
      "ec2:CreateNetworkInterface", "ec2:DeleteNetworkInterface", "ec2:*PrivateIpAddresses", # VPC Lambdas
      "ec2:Describe*",                                                                       # read-only; one wildcard keeps the boundary under 6,144 characters
    ]
    resources = ["*"]
  }
  dynamic "statement" {
    for_each = length(var.additional_job_actions) > 0 ? [1] : []
    content {
      # AdditionalJobActions
      actions   = var.additional_job_actions
      resources = ["*"]
    }
  }

  # Guard rails that hold even if an allow above is widened later.
  # The gha-repo-id tag only counts on a role named for that id, or on the
  # shared deploy role, whose session tags only the tagger can set: retagging
  # a role (for example deploy-B with A's id) grants it nothing, even without
  # the organisation SCP.
  statement {
    sid       = "TagMatchesName"
    effect    = "Deny"
    actions   = ["*"]
    resources = ["*"]
    condition {
      test     = "ArnNotLike"
      variable = "aws:PrincipalArn"
      values   = ["${local.arn.iam}:role/${local.base}-*", "${local.arn.iam}:role/${local.prefix}-deploy-${local.id}", "${local.arn.iam}:role/${local.prefix}-deploy"]
    }
  }
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
    resources = [var.app_secret_arn]
  }
  statement {
    sid       = "NoShellIngress"
    effect    = "Deny"
    actions   = ["lambda:CreateMicrovmShellAuthToken", "lambda:CreateMicrovmAuthToken"]
    resources = ["*"]
  }
  # A repository's tags can only ever carry its own values: on create, on
  # TagResource, and no Untag of them. Also enforced organisation-wide by the
  # SCP of modules/org-guardrails.
  dynamic "statement" {
    for_each = local.repo_tags
    content {
      sid       = "NoForeign${replace(title(replace(statement.value, "-", " ")), " ", "")}"
      effect    = "Deny"
      actions   = ["*"]
      resources = ["*"]
      condition {
        test     = "ForAnyValue:StringEquals"
        variable = "aws:TagKeys"
        values   = [statement.value]
      }
      condition {
        test     = "StringNotEquals"
        variable = "aws:RequestTag/${statement.value}"
        values   = ["$${aws:PrincipalTag/${statement.value}}"]
      }
    }
  }
}

output "name" {
  value = local.policy_name
}

output "arn" {
  value = local.boundary_arn
}

output "json" {
  value = data.aws_iam_policy_document.this.json
}

output "minified_json" {
  value = data.aws_iam_policy_document.this.minified_json
}
