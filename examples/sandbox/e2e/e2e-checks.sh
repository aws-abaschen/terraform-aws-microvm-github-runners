#!/usr/bin/env bash
# Allow and deny checks of a repository deploy session (phase1 | phase2).
# Needs AWS credentials, AWS_REGION, STATE (tfstate bucket) and BOUNDARY.
set -uo pipefail
phase=$1
tagger=$2
repo=$(echo "$GITHUB_REPOSITORY" | tr '[:upper:]' '[:lower:]')
id=$(printf %s "$repo" | sha256sum | cut -c1-10)
other=0000000000
fail=0
err=$(mktemp)

allowed() {
  local name=$1; shift
  if "$@" >"$err" 2>&1; then echo "PASS $name"; else echo "FAIL $name: $(head -c 300 "$err")"; fail=1; fi
}
denied() {
  local name=$1; shift
  if "$@" >"$err" 2>&1; then echo "FAIL $name: allowed"; fail=1
  elif grep -qE 'AccessDenied|not authorized|explicit deny' "$err"; then echo "PASS $name (denied)"
  else echo "FAIL $name: $(head -c 300 "$err")"; fail=1; fi
}

arn=$(aws sts get-caller-identity --query Arn --output text)
echo "session: $arn"
case $phase in
  phase1) [[ $arn == *":assumed-role/gha-microvm-deploy-$id/"* ]] || { echo "FAIL unexpected role"; fail=1; } ;;
  phase2) [[ $arn == *":assumed-role/gha-microvm-deploy/$id-"* ]] || { echo "FAIL unexpected role"; fail=1; } ;;
esac

trust='{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole"}]}'
role="gha-microvm-$id-e2e-$phase"
key="$id/e2e-$phase-$GITHUB_RUN_ID.txt"

allowed "read own registry entry" aws ssm get-parameter --name "/gha-microvm/registry/$id" --with-decryption
denied "read another registry entry" aws ssm get-parameter --name "/gha-microvm/registry/$other"
allowed "read platform config" aws ssm get-parameter --name /gha-microvm/platform/config --with-decryption
allowed "write own state prefix" aws s3api put-object --bucket "$STATE" --key "$key" --body "$0"
allowed "delete own state object" aws s3api delete-object --bucket "$STATE" --key "$key"
denied "write another state prefix" aws s3api put-object --bucket "$STATE" --key "$other/e2e.txt" --body "$0"
denied "create role without boundary" aws iam create-role --role-name "$role" --assume-role-policy-document "$trust" \
  --tags "Key=gha-repo-id,Value=$id" "Key=gha-repo,Value=$repo"
denied "create role with another repo's tags" aws iam create-role --role-name "$role" --assume-role-policy-document "$trust" \
  --permissions-boundary "$BOUNDARY" --tags "Key=gha-repo-id,Value=$other" "Key=gha-repo,Value=someone/else"
denied "create role named for another repo" aws iam create-role --role-name "gha-microvm-$other-e2e" --assume-role-policy-document "$trust" \
  --permissions-boundary "$BOUNDARY" --tags "Key=gha-repo-id,Value=$id" "Key=gha-repo,Value=$repo"
allowed "create own role (boundary, own tags)" aws iam create-role --role-name "$role" --assume-role-policy-document "$trust" \
  --permissions-boundary "$BOUNDARY" --tags "Key=gha-repo-id,Value=$id" "Key=gha-repo,Value=$repo"
denied "retag own role to another repo" aws iam tag-role --role-name "$role" --tags "Key=gha-repo-id,Value=$other"
denied "remove own role's boundary" aws iam delete-role-permissions-boundary --role-name "$role"
allowed "delete own role" aws iam delete-role --role-name "$role"
denied "invoke the tagger" aws lambda invoke --function-name "$tagger" --payload '{}' /dev/null
denied "update the broker" aws lambda update-function-configuration --function-name gha-microvm-broker --description x
denied "read the GitHub App key" aws secretsmanager get-secret-value --secret-id gha-microvm/platform/github-app
rm -f "$err"
exit $fail
