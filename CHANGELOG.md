# Changelog

All notable changes follow [Conventional Commits](https://www.conventionalcommits.org) and [Semantic Versioning](https://semver.org).

## [Unreleased]

### Added

- Go Lambda binary (webhook, dispatcher, sweeper, token broker) and in-VM supervisor.
- Terraform modules `bootstrap`, `runner-image`, `repo-runner`.
- MicroVM image with pinned GitHub Actions runner 2.338.2 (arm64 and x64) and Docker.
- VPC-only networking: `VPC_EGRESS` connector, central or local VPC endpoints, no NAT.
- Per-repository permissions boundaries and OIDC deploy roles.
- Alarms, runbooks, resilience drills and DORA mapping.
- CI (tests, lint, govulncheck, tflint, checkov) and release with SBOM and build provenance.
