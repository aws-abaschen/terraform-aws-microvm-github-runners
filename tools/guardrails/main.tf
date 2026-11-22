# Live test of modules/org-guardrails (just guardrails-up / guardrails-test).
# SCPs and RCPs never apply to the management account, so this creates one
# member account in its own OU and attaches the guardrails to that OU only.
# Apply from the management account. The account survives `guardrails-down`
# (which only detaches the policies); `guardrails-close` closes it, after
# which AWS keeps it suspended for about 90 days.
terraform {
  required_version = ">= 1.11"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 6.73, < 7.0"
    }
  }
}

variable "account_email" {
  description = "Root email of the test member account (must be unused by any AWS account)."
  type        = string
}

variable "region" {
  type    = string
  default = "eu-central-1"
}

variable "attach" {
  description = "Attach the guardrails to the test OU."
  type        = bool
  default     = true
}

provider "aws" {
  region = var.region
  default_tags {
    tags = { purpose = "microvm-guardrails-test" }
  }
}

data "aws_organizations_organization" "this" {}

resource "aws_organizations_organizational_unit" "test" {
  name      = "microvm-guardrails-test"
  parent_id = data.aws_organizations_organization.this.roots[0].id
}

resource "aws_organizations_account" "test" {
  name              = "microvm-guardrails-test"
  email             = var.account_email
  parent_id         = aws_organizations_organizational_unit.test.id
  role_name         = "OrganizationAccountAccessRole"
  close_on_deletion = true

  lifecycle {
    ignore_changes = [role_name]
  }
}

module "guardrails" {
  source = "../../modules/org-guardrails"
  create = var.attach
  # The management account reaches the member through this role; it must
  # stay able to set repository tags and manage platform roles.
  exempt_principal_arns = ["arn:aws:iam::*:role/OrganizationAccountAccessRole"]
  target_ids            = [aws_organizations_organizational_unit.test.id]
}

output "account_id" {
  value = aws_organizations_account.test.id
}

output "ou_id" {
  value = aws_organizations_organizational_unit.test.id
}
