# AGENTS.md

Per-repository ephemeral GitHub Actions runners on AWS Lambda MicroVMs, deployed with Terraform. Public repository in the `aws-abaschen` GitHub organization.
Docs site (GitHub Pages from `main:/docs`): https://aws-abaschen.github.io/terraform-aws-microvm-github-runners/

## Conventions

- Conventional Commits. Land each finished task as one squashed commit on `main` (no pull requests), then push.
- Design for scale (many repositories per account). Use one generic ABAC policy with principal tags (`gha-repo-id`, `gha-repo`) inside resource ARNs. Do not write per-repository policies. Enforce the tags with the SCP, the tag policy and `aws:TagKeys` conditions. Only the tagger sets session tags.
- Keep answers short. Don't summarize after finishing.
- New dependencies: use the latest version. If that isn't possible, add a comment explaining why.
- GitHub Actions: pin every action by full commit SHA with a `# vX.Y.Z` comment, at the latest release. This applies to docs and examples too.
- Maintain the `justfile` (bash) and `win.just` (PowerShell 7, `just win::<recipe>`). A developer must be able to build, deploy and test with them.
- Enterprise and DORA context:
  - no NAT in workload accounts, central egress and VPC endpoints
  - Lambdas in the VPC
  - KMS everywhere
  - audit logs and runbooks
- Line endings are LF (`.gitattributes`). On Windows, write files in binary mode from scripts.
- Keep `docs/` current with the code. `docs/testing.html` holds the latest test results; `docs/iam.html` holds the IAM design and its status.

## Layout

| Path | What |
| --- | --- |
| `cmd/lambda` | One Go binary. `FUNCTION_ROLE` = webhook, dispatcher, sweeper, broker or tagger |
| `cmd/supervisor` | Runs inside the MicroVM: hooks on :9000, reads and deletes the JIT SSM parameter, starts dockerd, runs `run.sh --jitconfig` |
| `cmd/smoke` | Sandbox smoke test: one MicroVM like the dispatcher starts it, plus a forged token to the tagger |
| `cmd/iamsim`, `tools/iamsim` | `just iam-sim`: renders the boundary, trust policies and guardrails offline, then checks them with `iam:SimulateCustomPolicy` and Access Analyzer. Add a case with every boundary or SCP change |
| `cmd/realjob` | `just sandbox-realjob <repo> <command>`: the test repository deploys and destroys its own runners through the tagger, then runs real jobs and the drills |
| `cmd/guardtest`, `tools/guardrails` | `just guardrails-up|test|down|close`: live test of `org-guardrails` in a member account in its own OU |
| `cmd/zipdet` | Reproducible zips with Unix mode bits, used by every build (also on Windows) |
| `internal/` | `broker` (presigned STS caller check), `tagger` (GitHub OIDC to a tagged deploy session), `runner` (dispatch and sweep), `github`, `webhook`, `awsx` (AWS adapters), `obs` |
| `modules/bootstrap` | Per account and Region. `bootstrap/policy` renders the generic boundary and the phase 2 trust policies; `tagger.tf` holds the tagger, the entry role and the shared deploy role |
| `modules/runner-image`, `modules/repo-runner` | Per repository. Alarm names in `repo-runner/alarms.tf` must match the list in `bootstrap/dashboard.tf` (the one platform dashboard) |
| `modules/org-guardrails` | SCP, RCP and tag policy, applied from the management account |
| `examples/` | `bootstrap-account`, `consumer-repo` (infra and three workflows), `sandbox` (with `e2e/`) |

## Checks

```sh
just check                      # gofmt, vet, go test -race, terraform validate
golangci-lint run ./...
tflint --recursive --config "$PWD/.tflint.hcl"
checkov -d modules --framework terraform
just iam-sim                    # AWS credentials, read-only APIs
```

## Sandbox (account 067751999967, eu-central-1)

- Use the SSO `admin` profile. The account already has the GitHub OIDC provider, so pass `false` for `oidc`.
- The test repository is the private `aws-abaschen/microvm-runner-sandbox`. It is new, so it signs in with the immutable OIDC subject, and its ids are required.
- On Windows, prefix every recipe with `win::`.

```powershell
$env:TF_VAR_github_ids = '{owner_id=143795778,repository_id=1466740461}'
just win::sandbox-up aws-abaschen/microvm-runner-sandbox eu-central-1 false   # about 15 minutes
just win::sandbox-smoke eu-central-1
just win::sandbox-e2e aws-abaschen/microvm-runner-sandbox
just win::sandbox-down aws-abaschen/microvm-runner-sandbox eu-central-1 false # always
```

The real-job cycle runs the sandbox in `repository` mode, where the test repository deploys its own runners:

