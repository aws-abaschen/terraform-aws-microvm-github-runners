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
      [ -f "$d/versions.tf" ] || [ -f "$d/main.tf" ] || continue; \
      echo "== $d"; terraform -chdir="$d" init -backend=false -input=false >/dev/null; terraform -chdir="$d" validate; \
    done

# Build the Lambda bundle (one binary, arm64): dist/lambda.zip
package:
    mkdir -p {{dist}}/lambda
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build {{goflags}} -ldflags "{{ldflags}}" -o {{dist}}/lambda/bootstrap ./cmd/lambda
    touch -d @0 {{dist}}/lambda/bootstrap
    rm -f {{dist}}/lambda.zip && (cd {{dist}}/lambda && zip -q -X -D ../lambda.zip bootstrap)
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
    find "$out" -exec touch -d @0 {} +
    rm -f {{dist}}/image-{{arch}}.zip && (cd "$out" && zip -q -X -D ../image-{{arch}}.zip Dockerfile supervisor actions-runner.tar.gz VERSION)
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

# Serve the documentation site locally
docs port="8000":
    python3 -m http.server {{port}} --directory docs

clean:
    rm -rf {{dist}}
