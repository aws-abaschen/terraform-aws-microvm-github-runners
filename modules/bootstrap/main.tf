data "aws_caller_identity" "this" {}
data "aws_region" "this" {}
data "aws_partition" "this" {}

locals {
  account   = data.aws_caller_identity.this.account_id
  region    = data.aws_region.this.region
  partition = data.aws_partition.this.partition
  prefix    = var.name_prefix
  tags      = merge(var.tags, { "managed-by" = "terraform", "component" = "microvm-github-runners" })

  # Must match modules/repo-runner and internal/broker.RepoID.
  repos = {
    for repo, cfg in var.repositories : lower(repo) => merge(cfg, {
      id   = substr(sha256(lower(repo)), 0, 10)
      base = "${local.prefix}-${substr(sha256(lower(repo)), 0, 10)}"
    })
  }
}

# ---------------------------------------------------------------- KMS

data "aws_iam_policy_document" "kms" {
  # checkov:skip=CKV_AWS_356:Key policies use "*" to mean "this key".
  # checkov:skip=CKV_AWS_111:Key policies use "*" to mean "this key".
  # checkov:skip=CKV_AWS_109:Root delegation is the standard key administration statement.
  statement {
    sid       = "AccountAdministration"
    actions   = ["kms:*"]
    resources = ["*"]
    principals {
      type        = "AWS"
      identifiers = ["arn:${local.partition}:iam::${local.account}:root"]
    }
  }
  statement {
    sid       = "CloudWatchLogs"
    actions   = ["kms:Encrypt*", "kms:Decrypt*", "kms:ReEncrypt*", "kms:GenerateDataKey*", "kms:Describe*"]
    resources = ["*"]
    principals {
      type        = "Service"
      identifiers = ["logs.${local.region}.amazonaws.com"]
    }
    condition {
      test     = "ArnLike"
      variable = "kms:EncryptionContext:aws:logs:arn"
      values   = ["arn:${local.partition}:logs:${local.region}:${local.account}:*"]
    }
  }
  # Lambda MicroVMs builds and launches images and rotates the run hook payload
  # key with its own service credentials (it uses no grants). Scoped to this
  # account's images, as documented in "Encrypting AWS Lambda MicroVM data".
  statement {
    sid       = "LambdaMicrovms"
    actions   = ["kms:GenerateDataKey", "kms:GenerateDataKeyWithoutPlaintext", "kms:Decrypt", "kms:ReEncrypt*"]
    resources = ["*"]
    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [local.account]
    }
    condition {
      test     = "ArnLike"
      variable = "aws:SourceArn"
      values   = ["arn:${local.partition}:lambda:${local.region}:${local.account}:microvm-image:*"]
    }
    condition {
      test     = "StringLike"
      variable = "kms:EncryptionContext:aws:lambda:MicrovmImageArn"
      values   = ["arn:${local.partition}:lambda:${local.region}:${local.account}:microvm-image:*"]
    }
  }
  statement {
    sid       = "CloudWatchAlarmsToEncryptedSNS"
    actions   = ["kms:Decrypt", "kms:GenerateDataKey*"]
    resources = ["*"]
    principals {
      type        = "Service"
      identifiers = ["cloudwatch.amazonaws.com"]
    }
    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [local.account]
    }
  }
}

resource "aws_kms_key" "this" {
  description             = "${local.prefix}: logs, queues, tables, parameters, secrets, artifacts, MicroVM state"
  enable_key_rotation     = true
  rotation_period_in_days = 365
  deletion_window_in_days = var.kms_deletion_window_days
  policy                  = data.aws_iam_policy_document.kms.json
  tags                    = local.tags
}

resource "aws_kms_alias" "this" {
  name          = "alias/${local.prefix}"
  target_key_id = aws_kms_key.this.key_id
}

# ---------------------------------------------------------------- S3

resource "aws_s3_bucket" "this" {
  # Versioning, SSE-KMS, public access block, lifecycle and logging are set on
  # both buckets through for_each below; checkov cannot follow for_each keys.
  # checkov:skip=CKV_AWS_21:Set by aws_s3_bucket_versioning.this
  # checkov:skip=CKV_AWS_145:Set by aws_s3_bucket_server_side_encryption_configuration.this
  # checkov:skip=CKV2_AWS_6:Set by aws_s3_bucket_public_access_block.this
  # checkov:skip=CKV2_AWS_61:Set by aws_s3_bucket_lifecycle_configuration.this
  # checkov:skip=CKV_AWS_18:Enabled when access_logs_bucket is set (organisation log archive).
  # checkov:skip=CKV_AWS_144:Artifacts are reproducible from releases; state is versioned and covered by AWS Backup. See docs/dora.html.
  # checkov:skip=CKV2_AWS_62:No consumer for bucket events.
  for_each      = toset(["artifacts", "tfstate"])
  bucket        = "${local.prefix}-${each.key}-${local.account}-${local.region}"
  force_destroy = var.force_destroy
  tags          = merge(local.tags, { purpose = each.key })
}

