# Throw-away test stack for a single AWS account.
# It stands in for the landing zone with a small VPC and one NAT gateway
# (≈ $1–2 per day). Destroy it after testing: `just sandbox-down`.
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
}

variable "region" {
  type    = string
  default = "eu-central-1"
}

variable "repository" {
  description = "Private test repository (owner/repo)."
  type        = string
}

variable "create_github_oidc_provider" {
  description = "Set false if the account already has the GitHub OIDC provider."
  type        = bool
  default     = true
}

provider "aws" {
  region = var.region
  default_tags {
    tags = { purpose = "microvm-runner-sandbox" }
  }
}

provider "awscc" {
  region = var.region
}

data "aws_availability_zones" "this" {
  state = "available"
}

# ---------------------------------------------------------------- Landing-zone stand-in

resource "aws_vpc" "this" {
  cidr_block           = "10.42.0.0/24"
  enable_dns_support   = true
  enable_dns_hostnames = true
  tags                 = { Name = "microvm-sandbox" }
}

resource "aws_subnet" "private" {
  count             = 2
  vpc_id            = aws_vpc.this.id
  cidr_block        = cidrsubnet(aws_vpc.this.cidr_block, 2, count.index)
  availability_zone = data.aws_availability_zones.this.names[count.index]
  tags              = { Name = "microvm-sandbox-private-${count.index}" }
}

resource "aws_subnet" "public" {
  vpc_id            = aws_vpc.this.id
  cidr_block        = cidrsubnet(aws_vpc.this.cidr_block, 2, 3)
  availability_zone = data.aws_availability_zones.this.names[0]
  tags              = { Name = "microvm-sandbox-public" }
}

resource "aws_internet_gateway" "this" {
  vpc_id = aws_vpc.this.id
}

resource "aws_eip" "nat" {
  domain = "vpc"
}

# Plays the role of the central egress of the networking account.
resource "aws_nat_gateway" "this" {
  allocation_id = aws_eip.nat.id
  subnet_id     = aws_subnet.public.id
  depends_on    = [aws_internet_gateway.this]
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.this.id
  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.this.id
  }
}

resource "aws_route_table_association" "public" {
  subnet_id      = aws_subnet.public.id
  route_table_id = aws_route_table.public.id
}

resource "aws_route_table" "private" {
  vpc_id = aws_vpc.this.id
  route {
    cidr_block     = "0.0.0.0/0"
    nat_gateway_id = aws_nat_gateway.this.id
  }
}

resource "aws_route_table_association" "private" {
  count          = 2
  subnet_id      = aws_subnet.private[count.index].id
  route_table_id = aws_route_table.private.id
}

# ---------------------------------------------------------------- Platform (bootstrap)

module "bootstrap" {
  source = "../../modules/bootstrap"

  vpc_id                           = aws_vpc.this.id
  subnet_ids                       = aws_subnet.private[*].id
  vpc_endpoints                    = "central" # AWS APIs through the NAT, like central endpoints
  dynamodb_gateway_route_table_ids = [aws_route_table.private.id]
  lambda_zip_path                  = "${path.module}/../../dist/lambda.zip"
  create_github_oidc_provider      = var.create_github_oidc_provider
  repositories                     = { (var.repository) = {} }

  log_retention_days          = 7
  kms_deletion_window_days    = 7
  force_destroy               = true
  secret_recovery_window_days = 0
}

# ---------------------------------------------------------------- One repository

module "image" {
  source                    = "../../modules/runner-image"
  repository                = var.repository
  platform_config_parameter = module.bootstrap.platform_config_parameter
  artifact_path             = "${path.module}/../../dist/image-arm64.zip"
  architecture              = "ARM_64"
  memory_mib                = 4096
}

module "runners" {
  source                      = "../../modules/repo-runner"
  repository                  = var.repository
  platform_config_parameter   = module.bootstrap.platform_config_parameter
  lambda_zip_path             = "${path.module}/../../dist/lambda.zip"
  image_arn                   = module.image.image_arn
  image_version               = module.image.image_version
  max_job_minutes             = 5 # VM hard limit 10 minutes: makes the reaper testable
  max_runners                 = 2
  deletion_protection         = false
  secret_recovery_window_days = 0
}

# ---------------------------------------------------------------- Outputs used by the smoke test

output "region" {
  value = var.region
}

output "smoke" {
  value = {
    image_arn       = module.image.image_arn
    runner_role_arn = module.runners.runner_role_arn
    connector_arn   = module.bootstrap.network_connector_arn
    kms_key_arn     = module.bootstrap.kms_key_arn
    log_group       = "/aws/lambda-microvms/${replace(module.image.image_name, "-runner", "")}-runtime"
    param_prefix    = "/gha-microvm/${module.runners.repository_id}"
    webhook_url     = module.runners.webhook_url
    webhook_secret  = module.runners.webhook_secret_arn
    app_secret_arn  = module.bootstrap.github_app_secret_arn
    queue_arn       = module.runners.queue_arn
    deploy_role_arn = module.bootstrap.repositories[lower(var.repository)].deploy_role_arn
  }
}
