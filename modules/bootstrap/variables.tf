variable "name_prefix" {
  description = "Prefix of every resource name. Must match the repo-runner module."
  type        = string
  default     = "gha-microvm"
  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{2,20}$", var.name_prefix))
    error_message = "3-21 chars, lower-case letters, digits and dashes."
  }
}

variable "vpc_id" {
  description = "VPC of the workload account. Egress goes through the central networking account (Transit Gateway); no NAT is required here."
  type        = string
}

variable "subnet_ids" {
  description = "Private subnets for Lambda functions, the MicroVM egress connector and local VPC endpoints. Use at least two AZs."
  type        = list(string)
  validation {
    condition     = length(var.subnet_ids) >= 2
    error_message = "Use at least two subnets in different AZs."
  }
}

variable "https_egress_cidrs" {
  description = "Destinations allowed on TCP 443 from Lambdas and MicroVMs: central VPC endpoints and the central egress path to GitHub. Narrow this when the central firewall publishes fixed ranges."
  type        = list(string)
  default     = ["0.0.0.0/0"]
}

variable "https_egress_prefix_list_ids" {
  description = "Managed prefix lists allowed on TCP 443 (for example the central endpoints or GitHub ranges)."
  type        = list(string)
  default     = []
}

variable "vpc_endpoints" {
  description = "\"central\": interface endpoints live in the networking account and resolve through shared Route 53 private zones. \"local\": this module creates them in the VPC."
  type        = string
  default     = "central"
  validation {
    condition     = contains(["central", "local"], var.vpc_endpoints)
    error_message = "Use central or local."
  }
}

variable "local_endpoint_services" {
  description = "Interface endpoints created when vpc_endpoints = \"local\"."
  type        = list(string)
  default     = ["sqs", "secretsmanager", "ssm", "kms", "logs", "sts", "lambda", "monitoring", "xray"]
}

variable "dynamodb_gateway_route_table_ids" {
  description = "Route tables that get a DynamoDB gateway endpoint. Gateway endpoints cannot be centralised; leave empty to reach DynamoDB through the central path."
  type        = list(string)
  default     = []
}

variable "github_api_url" {
  description = "GitHub REST API URL. Change for GitHub Enterprise Server or data residency (GHE.com)."
  type        = string
  default     = "https://api.github.com"
}

variable "github_oidc_host" {
  description = "Issuer host of GitHub Actions OIDC tokens. Change for GHES or GHE.com."
  type        = string
  default     = "token.actions.githubusercontent.com"
}

variable "create_github_oidc_provider" {
  description = "Create the IAM OIDC provider for GitHub Actions. Set false if the account already has one."
  type        = bool
  default     = true
}

variable "repositories" {
  description = <<-EOT
    Repositories onboarded in this account, keyed by owner/repo. Each gets a
    deploy role (assumable only from the given GitHub Environment) and a
    permissions boundary that confines it to its own resources and secrets.
  EOT
  type = map(object({
    environment            = optional(string, "runners")
    job_secret_prefixes    = optional(list(string))
    job_parameter_paths    = optional(list(string))
    additional_job_actions = optional(list(string), [])
  }))
  default = {}
  validation {
    condition     = alltrue([for k in keys(var.repositories) : can(regex("^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$", k))])
    error_message = "Keys must be owner/repo."
  }
}

variable "lambda_zip_path" {
  description = "Path to lambda.zip from a verified release (or `just package`). Used for the token broker."
  type        = string
}

variable "alarm_topic_arn" {
  description = "Existing SNS topic of your incident management process. When null a KMS-encrypted topic is created."
  type        = string
  default     = null
}

variable "log_retention_days" {
  description = "Retention of all log groups. DORA asks for retention matching your ICT logging policy; 400 days covers a full year plus audit lag."
  type        = number
  default     = 400
}

variable "kms_deletion_window_days" {
  type    = number
  default = 30
}

variable "access_logs_bucket" {
  description = "Bucket receiving S3 server access logs of the artifact and state buckets. Null disables access logging."
  type        = string
  default     = null
}

variable "permissions_boundary_arn" {
  description = "Organisation boundary to set on roles this module creates (broker, operator). Per-repo roles get the per-repo boundaries instead."
  type        = string
  default     = null
}

variable "tags" {
  description = "Tags on every resource, for example owner, cost-center, data-classification."
  type        = map(string)
  default     = {}
}

variable "code_signing_config_arn" {
  description = "Lambda code signing config for the broker. Recommended: sign lambda.zip with AWS Signer."
  type        = string
  default     = null
}