resource "aws_s3_bucket_ownership_controls" "this" {
  for_each = aws_s3_bucket.this
  bucket   = each.value.id
  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_public_access_block" "this" {
  for_each                = aws_s3_bucket.this
  bucket                  = each.value.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_versioning" "this" {
  for_each = aws_s3_bucket.this
  bucket   = each.value.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "this" {
  for_each = aws_s3_bucket.this
  bucket   = each.value.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = "aws:kms"
      kms_master_key_id = aws_kms_key.this.arn
    }
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "this" {
  for_each = aws_s3_bucket.this
  bucket   = each.value.id
  rule {
    id     = "expire-noncurrent"
    status = "Enabled"
    filter {}
    noncurrent_version_expiration {
      noncurrent_days = each.key == "tfstate" ? 400 : 90
    }
    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }
}

resource "aws_s3_bucket_logging" "this" {
  for_each      = var.access_logs_bucket == null ? {} : aws_s3_bucket.this
  bucket        = each.value.id
  target_bucket = var.access_logs_bucket
  target_prefix = "${each.value.id}/"
}

data "aws_iam_policy_document" "bucket" {
  for_each = aws_s3_bucket.this
  statement {
    sid       = "DenyInsecureTransport"
    effect    = "Deny"
    actions   = ["s3:*"]
    resources = [each.value.arn, "${each.value.arn}/*"]
    principals {
      type        = "*"
      identifiers = ["*"]
    }
    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }
  statement {
    sid       = "DenyUnencryptedUploads"
    effect    = "Deny"
    actions   = ["s3:PutObject"]
    resources = ["${each.value.arn}/*"]
    principals {
      type        = "*"
      identifiers = ["*"]
    }
    # Only a wrong header is denied. Without one the bucket default (SSE-KMS
    # with the platform key) applies. A negated operator alone (with or
    # without IfExists) also matches a missing header, hence the Null test.
    condition {
      test     = "Null"
      variable = "s3:x-amz-server-side-encryption"
      values   = ["false"]
    }
    condition {
      test     = "StringNotEquals"
      variable = "s3:x-amz-server-side-encryption"
      values   = ["aws:kms"]
    }
  }
}

resource "aws_s3_bucket_policy" "this" {
  for_each = aws_s3_bucket.this
  bucket   = each.value.id
  policy   = data.aws_iam_policy_document.bucket[each.key].json
}

# ---------------------------------------------------------------- Alarms

resource "aws_sns_topic" "alarms" {
  count             = var.alarm_topic_arn == null ? 1 : 0
  name              = "${local.prefix}-alarms"
  kms_master_key_id = aws_kms_key.this.arn
  tags              = local.tags
}

locals {
  alarm_topic_arn = coalesce(var.alarm_topic_arn, try(aws_sns_topic.alarms[0].arn, null))
}

# ---------------------------------------------------------------- Platform config

# Consumers read everything they need from one parameter, so a repository's
# Terraform only needs its own name and this path.
resource "aws_ssm_parameter" "platform" {
  name   = "/${local.prefix}/platform/config"
  type   = "SecureString"
  key_id = aws_kms_key.this.arn
  tier   = "Standard"
  tags   = local.tags
  value = jsonencode({
    name_prefix           = local.prefix
    kms_key_arn           = aws_kms_key.this.arn
    artifacts_bucket      = aws_s3_bucket.this["artifacts"].id
    tfstate_bucket        = aws_s3_bucket.this["tfstate"].id
    subnet_ids            = var.subnet_ids
    security_group_id     = aws_security_group.runners.id
    network_connector_arn = aws_lambdacore_network_connector.this.arn
    broker_function_arn   = aws_lambda_function.broker.arn
    alarm_topic_arn       = local.alarm_topic_arn
    github_api_url        = var.github_api_url
    log_retention_days    = var.log_retention_days
    boundary_arn          = aws_iam_policy.boundary.arn
    registry_path         = "/${local.prefix}/registry"
  })
}
