resource "aws_iam_openid_connect_provider" "github" {
  count          = var.create_github_oidc_provider ? 1 : 0
  url            = "https://${var.github_oidc_host}"
  client_id_list = ["sts.amazonaws.com"]
  tags           = local.tags
}

locals {
  oidc_provider_arn = "arn:${local.partition}:iam::${local.account}:oidc-provider/${var.github_oidc_host}"
  repo_tags         = { for repo, r in local.repos : repo => { "gha-repo" = repo, "gha-repo-id" = r.id } }
}

# One generic policy per account (modules/bootstrap/policy). It is BOTH the
# deploy role's identity policy and the permissions boundary of the deploy
# role and of every role a repository creates. The principal tags gha-repo-id
# and gha-repo select the repository, so adding repositories never changes it.
module "policy" {
  source                 = "./policy"
  name_prefix            = local.prefix
  partition              = local.partition
  region                 = local.region
  account                = local.account
  bucket_names           = [for b in aws_s3_bucket.this : b.id]
  kms_key_arn            = aws_kms_key.this.arn
  broker_function_arn    = aws_lambda_function.broker.arn
  app_secret_arn         = aws_secretsmanager_secret.app.arn
  additional_job_actions = var.additional_job_actions
}

resource "aws_iam_policy" "boundary" {
  name        = module.policy.name
  description = "Deploy policy and permissions boundary of every onboarded repository (ABAC on gha-repo-id)"
  policy      = module.policy.json
  tags        = local.tags

  lifecycle {
    precondition {
      condition     = length(module.policy.minified_json) <= 6144
      error_message = "The boundary policy exceeds the IAM limit of 6,144 characters: shorten additional_job_actions."
    }
  }
}

# Onboarding record read by the broker and by repo-runner. One small
# parameter per repository; nothing else in the account depends on the list.
resource "aws_ssm_parameter" "registry" {
  for_each = local.repos
  name     = "/${local.prefix}/registry/${each.value.id}"
  type     = "SecureString"
  key_id   = aws_kms_key.this.arn
  tier     = "Standard"
  value    = jsonencode({ repository = each.key, environment = each.value.environment })
  tags     = merge(local.tags, local.repo_tags[each.key])
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
  permissions_boundary = aws_iam_policy.boundary.arn
  max_session_duration = 3600
  # Principal tags: the generic policy scopes everything to them.
  tags = merge(local.tags, local.repo_tags[each.key])
}

resource "aws_iam_role_policy_attachment" "deploy" {
  for_each   = local.repos
  role       = aws_iam_role.deploy[each.key].name
  policy_arn = aws_iam_policy.boundary.arn
}
