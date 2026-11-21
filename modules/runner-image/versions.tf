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
  }
}
