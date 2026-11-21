# Lives in the consumer repository and is applied from its own workflow.
terraform {
  required_version = ">= 1.11"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 6.73, < 7.0"
    }
    awscc = {
      source  = "hashicorp/awscc"
      version = ">= 1.110, < 2.0"
    }
    random = {
      source  = "hashicorp/random"
      version = ">= 3.7, < 4.0"
    }
  }
  # Bucket and key come from the bootstrap outputs (tfstate_bucket, state_key).
  backend "s3" {
    region       = "eu-west-1"
    encrypt      = true
    use_lockfile = true
  }
}

provider "aws" {
  region = "eu-west-1"
}

provider "awscc" {
  region = "eu-west-1"
}

locals {
  repository = "acme/payments-api"
  # Release assets downloaded and attestation-verified by the deploy workflow.
  release = "${path.module}/.release"
}

module "image" {
  # Production: pin ?ref= to the release commit SHA.
  source        = "../../../../modules/runner-image"
  repository    = local.repository
  artifact_path = "${local.release}/image-arm64.zip"
  architecture  = "ARM_64"
  memory_mib    = 4096 # 2 vCPU baseline, bursts to 8 vCPU
}

module "runners" {
  source          = "../../../../modules/repo-runner"
  repository      = local.repository
  lambda_zip_path = "${local.release}/lambda.zip"
  image_arn       = module.image.image_arn
  image_version   = module.image.image_version
  architecture    = module.image.architecture

  max_job_minutes = 60
  max_runners     = 5
}

output "webhook_url" {
  value = module.runners.webhook_url
}

output "webhook_secret_arn" {
  value = module.runners.webhook_secret_arn
}

output "runner_labels" {
  value = module.runners.runner_labels
}
