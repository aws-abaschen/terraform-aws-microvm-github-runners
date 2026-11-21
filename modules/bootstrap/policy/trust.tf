# Trust policies of the phase 2 roles (tagger.tf), here so that they can be
# rendered and checked offline like the boundary (just iam-sim).

variable "oidc_provider_arn" {
  type    = string
  default = ""
}
variable "github_oidc_host" {
  type    = string
  default = "token.actions.githubusercontent.com"
}
variable "tagger_github_owners" {
  type    = list(string)
  default = []
}

locals {
  tagger_role = "${local.arn.iam}:role/${local.prefix}-tagger"
}

data "aws_iam_policy_document" "deploy_shared_trust" {
  statement {
    sid     = "TaggedByTagger"
    actions = ["sts:AssumeRole", "sts:TagSession"]
    principals {
      type        = "AWS"
      identifiers = [local.tagger_role]
    }
    # Both repository tags, and nothing else: without them the policy
    # variables of the boundary would not resolve.
    condition {
      test     = "Null"
      variable = "aws:RequestTag/gha-repo-id"
      values   = ["false"]
    }
    condition {
      test     = "Null"
      variable = "aws:RequestTag/gha-repo"
      values   = ["false"]
    }
    condition {
      test     = "ForAllValues:StringEquals"
      variable = "aws:TagKeys"
      values   = ["gha-repo-id", "gha-repo"]
    }
  }
  # SetSourceIdentity doesn't support the tag keys: its own statement.
  statement {
    sid     = "SourceIdentity"
    actions = ["sts:SetSourceIdentity"]
    principals {
      type        = "AWS"
      identifiers = [local.tagger_role]
    }
    condition {
      test     = "Null"
      variable = "sts:SourceIdentity"
      values   = ["false"]
    }
  }
}

data "aws_iam_policy_document" "deploy_entry_trust" {
  # checkov:skip=CKV_AWS_358:sub is limited to environment jobs of the allowed owners; the tagger checks the repository and environment against the registry.
  statement {
    actions = ["sts:AssumeRoleWithWebIdentity"]
    principals {
      type        = "Federated"
      identifiers = [var.oidc_provider_arn]
    }
    condition {
      test     = "StringEquals"
      variable = "${var.github_oidc_host}:aud"
      values   = ["sts.amazonaws.com"]
    }
    # Both default subject forms: repo:<owner>/... and the immutable
    # repo:<owner>@<owner_id>/... that new repositories get. An organisation
    # with another sub template must adapt this; the tagger never reads sub.
    condition {
      test     = "StringLike"
      variable = "${var.github_oidc_host}:sub"
      values   = flatten([for o in var.tagger_github_owners : ["repo:${o}/*:environment:*", "repo:${o}@*/*:environment:*"]])
    }
  }
}

output "deploy_shared_trust_json" {
  value = data.aws_iam_policy_document.deploy_shared_trust.json
}

output "deploy_entry_trust_json" {
  value = data.aws_iam_policy_document.deploy_entry_trust.json
}