```powershell
$env:TF_VAR_runners_deployed_by = 'repository'
$env:TF_VAR_github_app_source_secret = 'microvm-runner-sandbox/github-app'   # stored once by a person with just win::sandbox-app-key
$env:TF_VAR_per_repository_deploy_roles = 'false'                            # optional: tagger path only
just win::sandbox-up aws-abaschen/microvm-runner-sandbox eu-central-1 false
foreach ($c in 'prepare','deploy','webhook','job','drill-capacity','drill-vm-failure','drill-lost-webhooks') { just win::sandbox-realjob aws-abaschen/microvm-runner-sandbox $c }
just win::sandbox-realjob aws-abaschen/microvm-runner-sandbox destroy         # BEFORE sandbox-down (destroy-local as the fallback)
just win::sandbox-realjob aws-abaschen/microvm-runner-sandbox unhook
just win::sandbox-down aws-abaschen/microvm-runner-sandbox eu-central-1 false
```

Persistent test fixtures, outside the sandbox lifecycle:
- the private test repository and its GitHub App `aws-abaschen-microvm-runners-sandbox` (App id 5868401), installed on that repository alone
- the secret `microvm-runner-sandbox/github-app` (eu-central-1, tag `purpose=microvm-runner-sandbox-app`)
- the member account `155419942832`, created by `tools/guardrails` (local state) in OU `microvm-guardrails-test`, with the guardrails attached

- Always finish with `sandbox-down`: the NAT gateway costs about $1–2 a day.
- Then check that nothing tagged `purpose=microvm-runner-sandbox` is left. The tagging API keeps stale ARNs for a while, so look each one up.
- After deploying or changing anything, update the central docs repository (`D:\_projects\baschenis-cloud-ci\docs`, page `microvm-runners.md`). That repository may hold other people's uncommitted work: stage only your own lines.

## Gotchas

- **SSO and the Go SDK.** The Go SDK fails to refresh an older SSO token even when the AWS CLI works. Run the Go tools with `aws configure export-credentials --format powershell | Invoke-Expression; $env:AWS_PROFILE = $null`.
- **Immutable OIDC subject.** New GitHub repositories sign in as `repo:<owner>@<owner_id>/<repo>@<repository_id>:...`. Access Analyzer rejects a wildcard in place of an id, so pin the ids (`owner_id`, `repository_id`, and `tagger_github_owners` as owner => id).
- **Negated condition operators.** `StringNotEquals`, `ArnNotLike` and the like, with or without `IfExists`, also match a missing key. To deny only a wrong value, add a `Null ... = false` test.
- **The boundary is nearly full.** It is at 6,042 of 6,144 characters, and Terraform refuses a plan above the limit. Replace or merge entries rather than add them. Never put a `#` comment in the middle of an HCL list line: it silently drops the rest of the line. `just iam-sim` must have a case for every action.
- **Teardown order.** In `repository` mode, destroy the repository's stack before `sandbox-down`: the stack reads the platform configuration even to destroy itself.
- **PowerShell pipelines.** `Select-Object -First` ends a pipeline early and leaves `terraform` running detached, holding the state lock. Write the output to a log file instead.
- **New AWS accounts** activate their services over hours (`OptInRequired`, `SubscriptionRequiredException`), and SCP changes take minutes to apply. `guardtest` reports these as SKIP.
- **Terraform `flatten()`** flattens every level. Build lists of lists, such as dashboard `metrics`, with `concat()`. CloudWatch validates a dashboard body only on `PutDashboard`.
- **A failed `-target=module.bootstrap` step** of `sandbox-up` skips the full apply, so the NAT route is never created and the tagger answers `verifier unavailable`. Fix the failure and run `sandbox-up` again.
- **The IAM simulator** doesn't map `lambda:PassNetworkConnector` to connector ARNs. Real deploys are the proof for that action.

## Status (2026-11-22)

**Verified against real AWS and GitHub:**
- smoke test 6/6
- end-to-end 37/37 through both deploy paths, and 20/20 through the tagger alone (`per_repository_deploy_roles = false`)
- `just iam-sim` 92/92, with Access Analyzer finding no errors
- the repository deploying and destroying its own runners through the tagger, under the boundary
- a real job: webhook, broker registry lookup with the App, the dispatcher's own role on `RunMicrovm`, dockerd in the VM
- drills 3/3: capacity, VM failure, lost webhooks
- guardrails live: SCP 18/18
- the platform dashboard (CloudWatch accepted the body, and its SEARCH graphs show the runs)

**Pending:**
- the RCP and SQS guardrail checks, until the member account's services are active (rerun `just guardrails-test`)
- the shell-token denial live (needs a VM in the member account)
- the broker-outage and fallback drills (manual)

**Release:** `v0.1.0` is tagged once these docs and results are committed. The release workflow builds the assets, SBOM and provenance.
