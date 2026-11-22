# Windows recipes in PowerShell 7: just win::<task> (or just win <task>)
# Windows has no zip and `bash` resolves to WSL, so the bash recipes don't run here.
set shell := ["pwsh", "-NoLogo", "-NoProfile", "-Command"]
set export

AWS_PROFILE := env_var_or_default("AWS_PROFILE", "admin")
CGO_ENABLED := "0"
GOOS := "linux"

runner_version := "2.338.2"
runner_sha_arm64 := "670adb2f268fe21dcbb58b3db0e0aa9d5b3bddfe2c651481336e561ff86de945"
runner_sha_x64 := "baebf204307bac6193480852732f0aba2ae97b6c5597c244f4d5558390147c40"
sandbox := "examples/sandbox"

# List recipes
default:
    @just --list win

# Go unit tests (no race detector: it needs cgo)
test:
    $env:GOOS = ''; go vet ./...; if ($LASTEXITCODE) { exit 1 }; go test -count=1 ./...

# Build the Lambda bundle (one binary, arm64): dist/lambda.zip
package:
    New-Item -ItemType Directory -Force dist/lambda | Out-Null
    $env:GOARCH = 'arm64'; go build -trimpath -buildvcs=false '-ldflags=-s -w -buildid=' -o dist/lambda/bootstrap ./cmd/lambda; if ($LASTEXITCODE) { exit 1 }
    $env:GOOS = ''; go run ./cmd/zipdet -o dist/lambda.zip -C dist/lambda -x bootstrap bootstrap; if ($LASTEXITCODE) { exit 1 }
    (Get-FileHash dist/lambda.zip).Hash.ToLower()

# Build the MicroVM image artifact: dist/image-<arch>.zip (arch: arm64 | x64)
image-artifact arch="arm64" docker="true":
    $ErrorActionPreference = 'Stop'; \
    $map = @{ arm64 = @('arm64', '{{runner_sha_arm64}}'); x64 = @('amd64', '{{runner_sha_x64}}') }; \
    if (-not $map.ContainsKey('{{arch}}')) { throw 'arch must be arm64 or x64' }; $goarch, $sha = $map['{{arch}}']; \
    $out = 'dist/image-{{arch}}'; Remove-Item -Recurse -Force $out -ErrorAction SilentlyContinue; New-Item -ItemType Directory -Force $out, dist/cache | Out-Null; \
    $tgz = 'dist/cache/actions-runner-linux-{{arch}}-{{runner_version}}.tar.gz'; \
    if (-not (Test-Path $tgz)) { Invoke-WebRequest -Uri "https://github.com/actions/runner/releases/download/v{{runner_version}}/actions-runner-linux-{{arch}}-{{runner_version}}.tar.gz" -OutFile $tgz }; \
    if ((Get-FileHash $tgz).Hash.ToLower() -ne $sha) { throw "checksum mismatch: $tgz" }; \
    Copy-Item $tgz "$out/actions-runner.tar.gz"; \
    $env:GOARCH = $goarch; go build -trimpath -buildvcs=false '-ldflags=-s -w -buildid=' -o "$out/supervisor" ./cmd/supervisor; if ($LASTEXITCODE) { exit 1 }; \
    (Get-Content -Raw image/Dockerfile) -replace '(?m)^ARG ENABLE_DOCKER=.*$', 'ARG ENABLE_DOCKER={{docker}}' | Set-Content -NoNewline "$out/Dockerfile"; \
    $v = git describe --tags --always 2>$null; if (-not $v) { $v = 'dev' }; "runner={{runner_version}} supervisor=$v`n" | Set-Content -NoNewline "$out/VERSION"; \
    $env:GOOS = ''; $env:GOARCH = ''; go run ./cmd/zipdet -o 'dist/image-{{arch}}.zip' -C $out -x supervisor Dockerfile supervisor actions-runner.tar.gz VERSION; if ($LASTEXITCODE) { exit 1 }; \
    (Get-FileHash 'dist/image-{{arch}}.zip').Hash.ToLower()

# Check the account boundary, trust policies and guardrails with AWS's policy engine (read-only APIs, nothing deployed)
iam-sim:
    terraform -chdir=tools/iamsim init -input=false | Out-Null; if ($LASTEXITCODE) { exit 1 }
    terraform -chdir=tools/iamsim apply -auto-approve -input=false | Out-Null; if ($LASTEXITCODE) { exit 1 }
    $env:GOOS = ''; terraform -chdir=tools/iamsim output -json policies | go run ./cmd/iamsim

# Guardrails live test (management account): enable the policy types, create a member account in its own OU, attach the SCP, RCP and tag policy to that OU
guardrails-up email:
    $root = aws organizations list-roots --query 'Roots[0].Id' --output text; foreach ($t in 'SERVICE_CONTROL_POLICY','RESOURCE_CONTROL_POLICY','TAG_POLICY') { aws organizations enable-policy-type --root-id $root --policy-type $t 2>$null | Out-Null };     terraform -chdir=tools/guardrails init -input=false | Out-Null; terraform -chdir=tools/guardrails apply -input=false -auto-approve '-var=account_email={{email}}'

