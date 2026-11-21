# Detection (DORA Art. 10): every alarm names its runbook section and goes to
# the platform incident topic.

locals {
  runbook = "https://aws-abaschen.github.io/terraform-aws-microvm-github-runners/operations.html"
  custom = {
    "dispatch-errors" = { metric = "DispatchErrors", threshold = 3, desc = "Runners fail to launch" }
    "sweep-errors"    = { metric = "SweepErrors", threshold = 3, desc = "Sweeper cannot reconcile" }
    "reaped"          = { metric = "Reaped", threshold = 1, desc = "A VM hit the hard lifetime: job ran past timeout-minutes" }
    "lost"            = { metric = "Lost", threshold = 3, desc = "VMs disappear before their job ends" }
  }
}

resource "aws_cloudwatch_metric_alarm" "dlq" {
  alarm_name          = "${local.base}-dlq"
  alarm_description   = "Jobs could not be dispatched after retries. Runbook: ${local.runbook}#dlq"
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateNumberOfMessagesVisible"
  dimensions          = { QueueName = aws_sqs_queue.dlq.name }
  statistic           = "Maximum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [local.platform.alarm_topic_arn]
  ok_actions          = [local.platform.alarm_topic_arn]
  tags                = local.tags
}

resource "aws_cloudwatch_metric_alarm" "queue_age" {
  alarm_name          = "${local.base}-queue-age"
  alarm_description   = "Jobs wait more than 10 minutes for a runner (capacity or quota). Runbook: ${local.runbook}#queue-age"
  namespace           = "AWS/SQS"
  metric_name         = "ApproximateAgeOfOldestMessage"
  dimensions          = { QueueName = aws_sqs_queue.jobs.name }
  statistic           = "Maximum"
  period              = 300
  evaluation_periods  = 2
  threshold           = 600
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [local.platform.alarm_topic_arn]
  ok_actions          = [local.platform.alarm_topic_arn]
  tags                = local.tags
}

resource "aws_cloudwatch_metric_alarm" "fn_errors" {
  for_each            = aws_lambda_function.fn
  alarm_name          = "${local.base}-${each.key}-errors"
  alarm_description   = "${each.key} Lambda errors. Runbook: ${local.runbook}#lambda-errors"
  namespace           = "AWS/Lambda"
  metric_name         = "Errors"
  dimensions          = { FunctionName = each.value.function_name }
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 3
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [local.platform.alarm_topic_arn]
  ok_actions          = [local.platform.alarm_topic_arn]
  tags                = local.tags
}

resource "aws_cloudwatch_metric_alarm" "custom" {
  for_each            = local.custom
  alarm_name          = "${local.base}-${each.key}"
  alarm_description   = "${each.value.desc}. Runbook: ${local.runbook}#${each.key}"
  namespace           = "MicrovmGithubRunners"
  metric_name         = each.value.metric
  dimensions          = { Repository = local.repo }
  statistic           = "Sum"
  period              = 900
  evaluation_periods  = 1
  threshold           = each.value.threshold
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"
  alarm_actions       = [local.platform.alarm_topic_arn]
  ok_actions          = [local.platform.alarm_topic_arn]
  tags                = local.tags
}

resource "aws_cloudwatch_metric_alarm" "sweeper_silent" {
  alarm_name          = "${local.base}-sweeper-silent"
  alarm_description   = "Sweeper has not run for 15 minutes: lost webhooks and stuck VMs are not repaired. Runbook: ${local.runbook}#sweeper-silent"
  namespace           = "AWS/Lambda"
  metric_name         = "Invocations"
  dimensions          = { FunctionName = aws_lambda_function.fn["sweeper"].function_name }
  statistic           = "Sum"
  period              = 900
  evaluation_periods  = 1
  threshold           = 1
  comparison_operator = "LessThanThreshold"
  treat_missing_data  = "breaching"
  alarm_actions       = [local.platform.alarm_topic_arn]
  ok_actions          = [local.platform.alarm_topic_arn]
  tags                = local.tags
}
