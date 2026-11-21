# terraform-aws-microvm-github-runners

Ephemeral, per-repository GitHub Actions runners on **AWS Lambda MicroVMs**, deployed with Terraform. Built for regulated landing zones: no NAT in workload accounts, central egress and VPC endpoints, per-repository isolation, DORA-oriented logging and runbooks.

- One Firecracker VM per job, terminated afterwards. Jobs are capped at 1 hour (configurable).
- Each repository deploys its own runners from its own workflow, into its target account.
- A job can only read its own repository's secrets. The GitHub App key never leaves a platform-owned token broker.

📐 **Docs:** https://aws-abaschen.github.io/terraform-aws-microvm-github-runners/ ([security](https://aws-abaschen.github.io/terraform-aws-microvm-github-runners/security.html), [operations](https://aws-abaschen.github.io/terraform-aws-microvm-github-runners/operations.html), [DORA](https://aws-abaschen.github.io/terraform-aws-microvm-github-runners/dora.html))

## Modules

| Module | Applied by | Purpose |
|---|---|---|
| [`modules/bootstrap`](modules/bootstrap) | Platform team, once per account and Region | KMS, buckets, VPC egress connector, optional local endpoints, OIDC, generic ABAC boundary, repository registry and deploy roles, token broker |
| [`modules/org-guardrails`](modules/org-guardrails) | Organization admin, once | SCP, RCP and tag policy that make the repository tags trustworthy ([IAM design](https://aws-abaschen.github.io/terraform-aws-microvm-github-runners/iam.html)) |
| [`modules/runner-image`](modules/runner-image) | Repository workflow | MicroVM image (arm64 or x64) with the pinned runner and Docker |
| [`modules/repo-runner`](modules/repo-runner) | Repository workflow | Webhook, queue, dispatcher, sweeper, state, runner role, alarms |

See [`examples/`](examples) for a full account bootstrap and a consumer repository (Terraform + workflows).

## Quick start

```sh
just check            # gofmt, go vet, go test -race, terraform validate
just package          # dist/lambda.zip (reproducible)
just image-artifact   # dist/image-arm64.zip (runner SHA-256 verified)
just release-assets   # all assets + SHA256SUMS
```

Try it in a single throw-away account (VPC + NAT stand in for the landing zone, ≈ $1–2/day):

```sh
just sandbox-up owner/test-repo eu-central-1 false   # false: account already has the GitHub OIDC provider
just sandbox-smoke eu-central-1                       # one MicroVM, no GitHub needed
just sandbox-down owner/test-repo eu-central-1 false
```

Consumers use release assets: they verify `SHA256SUMS` and the build provenance with `gh attestation verify` before `terraform apply`.

## Requirements

- Terraform ≥ 1.11 (tested 1.17.2), providers `hashicorp/aws` ≥ 6.73, `hashicorp/awscc` ≥ 1.110, `hashicorp/random` ≥ 3.7
- A Region with Lambda MicroVMs
- A GitHub App with repository permissions *Administration: write* and *Actions: read*
- Private subnets with a route to central egress (Transit Gateway) and DNS for VPC endpoints

## Layout

```
cmd/lambda/       one binary: webhook | dispatcher | sweeper | broker
cmd/supervisor/   runs in the MicroVM: lifecycle hooks, JIT fetch, dockerd, run.sh
internal/         broker, runner (dispatch + sweep), github, webhook, awsx, obs
image/            Dockerfile for the MicroVM image
modules/          bootstrap, runner-image, repo-runner
examples/         bootstrap-account, consumer-repo
docs/             GitHub Pages site
```

## License

MIT
