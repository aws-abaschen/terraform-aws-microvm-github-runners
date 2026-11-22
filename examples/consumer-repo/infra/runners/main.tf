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
  # Bucket, key and region come from -backend-config in the deploy workflow
  # (bootstrap outputs tfstate_bucket and state_key).
  backend "s3" {
    # No encrypt = true: it sends AES256, which the bucket denies. Objects get
    # the bucket default, SSE-KMS with the platform key.
    use_lockfile = true
  }
}

variable "region" {
  type    = string
  default = "eu-west-1"
}

variable "repository" {
  type    = string
  default = "acme/payments-api"
}

variable "deletion_protection" {
  description = "Protect the state table. Disable only for throw-away stacks."
  type        = bool
  default     = true
}

variable "secret_recovery_window_days" {
  type    = number
  default = 30
}

provider "aws" {
  region = var.region
}

provider "awscc" {
  region = var.region
}

locals {
  repository = var.repository
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

  deletion_protection         = var.deletion_protection
  secret_recovery_window_days = var.secret_recovery_window_days
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

output "image_arn" {
  value = module.image.image_arn
}

output "queue_arn" {
  value = module.runners.queue_arn
}
