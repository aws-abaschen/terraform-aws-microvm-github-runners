data "aws_caller_identity" "this" {}
data "aws_region" "this" {}
data "aws_partition" "this" {}

data "aws_ssm_parameter" "platform" {
  name = var.platform_config_parameter
}

locals {
  platform  = jsondecode(data.aws_ssm_parameter.platform.value)
  repo      = lower(var.repository)
  id        = substr(sha256(local.repo), 0, 10)
  base      = "${local.platform.name_prefix}-${local.id}"
  account   = data.aws_caller_identity.this.account_id
  region    = data.aws_region.this.region
  partition = data.aws_partition.this.partition
  boundary  = local.platform.boundaries[local.repo]
  tags      = merge(var.tags, { "gha-repo" = local.repo, "gha-repo-id" = local.id, "managed-by" = "terraform" })
  key       = "${local.id}/image/${filesha256(var.artifact_path)}.zip"
}

resource "aws_s3_object" "artifact" {
  bucket                 = local.platform.artifacts_bucket
  key                    = local.key
  source                 = var.artifact_path
  source_hash            = filemd5(var.artifact_path)
  server_side_encryption = "aws:kms"
  kms_key_id             = local.platform.kms_key_arn
  tags                   = local.tags
}

data "aws_iam_policy_document" "trust" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [local.account]
    }
  }
}

resource "aws_iam_role" "build" {
  name                 = "${local.base}-image-build"
  description          = "Builds the MicroVM runner image of ${local.repo}"
  assume_role_policy   = data.aws_iam_policy_document.trust.json
  permissions_boundary = local.boundary
  tags                 = local.tags
}

resource "aws_cloudwatch_log_group" "build" {
  name              = "/aws/lambda-microvms/${local.base}-build"
  retention_in_days = local.platform.log_retention_days
  kms_key_id        = local.platform.kms_key_arn
  tags              = local.tags
}

data "aws_iam_policy_document" "build" {
  statement {
    actions   = ["s3:GetObject", "s3:GetObjectVersion"]
    resources = ["arn:${local.partition}:s3:::${local.platform.artifacts_bucket}/${local.id}/image/*"]
  }
  statement {
    actions   = ["kms:Decrypt", "kms:GenerateDataKey"]
    resources = [local.platform.kms_key_arn]
  }
  statement {
    actions   = ["logs:CreateLogStream", "logs:PutLogEvents"]
    resources = ["${aws_cloudwatch_log_group.build.arn}:*"]
  }
}

resource "aws_iam_role_policy" "build" {
  role   = aws_iam_role.build.id
  policy = data.aws_iam_policy_document.build.json
}

resource "awscc_lambda_microvm_image" "this" {
  name                       = "${local.base}-runner"
  description                = "One-job GitHub Actions runner for ${local.repo}"
  base_image_arn             = "arn:${local.partition}:lambda:${local.region}:aws:microvm-image:al2023-1"
  base_image_version         = var.base_image_version
  build_role_arn             = aws_iam_role.build.arn
  code_artifact              = { uri = "s3://${aws_s3_object.artifact.bucket}/${aws_s3_object.artifact.key}" }
  cpu_configurations         = [{ architecture = var.architecture }]
  additional_os_capabilities = var.enable_docker ? ["ALL"] : []
  # Build-time egress follows the landing-zone path too.
  egress_network_connectors = [local.platform.network_connector_arn]
  kms_key_identifier        = local.platform.kms_key_arn
  environment_variables     = [{ key = "GHA_REPOSITORY", value = local.repo }]
  resources                 = [{ minimum_memory_in_mi_b = var.memory_mib }]
  logging = {
    cloudwatch = { log_group = aws_cloudwatch_log_group.build.name }
  }
  hooks = {
    port = 9000
    microvm_image_hooks = {
      ready                       = "ENABLED"
      ready_timeout_in_seconds    = 600
      validate                    = "ENABLED"
      validate_timeout_in_seconds = 120
    }
    microvm_hooks = {
      run                          = "ENABLED"
      run_timeout_in_seconds       = 60
      terminate                    = "ENABLED"
      terminate_timeout_in_seconds = 30
      resume                       = "DISABLED"
      suspend                      = "DISABLED"
    }
  }
  tags       = [for k, v in local.tags : { key = k, value = v }]
  depends_on = [aws_iam_role_policy.build]
}
