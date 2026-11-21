# Renders the account boundary, the phase 2 trust policies and the
# organisation guardrails offline, for `just iam-sim` (cmd/iamsim).
# No AWS calls: aws_iam_policy_document is evaluated locally.
terraform {
  required_version = ">= 1.11"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 6.73, < 7.0"
    }
  }
}

provider "aws" {
  region                      = "eu-central-1"
  access_key                  = "mock"
  secret_key                  = "mock"
  skip_credentials_validation = true
  skip_requesting_account_id  = true
  skip_metadata_api_check     = true
  skip_region_validation      = true
}

locals {
  account = "111111111111"
  region  = "eu-central-1"
}

module "policy" {
  source               = "../../modules/bootstrap/policy"
  name_prefix          = "gha-microvm"
  partition            = "aws"
  region               = local.region
  account              = local.account
  bucket_names         = ["gha-microvm-artifacts-${local.account}-${local.region}", "gha-microvm-tfstate-${local.account}-${local.region}"]
  kms_key_arn          = "arn:aws:kms:${local.region}:${local.account}:key/11111111-2222-3333-4444-555555555555"
  broker_function_arn  = "arn:aws:lambda:${local.region}:${local.account}:function:gha-microvm-broker"
  app_secret_arn       = "arn:aws:secretsmanager:${local.region}:${local.account}:secret:gha-microvm/platform/github-app-AbCdEf"
  oidc_provider_arn    = "arn:aws:iam::${local.account}:oidc-provider/token.actions.githubusercontent.com"
  tagger_github_owners = { acme = 1234567 }
}

module "guardrails" {
  source                = "../../modules/org-guardrails"
  exempt_principal_arns = ["arn:aws:iam::*:role/aws-reserved/sso.amazonaws.com/*/AWSReservedSSO_AdministratorAccess_*"]
}

output "policies" {
  value = {
    account      = local.account
    region       = local.region
    boundary     = module.policy.minified_json
    shared_trust = module.policy.deploy_shared_trust_json
    entry_trust  = module.policy.deploy_entry_trust_json
    scp          = module.guardrails.scp_json
    rcp          = module.guardrails.rcp_json
  }
}
