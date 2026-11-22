# The one operations dashboard of an account, <prefix>-platform:
#   1. alarm status: platform alarms, then every onboarded repository's alarms
#   2. all repositories: one line per repository (CloudWatch SEARCH over the
#      Repository dimension, so new repositories appear without a change)
#   3. one repository, chosen with the Repository selector at the top
#   4. platform functions: broker and tagger
# Runbooks: docs/operations.html.

locals {
  runbook = "https://aws-abaschen.github.io/terraform-aws-microvm-github-runners/operations.html"
  ns      = "MicrovmGithubRunners"
  search  = "{${local.ns},Repository}"

  platform_fns = merge(
    { broker = aws_lambda_function.broker.function_name },
    local.tagger ? { tagger = aws_lambda_function.tagger[0].function_name } : {},
  )
  # Alarm names of a repository. Must match modules/repo-runner/alarms.tf.
  repo_alarm_suffixes = ["dlq", "queue-age", "sweeper-silent", "webhook-errors", "dispatcher-errors", "sweeper-errors", "dispatch-errors", "sweep-errors", "reaped", "lost"]
  alarm_arn           = "arn:${local.partition}:cloudwatch:${local.region}:${local.account}:alarm"
  alarm_list = concat(
    ["${local.alarm_arn}:${local.prefix}-broker-errors"],
    local.tagger ? ["${local.alarm_arn}:${local.prefix}-tagger-errors"] : [],
    flatten([for repo, r in local.repos : [for s in local.repo_alarm_suffixes : "${local.alarm_arn}:${r.base}-${s}"]]),
  )
  # An alarm widget holds at most 100 alarms.
  alarm_chunks = chunklist(local.alarm_list, 100)
  alarm_rows   = length(local.alarm_chunks) * 3

  first_repo = length(local.repos) > 0 ? sort(keys(local.repos))[0] : "none"
  # A custom metric of the repository chosen in the selector.
  sel = { for m in ["ActiveRunners", "Dispatched_queued", "Dispatched_completed", "Throttled", "Redispatched", "IdleTerminated", "EndedBySweeper", "Lost", "Reaped", "DispatchErrors", "SweepErrors"] :
  m => [local.ns, m, "Repository", local.first_repo] }

  graph = {
    for k, v in {
      runners  = { title = "Active runners", metric = "ActiveRunners", stat = "Maximum" }
      jobs     = { title = "Jobs dispatched", metric = "Dispatched_queued", stat = "Sum" }
      throttle = { title = "Jobs waiting for capacity (Throttled)", metric = "Throttled", stat = "Sum" }
    } : k => merge(v, { expression = "SEARCH('${local.search} MetricName=\"${v.metric}\"', '${v.stat}', 300)" })
  }
  y1 = 2 + local.alarm_rows # all repositories
  y2 = local.y1 + 13        # selected repository
  y3 = local.y2 + 14        # platform functions

  dashboard_widgets = concat(
    [{
      type       = "text", x = 0, y = 0, width = 24, height = 2
      properties = { markdown = "## MicroVM runners (${local.region})\nAlarms page the platform topic. Use the **Repository** selector for one repository's detail. Runbooks: [operations](${local.runbook})." }
    }],
    [for i, c in local.alarm_chunks : {
      type       = "alarm", x = 0, y = 2 + i * 3, width = 24, height = 3
      properties = { title = i == 0 ? "Alarms: platform and repositories" : "Alarms (continued)", alarms = c, sortBy = "stateUpdatedTimestamp" }
    }],
    [{
      type       = "text", x = 0, y = local.y1, width = 24, height = 1
      properties = { markdown = "### All repositories, one line each" }
    }],
    [for i, k in ["runners", "jobs", "throttle"] : {
      type = "metric", x = i * 8, y = local.y1 + 1, width = 8, height = 6
      properties = {
        title   = local.graph[k].title, region = local.region, view = "timeSeries", period = 300
        metrics = [[{ expression = local.graph[k].expression, id = "e1", label = "" }]]
      }
    }],
    [
      {
        type = "metric", x = 0, y = local.y1 + 7, width = 8, height = 6
        properties = {
          title   = "Problems (Lost, Reaped, DispatchErrors, SweepErrors)", region = local.region, view = "timeSeries", period = 300
          metrics = [[{ expression = "SEARCH('${local.search} MetricName=(\"Lost\" OR \"Reaped\" OR \"DispatchErrors\" OR \"SweepErrors\")', 'Sum', 300)", id = "e1", label = "" }]]
        }
      },
      {
        type = "metric", x = 8, y = local.y1 + 7, width = 8, height = 6
        properties = {
          title       = "Oldest queued job (s)", region = local.region, view = "timeSeries", period = 60
          metrics     = [[{ expression = "SEARCH('{AWS/SQS,QueueName} MetricName=\"ApproximateAgeOfOldestMessage\" ${local.prefix} jobs', 'Maximum', 60)", id = "e1", label = "" }]]
          annotations = { horizontal = [{ label = "queue-age alarm", value = 600 }] }
        }
      },
      {
        type = "metric", x = 16, y = local.y1 + 7, width = 8, height = 6
        properties = {
          title   = "Function errors (webhook, dispatcher, sweeper)", region = local.region, view = "timeSeries", period = 300
          metrics = [[{ expression = "SEARCH('{AWS/Lambda,FunctionName} MetricName=\"Errors\" ${local.prefix}', 'Sum', 300)", id = "e1", label = "" }]]
        }
      },
      {
        type       = "text", x = 0, y = local.y2, width = 24, height = 1
        properties = { markdown = "### Selected repository" }
      },
      {
        type = "metric", x = 0, y = local.y2 + 1, width = 12, height = 6
        properties = {
          title = "Runners and jobs", region = local.region, view = "timeSeries", period = 60
          metrics = [
            concat(local.sel["ActiveRunners"], [{ stat = "Maximum", label = "active runners" }]),
            concat(local.sel["Dispatched_queued"], [{ stat = "Sum", label = "jobs dispatched" }]),
            concat(local.sel["Dispatched_completed"], [{ stat = "Sum", label = "jobs completed" }]),
          ]
        }
      },
      {
        type = "metric", x = 12, y = local.y2 + 1, width = 12, height = 6
        properties = {
          title   = "Health (alarms at Lost >= 3, Reaped >= 1, DispatchErrors >= 3, SweepErrors >= 3 per 5 min)", region = local.region, view = "timeSeries", period = 300, stat = "Sum"
          metrics = [for k in ["Throttled", "Redispatched", "IdleTerminated", "EndedBySweeper", "Lost", "Reaped", "DispatchErrors", "SweepErrors"] : concat(local.sel[k], [{ label = k }])]
        }
      },
      {
        type       = "text", x = 0, y = local.y3, width = 24, height = 1
        properties = { markdown = "### Platform functions" }
      },
      {
        type = "metric", x = 0, y = local.y3 + 1, width = 12, height = 6
        properties = {
          title = "Broker and tagger: invocations and errors (tagger errors include refused tokens)", region = local.region, view = "timeSeries", period = 300, stat = "Sum"
          metrics = concat(
            [for k, f in local.platform_fns : ["AWS/Lambda", "Invocations", "FunctionName", f, { label = "${k} invocations" }]],
            [for k, f in local.platform_fns : ["AWS/Lambda", "Errors", "FunctionName", f, { label = "${k} errors" }]],
          )
        }
      },
      {
        type = "metric", x = 12, y = local.y3 + 1, width = 12, height = 6
        properties = {
          title = "Account Lambda concurrency and platform latency", region = local.region, view = "timeSeries", period = 60
          metrics = concat(
            [["AWS/Lambda", "ConcurrentExecutions", { stat = "Maximum", label = "account concurrency" }]],
            [for k, f in local.platform_fns : ["AWS/Lambda", "Duration", "FunctionName", f, { stat = "p95", label = "${k} p95 (ms)", yAxis = "right" }]],
          )
        }
      },
    ],
  )

  # The Repository selector: changes the Repository dimension of the
  # "Selected repository" graphs; its values come from the metrics.
  dashboard_variables = [{
    type         = "property"
    property     = "Repository"
    inputType    = "select"
    id           = "repository"
    label        = "Repository"
    defaultValue = local.first_repo
    visible      = true
    search       = "${local.search} MetricName=\"ActiveRunners\""
    populateFrom = "Repository"
  }]
}

resource "aws_cloudwatch_dashboard" "platform" {
  dashboard_name = "${local.prefix}-platform"
  dashboard_body = jsonencode({ widgets = local.dashboard_widgets, variables = local.dashboard_variables })
}
