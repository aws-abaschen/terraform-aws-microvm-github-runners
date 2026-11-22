#!/usr/bin/env bash
# Exchanges a GitHub OIDC token (audience <prefix>-tagger) for a session of
# the shared deploy role, tagged with this repository, and exports it to the
# following steps. Run after assuming the entry role. Usage: tagger-login.sh <function>
# Transient tagger errors (GitHub keys, registry or replay store unreachable,
# for example on a cold start) are retried with a fresh token; a denial is not.
set -euo pipefail
tagger=$1
audience=${TAGGER_AUDIENCE:-gha-microvm-tagger}
for attempt in 1 2 3; do
  token=$(curl -fsS -H "Authorization: bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" "$ACTIONS_ID_TOKEN_REQUEST_URL&audience=$audience" | jq -r .value)
  echo "::add-mask::$token"
  jq -n --arg t "$token" '{token: $t}' > req.json
  aws lambda invoke --function-name "$tagger" --cli-binary-format raw-in-base64-out --payload fileb://req.json creds.json > invoke.json
  rm -f req.json
  if ! jq -e .FunctionError invoke.json >/dev/null; then
    break
  fi
  msg=$(jq -r .errorMessage creds.json)
  rm -f creds.json invoke.json
  case "$msg" in
    "verifier unavailable" | "registry unavailable" | "replay store unavailable")
      if [ "$attempt" -lt 3 ]; then
        echo "::warning::tagger: $msg (attempt $attempt), retrying"
        sleep $((attempt * 10))
        continue
      fi ;;
  esac
  echo "::error::tagger refused: $msg"
  exit 1
done
for k in AccessKeyId SecretAccessKey SessionToken; do echo "::add-mask::$(jq -r ".$k" creds.json)"; done
{
  echo "AWS_ACCESS_KEY_ID=$(jq -r .AccessKeyId creds.json)"
  echo "AWS_SECRET_ACCESS_KEY=$(jq -r .SecretAccessKey creds.json)"
  echo "AWS_SESSION_TOKEN=$(jq -r .SessionToken creds.json)"
} >> "$GITHUB_ENV"
echo "session expires $(jq -r .Expiration creds.json)"
rm -f creds.json invoke.json