# Run the guardrail checks inside the member account (after guardrails-up)
guardrails-test:
    $env:GOOS = ''; go run ./cmd/guardtest -account (terraform -chdir=tools/guardrails output -raw account_id)

# Detach the guardrails from the test OU; the member account stays (no cost)
guardrails-down email:
    terraform -chdir=tools/guardrails apply -input=false -auto-approve '-var=account_email={{email}}' '-var=attach=false'

# Close the test member account (AWS keeps it suspended for about 90 days)
guardrails-close email:
    terraform -chdir=tools/guardrails destroy -input=false -auto-approve '-var=account_email={{email}}'

# Deploy the sandbox: VPC + NAT, bootstrap, image and one repository (≈ $1–2/day; run sandbox-down after)
sandbox-up repository region="eu-central-1" oidc="true": package (image-artifact "arm64")
    terraform -chdir={{sandbox}} init -input=false; if ($LASTEXITCODE) { exit 1 }
    terraform -chdir={{sandbox}} apply -input=false -auto-approve '-var=repository={{repository}}' '-var=region={{region}}' '-var=create_github_oidc_provider={{oidc}}' '-target=module.bootstrap'; if ($LASTEXITCODE) { exit 1 }
    terraform -chdir={{sandbox}} apply -input=false -auto-approve '-var=repository={{repository}}' '-var=region={{region}}' '-var=create_github_oidc_provider={{oidc}}'

# Launch one MicroVM like the dispatcher does and verify start, role, KMS, VPC path and terminate
sandbox-smoke region="eu-central-1":
    $env:GOOS = ''; terraform -chdir={{sandbox}} output -json smoke | go run ./cmd/smoke -region {{region}}

# Run the end-to-end IAM workflow (both deploy paths) from the sandbox test repository, after sandbox-up
sandbox-e2e repository:
    $tmp = Join-Path ([IO.Path]::GetTempPath()) ([guid]::NewGuid()); gh repo clone {{repository}} $tmp -- -q; if ($LASTEXITCODE) { exit 1 };     New-Item -ItemType Directory -Force "$tmp/.github/workflows" | Out-Null; Copy-Item {{sandbox}}/e2e/e2e.yml, {{sandbox}}/e2e/e2e-checks.sh "$tmp/.github/workflows/";     git -C $tmp add -A; git -C $tmp diff --cached --quiet; if ($LASTEXITCODE) { git -C $tmp commit -qm 'ci: e2e workflow'; git -C $tmp push -q; if ($LASTEXITCODE) { exit 1 } };     gh api -X PUT 'repos/{{repository}}/environments/runners' | Out-Null;     $in = terraform -chdir={{sandbox}} output -json e2e_inputs | ConvertFrom-Json; $a = @(); foreach ($p in $in.PSObject.Properties) { $a += '-f'; $a += "$($p.Name)=$($p.Value)" };     gh workflow run e2e.yml --repo {{repository}} @a; if ($LASTEXITCODE) { exit 1 }; Start-Sleep 10;     $id = gh run list --repo {{repository}} --workflow e2e.yml --limit 1 --json databaseId --jq '.[0].databaseId'; gh run watch --repo {{repository}} --exit-status $id

# Store the test GitHub App ({app_id, private_key} from its .pem) in a secret that outlives the sandbox. Run it yourself: the key never passes through anyone else
sandbox-app-key app_id pem region="eu-central-1":
    $f = New-TemporaryFile; try { @{ app_id = [int64]'{{app_id}}'; private_key = (Get-Content -Raw '{{pem}}') } | ConvertTo-Json -Compress | Set-Content -NoNewline $f; \
    $name = 'microvm-runner-sandbox/github-app'; aws secretsmanager describe-secret --region {{region}} --secret-id $name 2>$null | Out-Null; \
    if ($LASTEXITCODE -eq 0) { aws secretsmanager put-secret-value --region {{region}} --secret-id $name --secret-string "file://$f" | Out-Null } else { aws secretsmanager create-secret --region {{region}} --name $name --description 'Test GitHub App for the MicroVM runners sandbox' --tags Key=purpose,Value=microvm-runner-sandbox-app --secret-string "file://$f" | Out-Null }; \
    if ($LASTEXITCODE) { exit 1 }; "stored in $name" } finally { Remove-Item $f }

# Real-job test and drills (cmd/realjob): prepare | deploy | webhook | job | drill-capacity | drill-vm-failure | drill-lost-webhooks | unhook | destroy
sandbox-realjob repository command *flags:
    $env:GOOS = ''; go run ./cmd/realjob -repo {{repository}} {{flags}} {{command}}

# Destroy everything the sandbox created
sandbox-down repository region="eu-central-1" oidc="true":
    terraform -chdir={{sandbox}} destroy -input=false -auto-approve '-var=repository={{repository}}' '-var=region={{region}}' '-var=create_github_oidc_provider={{oidc}}'
