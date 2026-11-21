# On Windows use `just win::<task>` (PowerShell 7 recipes in win.just)
mod win

set shell := ["bash", "-euo", "pipefail", "-c"]

# Pinned GitHub Actions runner. Bump both version and checksums together.
runner_version := "2.338.2"
runner_sha_arm64 := "670adb2f268fe21dcbb58b3db0e0aa9d5b3bddfe2c651481336e561ff86de945"
runner_sha_x64 := "baebf204307bac6193480852732f0aba2ae97b6c5597c244f4d5558390147c40"

dist := "dist"
goflags := "-trimpath -buildvcs=false"
ldflags := "-s -w -buildid="

default:
    @just --list

# Run every check CI runs
check: fmt-check vet test tf-validate

# Go unit tests with the race detector
test:
    go test -race -count=1 ./...

vet:
    go vet ./...

# Fail when Go or Terraform files are not formatted
fmt-check:
    test -z "$(gofmt -l .)" || (gofmt -l . && exit 1)
    terraform fmt -check -recursive modules examples

fmt:
    gofmt -w .
    terraform fmt -recursive modules examples

# Known-vulnerability scan of Go dependencies
vuln:
    go run golang.org/x/vuln/cmd/govulncheck@v1.10.0 ./...

# terraform validate every module and example
tf-validate:
    for d in modules/* examples/*/ examples/consumer-repo/infra/runners; do \
      [ -f "$d/versions.tf" ] || [ -f "$d/main.tf" ] || continue; [ "$d" = "examples/sandbox/" ] && continue; \
      echo "== $d"; terraform -chdir="$d" init -backend=false -input=false >/dev/null; terraform -chdir="$d" validate; \
    done

# Build the Lambda bundle (one binary, arm64): dist/lambda.zip
package:
    mkdir -p {{dist}}/lambda
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build {{goflags}} -ldflags "{{ldflags}}" -o {{dist}}/lambda/bootstrap ./cmd/lambda
    go run ./cmd/zipdet -o {{dist}}/lambda.zip -C {{dist}}/lambda -x bootstrap bootstrap
    sha256sum {{dist}}/lambda.zip

# Build the MicroVM image artifact: dist/image-<arch>.zip (arch: arm64 | x64)
image-artifact arch="arm64" docker="true":
    #!/usr/bin/env bash
    set -euo pipefail
    case "{{arch}}" in
      arm64) goarch=arm64; sha={{runner_sha_arm64}} ;;
      x64)   goarch=amd64; sha={{runner_sha_x64}} ;;
      *) echo "arch must be arm64 or x64"; exit 1 ;;
    esac
    out={{dist}}/image-{{arch}}; rm -rf "$out"; mkdir -p "$out" {{dist}}/cache
    tgz={{dist}}/cache/actions-runner-linux-{{arch}}-{{runner_version}}.tar.gz
    [ -f "$tgz" ] || curl -fsSL -o "$tgz" "https://github.com/actions/runner/releases/download/v{{runner_version}}/actions-runner-linux-{{arch}}-{{runner_version}}.tar.gz"
    echo "$sha  $tgz" | sha256sum -c -
    cp "$tgz" "$out/actions-runner.tar.gz"
    CGO_ENABLED=0 GOOS=linux GOARCH=$goarch go build {{goflags}} -ldflags "{{ldflags}}" -o "$out/supervisor" ./cmd/supervisor
    sed "s/^ARG ENABLE_DOCKER=.*/ARG ENABLE_DOCKER={{docker}}/" image/Dockerfile > "$out/Dockerfile"
    echo "runner={{runner_version}} supervisor=$(git describe --tags --always 2>/dev/null || echo dev)" > "$out/VERSION"
    go run ./cmd/zipdet -o {{dist}}/image-{{arch}}.zip -C "$out" -x supervisor Dockerfile supervisor actions-runner.tar.gz VERSION
    sha256sum {{dist}}/image-{{arch}}.zip

# Build every release asset and its checksums
release-assets: package (image-artifact "arm64") (image-artifact "x64")
    cd {{dist}} && sha256sum lambda.zip image-arm64.zip image-x64.zip > SHA256SUMS && cat SHA256SUMS

