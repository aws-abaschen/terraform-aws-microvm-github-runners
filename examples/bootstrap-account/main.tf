# Applied once per workload account and Region by the platform team.
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
  region = "eu-west-1"
  default_tags {
    tags = {
      owner               = "platform-team"
      cost-center         = "cc-1234"
      data-classification = "internal"
    }
  }
}

module "runners_platform" {
  # Pin to a release commit SHA in production.
  source = "../../modules/bootstrap"

  vpc_id     = "vpc-0123456789abcdef0"
  subnet_ids = ["subnet-0aaaaaaaaaaaaaaaa", "subnet-0bbbbbbbbbbbbbbbb"]

  # No NAT here: HTTPS leaves through the Transit Gateway to the central
  # egress/inspection VPC; AWS APIs resolve to the central interface endpoints.
  vpc_endpoints      = "central"
  https_egress_cidrs = ["0.0.0.0/0"]

  lambda_zip_path = "${path.module}/lambda.zip" # verified release asset

  repositories = {
    # owner_id and repository_id pin the immutable OIDC subject:
    # gh api repos/acme/payments-api --jq '.owner.id,.id'
    "acme/payments-api" = { environment = "runners", owner_id = 1234567, repository_id = 987654321 }
    # Jobs read gha/acme/ledger/* secrets and parameters, plus anything
    # tagged gha-repo-id=<id of acme/ledger>.
    "acme/ledger" = { environment = "runners" }
  }

  # Phase 2: deploys of acme repositories go through the tagger and one shared
  # deploy role. Set per_repository_deploy_roles = false once all have moved.
  tagger_github_owners = { acme = 1234567 } # gh api users/acme --jq .id

  alarm_topic_arn    = null # or the topic of your incident management tool
  log_retention_days = 400
}

output "repositories" {
  value = module.runners_platform.repositories
}

output "github_app_secret_arn" {
  value = module.runners_platform.github_app_secret_arn
}

output "tagger" {
  value = module.runners_platform.tagger
}
