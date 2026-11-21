variable "name_prefix" {
  description = "Name prefix used by bootstrap and repo-runner in the member accounts."
  type        = string
  default     = "gha-microvm"
}

variable "partition" {
  type    = string
  default = "aws"
}

variable "exempt_principal_arns" {
  description = "Administrators and platform pipelines that may manage repository tags and platform roles (ArnLike patterns, for example arn:aws:iam::*:role/aws-reserved/sso.amazonaws.com/*/AWSReservedSSO_AdministratorAccess_*). The principal that applies bootstrap must be in this list."
  type        = list(string)
  validation {
    condition     = length(var.exempt_principal_arns) > 0
    error_message = "List at least the role that applies bootstrap."
  }
}

variable "break_glass_principal_arns" {
  description = "Roles allowed to open MicroVM shell sessions (ArnLike patterns). Empty: nobody."
  type        = list(string)
  default     = []
}

variable "create" {
  description = "Create the policies in AWS Organizations. False only renders them (see outputs)."
  type        = bool
  default     = false
}

variable "target_ids" {
  description = "OUs or accounts to attach the three policies to. Test on one account first."
  type        = list(string)
  default     = []
}

variable "tags" {
  type    = map(string)
  default = {}
}
