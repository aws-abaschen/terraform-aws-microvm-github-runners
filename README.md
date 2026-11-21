# terraform-aws-microvm-github-runners

Per-repository, ephemeral GitHub Actions runners on **AWS Lambda MicroVMs**, deployed with Terraform.

- One fresh Firecracker VM per job, terminated afterwards.
- Jobs are capped at 1 hour.
- Each repository deploys its own runner stack into its own target account.
- The runner can read only `gha/<owner>/<repo>/*` secrets.

📐 **Proposal and diagrams:** https://aws-abaschen.github.io/terraform-aws-microvm-github-runners/

## Status

Proposal (v0.1). Implementation milestones are listed on the page.

## Layout (planned)

```
modules/runner-image   awscc_lambda_microvm_image + build role
modules/repo-runner    webhook λ, SQS+DLQ, dispatcher λ, sweeper, DynamoDB, IAM
modules/bootstrap      GitHub OIDC provider + per-repo deploy role
image/                 Dockerfile + Go supervisor
lambda/                Go: webhook, dispatcher, sweeper
docs/                  GitHub Pages site
```

## Local

```sh
just docs   # serve the proposal page on http://localhost:8000
```

## License

MIT
