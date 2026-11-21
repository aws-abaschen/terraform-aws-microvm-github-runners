variable "repository" {
  description = "owner/repo this image belongs to."
  type        = string
}

variable "platform_config_parameter" {
  description = "SSM parameter written by the bootstrap module."
  type        = string
  default     = "/gha-microvm/platform/config"
}

variable "artifact_path" {
  description = "Image artifact zip from a verified release or `just image-artifact` (Dockerfile, supervisor, actions-runner.tar.gz, VERSION)."
  type        = string
}

variable "architecture" {
  description = "ARM_64 (Graviton, cheaper) or X86_64 (base image version 3 or later). Must match the artifact."
  type        = string
  default     = "ARM_64"
  validation {
    condition     = contains(["ARM_64", "X86_64"], var.architecture)
    error_message = "ARM_64 or X86_64."
  }
}

variable "memory_mib" {
  description = "Baseline memory. vCPU = memory / 2 GiB; bursts to 4x. 4096 = 2 vCPU baseline."
  type        = number
  default     = 4096
  validation {
    condition     = contains([512, 1024, 2048, 4096, 8192], var.memory_mib)
    error_message = "One of 512, 1024, 2048, 4096, 8192."
  }
}

variable "base_image_version" {
  description = "Version of the Lambda-managed al2023-1 base image. Pin it; review AWS deprecation notices (60 + 30 days)."
  type        = string
  default     = "3"
}

variable "enable_docker" {
  description = "Grant full OS capabilities so dockerd can run. Must match how the artifact was built."
  type        = bool
  default     = true
}

variable "tags" {
  type    = map(string)
  default = {}
}
