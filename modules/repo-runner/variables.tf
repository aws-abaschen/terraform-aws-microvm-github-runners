variable "repository" {
  description = "owner/repo served by these runners. Runners are registered at repository level."
  type        = string
  validation {
    condition     = can(regex("^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$", var.repository))
    error_message = "Use owner/repo."
  }
}

variable "platform_config_parameter" {
  description = "SSM parameter written by the bootstrap module."
  type        = string
  default     = "/gha-microvm/platform/config"
}

variable "lambda_zip_path" {
  description = "lambda.zip from a verified release or `just package`."
  type        = string
}

variable "image_arn" {
  description = "MicroVM image from the runner-image module."
  type        = string
}

variable "image_version" {
  description = "Pinned image version. Null runs the latest active version."
  type        = string
  default     = null
}

variable "architecture" {
  description = "Architecture of the image: ARM_64 or X86_64. Sets the default arch label."
  type        = string
  default     = "ARM_64"
}

variable "runner_labels" {
  description = "Labels of every runner. Default: self-hosted, linux, arm64|x64, microvm."
  type        = list(string)
  default     = null
}

variable "required_labels" {
  description = "A job must ask for all of these to get a MicroVM. Keeps plain `self-hosted` jobs away."
  type        = list(string)
  default     = ["microvm"]
}

variable "max_job_minutes" {
  description = "Longest job. The VM is hard-killed 5 minutes later. Set the same timeout-minutes in workflows."
  type        = number
  default     = 60
  validation {
    condition     = var.max_job_minutes >= 5 && var.max_job_minutes <= 475
    error_message = "Between 5 and 475 minutes (MicroVM limit is 8 h)."
  }
}

variable "max_runners" {
  description = "Concurrent MicroVMs for this repository. Size against the account MicroVM memory quota."
  type        = number
  default     = 5
}

variable "idle_timeout_minutes" {
  description = "Terminate a runner that registered but got no job within this time."
  type        = number
  default     = 10
}

variable "runner_group_id" {
  description = "Runner group. Repository-level runners use 1 (Default)."
  type        = number
  default     = 1
}

variable "additional_runner_policy_json" {
  description = "Extra IAM policy for jobs (ambient credentials of every job, including pull requests). Still capped by the repository boundary. Prefer GitHub OIDC roles for anything sensitive."
  type        = string
  default     = null
}

variable "reserve_concurrency" {
  description = "Reserve Lambda concurrency per function (webhook 20, dispatcher 5, sweeper 1). Off by default: 26 reserved per repository would fill the default account quota of 1,000 at about 34 repositories."
  type        = bool
  default     = false
}

variable "webhook_auth_type" {
  description = "NONE: GitHub calls the Function URL directly (HMAC verified). AWS_IAM: only a central ingress (for example API Gateway + WAF in the networking account) may call it."
  type        = string
  default     = "NONE"
  validation {
    condition     = contains(["NONE", "AWS_IAM"], var.webhook_auth_type)
    error_message = "NONE or AWS_IAM."
  }
}

variable "webhook_invoker_principals" {
  description = "IAM principals allowed to invoke the webhook when webhook_auth_type = AWS_IAM."
  type        = list(string)
  default     = []
}

variable "webhook_secret_version" {
  description = "Bump to rotate the webhook secret. Then update the GitHub webhook with the new value."
  type        = number
  default     = 1
}

variable "sweep_interval_minutes" {
  type    = number
  default = 2
}

variable "code_signing_config_arn" {
  description = "Lambda code signing config. Recommended: sign lambda.zip with AWS Signer in your release pipeline."
  type        = string
  default     = null
}

variable "deletion_protection" {
  description = "Protect the state table from deletion."
  type        = bool
  default     = true
}

variable "tags" {
  type    = map(string)
  default = {}
}

variable "secret_recovery_window_days" {
  description = "Recovery window of the webhook secret. 0 deletes immediately (sandboxes only)."
  type        = number
  default     = 7
}
