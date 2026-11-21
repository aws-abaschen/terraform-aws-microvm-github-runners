# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately through GitHub Security Advisories:
https://github.com/aws-abaschen/terraform-aws-microvm-github-runners/security/advisories/new

Do not open public issues for security problems. You will get an answer within 5 working days.

## Supported versions

Only the latest release receives fixes.

## Design

The threat model and controls are documented at
https://aws-abaschen.github.io/terraform-aws-microvm-github-runners/security.html

## Verifying releases

```sh
gh release download <tag> --repo aws-abaschen/terraform-aws-microvm-github-runners
sha256sum --ignore-missing -c SHA256SUMS
gh attestation verify lambda.zip --repo aws-abaschen/terraform-aws-microvm-github-runners
gh attestation verify image-arm64.zip --repo aws-abaschen/terraform-aws-microvm-github-runners
```
