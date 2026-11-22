# Organisation guardrails for MicroVM runner accounts: one SCP, one RCP and
# one tag policy. They hold only the name prefix and the exempt roles, so they
# are the same for every account (cell) and every number of repositories.
# Apply from the management account or a delegated administrator.
# Design: docs/iam.html.

locals {
  prefix  = var.name_prefix
  tagger  = "arn:${var.partition}:iam::*:role/${local.prefix}-tagger"
  exempt  = concat(var.exempt_principal_arns, [local.tagger])
  keys    = ["gha-repo-id", "gha-repo"]
  pt      = { for k in local.keys : k => "$${aws:PrincipalTag/${k}}" }
  rt_id   = "$${aws:ResourceTag/gha-repo-id}"
  iam_arn = "arn:${var.partition}:iam::*"

  platform_iam = [
    "${local.iam_arn}:policy/${local.prefix}-boundary",
    "${local.iam_arn}:role/${local.prefix}-broker",
    "${local.iam_arn}:role/${local.prefix}-deploy*", # shared, entry and per-repository deploy roles
    "${local.iam_arn}:role/${local.prefix}-tagger*",
    "${local.iam_arn}:role/${local.prefix}-connector-operator",
  ]

  scp = {
    Version = "2012-10-17"
    Statement = concat(
      # Only a repository (or an exempt administrator) may set or remove its
      # own repository tags. Covers create-with-tags, Tag* and Untag* on every
      # service, IAM principals and STS sessions included.
      [for k in local.keys : {
        Sid      = "DenyForeign${replace(title(replace(k, "-", " ")), " ", "")}"
        Effect   = "Deny"
        Action   = "*"
        Resource = "*"
        Condition = {
          "ForAnyValue:StringEquals" = { "aws:TagKeys" = [k] }
          StringNotEquals            = { "aws:RequestTag/${k}" = local.pt[k] }
          ArnNotLike                 = { "aws:PrincipalArn" = local.exempt }
        }
      }],
      [
        {
          # A repository can't move a resource that carries another
          # repository's id: retagging is denied once the tag is set.
          Sid      = "DenyRetagOtherRepository"
          Effect   = "Deny"
          Action   = "*"
          Resource = "*"
          Condition = {
            "ForAnyValue:StringEquals" = { "aws:TagKeys" = local.keys }
            Null                       = { "aws:ResourceTag/gha-repo-id" = "false", "aws:PrincipalTag/gha-repo-id" = "false" }
            StringNotEquals            = { "aws:ResourceTag/gha-repo-id" = local.pt["gha-repo-id"] }
          }
        },
        {
          Sid       = "DenyRepoSessionTags"
          Effect    = "Deny"
          Action    = "sts:TagSession"
          Resource  = "*"
          Condition = { "ForAnyValue:StringEquals" = { "aws:TagKeys" = local.keys }, ArnNotLike = { "aws:PrincipalArn" = [local.tagger] } }
        },
        {
          Sid       = "DenyBoundaryEscape"
          Effect    = "Deny"
          Action    = ["iam:CreateRole", "iam:PutRolePermissionsBoundary"]
          Resource  = "*"
          Condition = { Null = { "aws:PrincipalTag/gha-repo-id" = "false" }, ArnNotLike = { "iam:PermissionsBoundary" = "${local.iam_arn}:policy/${local.prefix}-boundary" } }
        },
        {
          Sid       = "DenyRepoIamEscalation"
          Effect    = "Deny"
          Action    = ["iam:DeleteRolePermissionsBoundary", "iam:CreatePolicyVersion", "iam:SetDefaultPolicyVersion", "iam:CreateUser", "iam:CreateAccessKey", "iam:CreateLoginProfile"]
          Resource  = "*"
          Condition = { Null = { "aws:PrincipalTag/gha-repo-id" = "false" } }
        },
        {
          Sid       = "ProtectPlatform"
          Effect    = "Deny"
          Action    = ["iam:Attach*", "iam:Create*", "iam:Delete*", "iam:Detach*", "iam:Put*", "iam:SetDefaultPolicyVersion", "iam:Tag*", "iam:Untag*", "iam:Update*"]
          Resource  = local.platform_iam
          Condition = { ArnNotLike = { "aws:PrincipalArn" = var.exempt_principal_arns } }
        },
        {
          Sid       = "DenyMicrovmShell"
          Effect    = "Deny"
          Action    = ["lambda:CreateMicrovmShellAuthToken", "lambda:CreateMicrovmAuthToken"]
          Resource  = "*"
          Condition = { ArnNotLike = { "aws:PrincipalArn" = coalescelist(var.break_glass_principal_arns, ["arn:${var.partition}:iam::*:role/none"]) } }
        },
      ],
    )
  }

  # Second layer on data: a principal of one repository can't use resources
  # tagged for another, whatever its identity policy says. Untagged callers
  # (administrators, AWS services) are not affected.
  rcp = {
    Version = "2012-10-17"
    Statement = [{
      Sid       = "DenyCrossRepository"
      Effect    = "Deny"
      Principal = "*"
      Action    = ["secretsmanager:*", "sqs:*", "dynamodb:*", "logs:*", "cloudwatch:*"]
      Resource  = "*"
      Condition = {
        Null            = { "aws:ResourceTag/gha-repo-id" = "false", "aws:PrincipalTag/gha-repo-id" = "false" }
        StringNotEquals = { "aws:PrincipalTag/gha-repo-id" = local.rt_id }
      }
    }]
  }

  # Spelling and case of the keys; presence and values are enforced by the SCP.
  tag_policy = {
    tags = { for k in local.keys : k => { tag_key = { "@@assign" = k } } }
  }

  policies = {
    scp = { type = "SERVICE_CONTROL_POLICY", content = jsonencode(local.scp), description = "MicroVM runners: repository tags, boundaries, platform roles" }
    rcp = { type = "RESOURCE_CONTROL_POLICY", content = jsonencode(local.rcp), description = "MicroVM runners: no cross-repository data access" }
    tag = { type = "TAG_POLICY", content = jsonencode(local.tag_policy), description = "MicroVM runners: repository tag keys" }
  }
}

resource "aws_organizations_policy" "this" {
  for_each    = var.create ? local.policies : {}
  name        = "${local.prefix}-${each.key}"
  description = each.value.description
  type        = each.value.type
  content     = each.value.content
  tags        = var.tags

  lifecycle {
    precondition {
      condition     = each.key == "tag" || length(each.value.content) <= 5120
      error_message = "${each.key} exceeds 5,120 characters: shorten exempt_principal_arns."
    }
  }
}

resource "aws_organizations_policy_attachment" "this" {
  for_each = var.create ? {
    # Keyed by position, so targets created in the same apply (unknown ids) work.
    for pair in setproduct(keys(local.policies), range(length(var.target_ids))) : "${pair[0]}:${pair[1]}" => { policy = pair[0], target = var.target_ids[pair[1]] }
  } : {}
  policy_id = aws_organizations_policy.this[each.value.policy].id
  target_id = each.value.target
}