# Plan or apply the runners of a consumer repository (run from its infra dir)
deploy dir="examples/consumer-repo/infra/runners" action="plan":
    terraform -chdir={{dir}} init -input=false
    terraform -chdir={{dir}} {{action}} -input=false

# Send a signed fake workflow_job ping to a deployed webhook
ping url secret_arn:
    #!/usr/bin/env bash
    set -euo pipefail
    secret=$(aws secretsmanager get-secret-value --secret-id "{{secret_arn}}" --query SecretString --output text)
    body='{"zen":"ping"}'
    sig=$(printf '%s' "$body" | openssl dgst -sha256 -hmac "$secret" | awk '{print $2}')
    curl -fsS -X POST "{{url}}" -H "X-GitHub-Event: ping" -H "X-Hub-Signature-256: sha256=$sig" -H "Content-Type: application/json" -d "$body"; echo

# Resilience drill: purge in-flight dispatches and let the sweeper recover (DORA Art. 25)
drill-lost-webhooks queue_url:
    aws sqs purge-queue --queue-url "{{queue_url}}"
    @echo "Queue purged. Queued jobs must start within ~5 minutes via the sweeper. Record the result in your test log."

# ---------------------------------------------------------------- Sandbox (single account, throw-away)

sandbox := "examples/sandbox"

# Check the account boundary, trust policies and guardrails with AWS's policy engine (read-only APIs, nothing deployed)
iam-sim:
    terraform -chdir=tools/iamsim init -input=false >/dev/null
    terraform -chdir=tools/iamsim apply -auto-approve -input=false >/dev/null
    terraform -chdir=tools/iamsim output -json policies | go run ./cmd/iamsim

# Deploy the sandbox: VPC + NAT, bootstrap, image and one repository (≈ $1–2/day; run sandbox-down after)
sandbox-up repository region="eu-central-1" oidc="true": package (image-artifact "arm64")
    terraform -chdir={{sandbox}} init -input=false
    terraform -chdir={{sandbox}} apply -input=false -auto-approve -var repository={{repository}} -var region={{region}} -var create_github_oidc_provider={{oidc}} -target=module.bootstrap
    terraform -chdir={{sandbox}} apply -input=false -auto-approve -var repository={{repository}} -var region={{region}} -var create_github_oidc_provider={{oidc}}

# Launch one MicroVM like the dispatcher does and verify start, role, KMS, VPC path and terminate
sandbox-smoke region="eu-central-1":
    terraform -chdir={{sandbox}} output -json smoke | go run ./cmd/smoke -region {{region}}

# Run the end-to-end IAM workflow (both deploy paths) from the sandbox test repository, after sandbox-up
sandbox-e2e repository:
    #!/usr/bin/env bash
    set -euo pipefail
    tmp=$(mktemp -d); gh repo clone {{repository}} "$tmp" -- -q
    mkdir -p "$tmp/.github/workflows"; cp {{sandbox}}/e2e/e2e.yml {{sandbox}}/e2e/e2e-checks.sh "$tmp/.github/workflows/"
    (cd "$tmp" && git add -A && { git diff --cached --quiet || { git commit -qm "ci: e2e workflow" && git push -q; }; })
    gh api -X PUT "repos/{{repository}}/environments/runners" >/dev/null
    mapfile -t args < <(terraform -chdir={{sandbox}} output -json e2e_inputs | jq -r 'to_entries[] | "-f", "\(.key)=\(.value)"')
    gh workflow run e2e.yml --repo {{repository}} "${args[@]}"
    sleep 10
    gh run watch --repo {{repository}} --exit-status "$(gh run list --repo {{repository}} --workflow e2e.yml --limit 1 --json databaseId --jq '.[0].databaseId')"

# Destroy everything the sandbox created
sandbox-down repository region="eu-central-1" oidc="true":
    terraform -chdir={{sandbox}} destroy -input=false -auto-approve -var repository={{repository}} -var region={{region}} -var create_github_oidc_provider={{oidc}}

# Serve the documentation site locally
docs port="8000":
    python3 -m http.server {{port}} --directory docs

clean:
    rm -rf {{dist}}
