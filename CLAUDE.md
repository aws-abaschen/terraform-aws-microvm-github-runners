# CLAUDE.md

Per-repository ephemeral GitHub Actions runners on AWS Lambda MicroVMs, deployed with Terraform. Public repo in the `aws-abaschen` GitHub org. Docs: https://aws-abaschen.github.io/terraform-aws-microvm-github-runners/

## Conventions

- Conventional commits; land each finished task as one squashed commit on `main` (no PRs), then push.
- Design for scale (many repositories per account): prefer generic ABAC policies (`aws:PrincipalTag` matched to `aws:ResourceTag` / `aws:RequestTag`) over per-repository policies. Enforce the tags with tag policies and `aws:TagKeys` conditions, and set session tags in the roles that create resources.
- Keep answers short; no summary after completion.
- New dependencies: use the latest version; if not possible, add a comment explaining why.
- GitHub Actions: always pin by commit SHA with a `# vX.Y.Z` comment, latest release, also in docs and examples.
- Maintain the `justfile`: a developer must be able to build, deploy and test with it.
- Enterprise and DORA context: no NAT in workload accounts, central egress and VPC endpoints, Lambdas in VPC, KMS everywhere, audit logs, runbooks.

## Layout

- `cmd/lambda`: one Go binary, `FUNCTION_ROLE` = webhook | dispatcher | sweeper | broker | tagger.
- `cmd/supervisor`: runs inside the MicroVM (hooks on :9000, reads and deletes the JIT SSM parameter, starts dockerd, runs `run.sh --jitconfig`).
- `cmd/smoke`: sandbox smoke test, launches one MicroVM like the dispatcher.
- `cmd/iamsim` + `tools/iamsim`: `just iam-sim`, IAM simulation of the boundary, trust policies and SCP (read-only AWS APIs). Add a case with every boundary or SCP change.
- `cmd/zipdet`: reproducible zips with Unix mode bits (used by every build, also on Windows).
- `win.just`: PowerShell recipes for Windows (`just win::package`, `just win::sandbox-up …`); no zip and `bash` is WSL there.
- `internal/`: broker (presigned STS caller check), tagger (GitHub OIDC to tagged deploy session), runner (dispatch + sweep), github, webhook, awsx, obs.
- `modules/`: `bootstrap` (per account; `bootstrap/policy` renders the generic ABAC boundary), `runner-image`, `repo-runner` (per repository), `org-guardrails` (SCP, RCP, tag policy).
- IAM design and scaling rules: `docs/iam.html`. Tests and sample repositories: `docs/testing.html` (keep its results current).
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
2. `just sandbox-smoke eu-central-1`, then read the VM logs (`aws logs tail <log_group> --since 15m`). Then `just sandbox-e2e aws-abaschen/microvm-runner-sandbox` (real OIDC, both deploy paths).
3. Status (2026-11-21): smoke test passes (5/5) after fixes to the connector trust, KMS key policy, boundary size and Dockerfile.
   Verified: execution role trust `lambda.amazonaws.com`, run and terminate hooks with `NO_INGRESS`, KMS on `RunMicrovm`, connector operator trust (`lambda.amazonaws.com` only, no `aws:SourceAccount`), IMDS credentials in the VM.
   Still unverified: `RunMicrovm` with the dispatcher's own role (the smoke test runs as admin), dockerd readiness inside the VM, a real job end to end.
   ABAC boundary (docs/iam.html): smoke test 5/5 again with the tag-scoped roles. Phase 2 tagger built: smoke test 6/6. `just sandbox-e2e aws-abaschen/microvm-runner-sandbox` (private test repo, created 2026-11-21; ids required: `TF_VAR_github_ids='{owner_id=143795778,repository_id=1466740461}'`) passed 37/37 through both deploy paths with real GitHub OIDC. Unverified: CreateMicrovmImage with tags from a deploy session, broker registry lookup, org-guardrails attached to an OU.
4. Always finish with `just sandbox-down aws-abaschen/microvm-runner-sandbox eu-central-1 false` so nothing keeps costing money (NAT gateway ≈ $1–2/day). Check nothing tagged `purpose=microvm-runner-sandbox` is left.
5. If the smoke test passes: optional end-to-end test with a GitHub App on a private test repo, then tag `v0.1.0` (the release workflow builds assets, SBOM and provenance).

Also pending: add an entry for this project in the central docs repository `aws-baschenis-ci/docs`.
