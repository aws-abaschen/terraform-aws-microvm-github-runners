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
- Phase 2 tagger (`bootstrap.tagger_github_owners`): a function that exchanges a GitHub OIDC token for a session of one shared deploy role tagged with the repository, after checking the signature, owner, registry entry, environment and replay. `per_repository_deploy_roles = false` removes the per-repository deploy roles. Example workflow `deploy-runners-tagger.yml`.
- `just iam-sim` (`cmd/iamsim`, `tools/iamsim`): renders the boundary, the phase 2 trust policies and the guardrails offline and checks them with `iam:SimulateCustomPolicy` (61 boundary and 11 SCP cases) and Access Analyzer. The trust policies moved into `bootstrap/policy` for this.
- Documentation page `testing.html`: test layers, what they found, and the sample repositories.
- `just sandbox-e2e`: an end-to-end workflow in the sandbox test repository that checks both deploy paths against real AWS.
- Alarms, runbooks, resilience drills and DORA mapping.
- CI (tests, lint, govulncheck, tflint, checkov) and release with SBOM and build provenance.

### Changed (breaking)

- `bootstrap.repositories[*]` drops `job_secret_prefixes`, `job_parameter_paths` and `additional_job_actions`; use `gha/<owner>/<repo>/` paths, tag shared secrets and parameters `gha-repo-id=<id>`, and `bootstrap.additional_job_actions` for the whole account.
- `repo-runner` drops `job_secret_prefixes` and `job_parameter_paths`, and no longer reserves Lambda concurrency unless `reserve_concurrency = true`.
- Platform configuration: `boundary_arn` and `registry_path` replace `boundaries`. The broker reads the registry instead of `ALLOWED_REPOSITORIES`.

### Fixed

- Bucket policy: `DenyUnencryptedUploads` used `StringNotEqualsIfExists`, which also matches a missing header, so every upload without an explicit `aws:kms` header was denied, Terraform state included. It now denies only a present header other than `aws:kms` (a `Null` test plus `StringNotEquals`). The consumer example drops `encrypt = true`, which sends `AES256`.
- Deploy role trust: support GitHub's immutable OIDC subject `repo:<owner>@<owner_id>/<repo>@<repository_id>:...`, the default for new repositories, through the new `owner_id` and `repository_id` of `bootstrap.repositories` (exact match; Access Analyzer rejects a wildcard id). The tagger checks a pinned `repository_id`.
- `tagger_github_owners` is a map of owner to numeric owner id (was a list): the entry role trusts `repo:<owner>@<id>/*` and the tagger checks `repository_owner_id`.

- Network connector operator role: trust `lambda.amazonaws.com` without `aws:SourceAccount`, which `CreateNetworkConnector` rejects.
- KMS key policy: allow the Lambda service principal to build and launch MicroVM images; dispatcher gets `kms:ReEncrypt*`.
- Repository boundary policy: under the IAM limit of 6,144 characters, with a plan-time check.
- MicroVM image: `install_weak_deps=0` for microdnf, OpenSSL from the base image's `openssl-snapsafe-libs`.
- Windows recipes: quote Terraform arguments for PowerShell.
