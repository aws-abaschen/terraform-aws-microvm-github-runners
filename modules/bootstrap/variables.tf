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
    registry entry and a deploy role (assumable only from the given GitHub
    Environment) tagged gha-repo-id and gha-repo. The shared boundary confines
    it to its own resources, to gha/<owner>/<repo>/ secrets and parameters,
    and to data tagged with its gha-repo-id.

    owner_id and repository_id are GitHub's numeric ids (gh api repos/<owner>/<repo>
    --jq '.owner.id,.id'). When set, the deploy role trusts only the immutable
    OIDC subject repo:<owner>@<owner_id>/<repo>@<repository_id>:..., which is
    GitHub's default for new repositories, and the tagger requires the token's
    repository_id: a deleted and recreated repository with the same name gets
    nothing. Without them only repo:<owner>/<repo>:... is trusted (Access
    Analyzer rejects a wildcard id).
  EOT
  type = map(object({
    environment   = optional(string, "runners")
    owner_id      = optional(number)
    repository_id = optional(number)
  }))
  default = {}
  validation {
    condition     = alltrue([for k in keys(var.repositories) : can(regex("^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$", k))])
    error_message = "Keys must be owner/repo."
  }
  validation {
    condition     = alltrue([for r in values(var.repositories) : (r.owner_id == null) == (r.repository_id == null)])
    error_message = "Set both owner_id and repository_id, or neither."
  }
}

variable "tagger_github_owners" {
  description = <<-EOT
    Phase 2 (docs/iam.html): GitHub owners (organisations or users) whose
    onboarded repositories deploy through the tagger and the shared deploy role
    <prefix>-deploy, mapped to their numeric owner id
    (gh api users/<owner> --jq .id). The entry role trusts
    repo:<owner>/*:environment:* and the immutable repo:<owner>@<id>/*:..., and
    the tagger requires the token's repository_owner_id: a new account that
    takes over a deleted owner's name gets nothing. Empty disables the tagger.
  EOT
  type        = map(number)
  default     = {}
  validation {
    condition     = alltrue([for o in keys(var.tagger_github_owners) : can(regex("^[A-Za-z0-9-]+$", o))])
    error_message = "Owners are GitHub logins: letters, digits and dashes, no wildcards."
  }
}

variable "per_repository_deploy_roles" {
  description = "Create one deploy role per repository (phase 1). Set false once every repository deploys through the tagger, to free one role per repository."
  type        = bool
  default     = true
}

variable "tagger_deletion_protection" {
  description = "Deletion protection of the tagger replay table. Disable only in sandboxes."
  type        = bool
  default     = true
}

variable "additional_job_actions" {
  description = "Extra actions on \"*\" allowed to every repository of this account (boundary ceiling, not a grant). Keep it short: the boundary is limited to 6,144 characters."
  type        = list(string)
  default     = []
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
  description = "Organisation boundary to set on roles this module creates (broker, operator). Repository roles get the shared repository boundary instead."
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

variable "force_destroy" {
  description = "Allow destroying the artifact and state buckets with content. Sandboxes only."
  type        = bool
  default     = false
}

variable "secret_recovery_window_days" {
  description = "Recovery window of the GitHub App secret. 0 deletes immediately (sandboxes only)."
  type        = number
  default     = 30
}
