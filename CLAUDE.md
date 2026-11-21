# CLAUDE.md

Per-repository ephemeral GitHub Actions runners on AWS Lambda MicroVMs, deployed with Terraform. Public repo in the `aws-abaschen` GitHub org. Docs: https://aws-abaschen.github.io/terraform-aws-microvm-github-runners/

## Conventions

- Conventional commits; commit and push when a task is done.
- Keep answers short; no summary after completion.
- New dependencies: use the latest version; if not possible, add a comment explaining why.
- GitHub Actions: always pin by commit SHA with a `# vX.Y.Z` comment, latest release, also in docs and examples.
- Maintain the `justfile`: a developer must be able to build, deploy and test with it.
- Enterprise and DORA context: no NAT in workload accounts, central egress and VPC endpoints, Lambdas in VPC, KMS everywhere, audit logs, runbooks.

## Layout

- `cmd/lambda`: one Go binary, `FUNCTION_ROLE` = webhook | dispatcher | sweeper | broker.
- `cmd/supervisor`: runs inside the MicroVM (hooks on :9000, reads and deletes the JIT SSM parameter, starts dockerd, runs `run.sh --jitconfig`).
- `cmd/smoke`: sandbox smoke test, launches one MicroVM like the dispatcher.
- `cmd/zipdet`: reproducible zips with Unix mode bits (used by every build, also on Windows).
- `win.just`: PowerShell recipes for Windows (`just win::package`, `just win::sandbox-up …`); no zip and `bash` is WSL there.
- `internal/`: broker (presigned STS caller check), runner (dispatch + sweep), github, webhook, awsx, obs.
- `modules/`: `bootstrap` (per account), `runner-image`, `repo-runner` (per repository).
- `examples/`: `bootstrap-account`, `consumer-repo`, `sandbox`.

## Checks

```sh
just check        # gofmt, vet, go test -race, terraform validate
golangci-lint run ./...
tflint --recursive --config "$PWD/.tflint.hcl"
checkov -d modules --framework terraform
```

## Current task: sandbox test in a single account

Account 067751999967, Region eu-central-1, SSO admin profile. The account already has the GitHub OIDC provider, so pass `false` for `oidc`.

On Windows prefix the sandbox recipes with `win::` (e.g. `just win::sandbox-up …`).

1. `just sandbox-up aws-abaschen/microvm-runner-sandbox eu-central-1 false` (about 15 minutes, the image build is slow).
2. `just sandbox-smoke eu-central-1`, then read the VM logs (`aws logs tail <log_group> --since 15m`).
3. Fix whatever fails. Unverified assumptions to check first:
   - MicroVM execution role trust principal `lambda.amazonaws.com` (modules/repo-runner/iam.tf).
   - Lifecycle hooks fire with the `NO_INGRESS` connector.
   - KMS key on `RunMicrovm` (`KmsKeyIdentifier`) works with the dispatcher's permissions.
   - Network connector operator role trust (both `lambda.amazonaws.com` and `network-connectors.lambda.amazonaws.com` are allowed).
   - IMDS credentials inside the VM (default SDK chain).
4. Always finish with `just sandbox-down aws-abaschen/microvm-runner-sandbox eu-central-1 false` so nothing keeps costing money (NAT gateway ≈ $1–2/day). Check nothing tagged `purpose=microvm-runner-sandbox` is left.
5. If the smoke test passes: optional end-to-end test with a GitHub App on a private test repo, then tag `v0.1.0` (the release workflow builds assets, SBOM and provenance).

Also pending: add an entry for this project in the central docs repository `aws-baschenis-ci/docs`.
