# Changelog

All notable changes follow [Conventional Commits](https://www.conventionalcommits.org) and [Semantic Versioning](https://semver.org).

## [Unreleased]

### Added

- Go Lambda binary (webhook, dispatcher, sweeper, token broker) and in-VM supervisor.
- Terraform modules `bootstrap`, `runner-image`, `repo-runner`.
- MicroVM image with pinned GitHub Actions runner 2.338.2 (arm64 and x64) and Docker.
- VPC-only networking: `VPC_EGRESS` connector, central or local VPC endpoints, no NAT.
- One generic ABAC boundary per account: principal tags `gha-repo-id` and `gha-repo` scope every permission through policy variables, so IAM no longer grows with the number of repositories (`docs/iam.html`).
- Repository registry (`/<prefix>/registry/<id>`) read by the broker and `repo-runner`.
- Module `org-guardrails`: SCP, RCP and tag policy for the repository tags.
- OIDC deploy roles tagged with the repository.
- Alarms, runbooks, resilience drills and DORA mapping.
- CI (tests, lint, govulncheck, tflint, checkov) and release with SBOM and build provenance.

### Changed (breaking)

- `bootstrap.repositories[*]` drops `job_secret_prefixes`, `job_parameter_paths` and `additional_job_actions`; use `gha/<owner>/<repo>/` paths, tag shared secrets and parameters `gha-repo-id=<id>`, and `bootstrap.additional_job_actions` for the whole account.
- `repo-runner` drops `job_secret_prefixes` and `job_parameter_paths`, and no longer reserves Lambda concurrency unless `reserve_concurrency = true`.
- Platform configuration: `boundary_arn` and `registry_path` replace `boundaries`. The broker reads the registry instead of `ALLOWED_REPOSITORIES`.

### Fixed

- Network connector operator role: trust `lambda.amazonaws.com` without `aws:SourceAccount`, which `CreateNetworkConnector` rejects.
- KMS key policy: allow the Lambda service principal to build and launch MicroVM images; dispatcher gets `kms:ReEncrypt*`.
- Repository boundary policy: under the IAM limit of 6,144 characters, with a plan-time check.
- MicroVM image: `install_weak_deps=0` for microdnf, OpenSSL from the base image's `openssl-snapsafe-libs`.
- Windows recipes: quote Terraform arguments for PowerShell.
