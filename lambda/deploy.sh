#!/usr/bin/env bash
# Deploy the ACV Smart Home Lambda to AWS (us-east-1) and wire it to the skill.
# Requires: aws CLI authenticated (profile alexa-chatgpt-voice), signing key, Funnel URL.
# Cost: AWS Lambda free tier covers this easily (a few invocations per session).
# Uses an explicit named profile + region on EVERY call (never the default profile).
set -euo pipefail

REGION="us-east-1"                 # ar-SA + en-US -> North America -> us-east-1
PROFILE="alexa-chatgpt-voice"
export AWS_PROFILE="$PROFILE" AWS_REGION="$REGION" AWS_DEFAULT_REGION="$REGION"
AWS=(aws --profile "$PROFILE" --region "$REGION")

FN_NAME="acv-chatgpt-voice"
ROLE_NAME="acv-lambda-role"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KEY_FILE="$HOME/.config/acv/signing.key"
FUNNEL_FILE="$HOME/.config/acv/funnel-url"

command -v aws >/dev/null || { echo "aws CLI not installed"; exit 1; }
[ -f "$KEY_FILE" ] || { echo "signing key missing (acv init-secret)"; exit 1; }
[ -f "$FUNNEL_FILE" ] || { echo "funnel url missing (acv enable-public-endpoint)"; exit 1; }

SIGNING_KEY="$(cat "$KEY_FILE")"
UBUNTU_URL="$(cat "$FUNNEL_FILE")"

# The Alexa invoke permission is always restricted to one skill: $1, or the id saved in
# ~/.config/acv/skill-id. Checked before anything changes; there is no unrestricted mode.
SKILL_ID="${1:-}"
if [ -z "$SKILL_ID" ] && [ -s "$HOME/.config/acv/skill-id" ]; then SKILL_ID="$(tr -d ' \n' < "$HOME/.config/acv/skill-id")"; fi
[[ "$SKILL_ID" =~ ^amzn1\.ask\.skill\.[0-9a-f-]{36}$ ]] || { echo "no valid skill id (arg or ~/.config/acv/skill-id), refusing"; exit 1; }

TMP="" ENV_JSON=""
trap 'rm -rf "$TMP"; rm -f "$ENV_JSON"' EXIT

ACCOUNT_ID="$("${AWS[@]}" sts get-caller-identity --query Account --output text)"
echo "account=$ACCOUNT_ID region=$REGION profile=$PROFILE ubuntu=$UBUNTU_URL"

# 0. Back up what is live before touching it: configuration (all env vars), invoke policy, code.
#    Fail closed: only an explicit ResourceNotFoundException counts as "nothing to back up".
#    A new timestamped folder every run; refuse rather than overwrite one that already exists.
set +e
CODE_URL="$("${AWS[@]}" lambda get-function --function-name "$FN_NAME" --query Code.Location --output text 2>&1)"; RC=$?
set -e
if [ $RC -ne 0 ]; then
  grep -q ResourceNotFoundException <<<"$CODE_URL" || { echo "cannot read the live function, refusing: $CODE_URL"; exit 1; }
  EXISTS=0
else
  EXISTS=1
  BACKUP="$HOME/.config/acv/backups/lambda-$(date -u +%Y%m%dT%H%M%SZ)"
  [ -e "$BACKUP" ] && { echo "backup $BACKUP already exists, refusing"; exit 1; }
  (umask 077; mkdir -p "$BACKUP")
  "${AWS[@]}" lambda get-function-configuration --function-name "$FN_NAME" > "$BACKUP/configuration.json"
  set +e
  POLICY="$("${AWS[@]}" lambda get-policy --function-name "$FN_NAME" 2>&1)"; RC=$?
  set -e
  if [ $RC -eq 0 ]; then
    printf '%s\n' "$POLICY" > "$BACKUP/policy.json"
  elif grep -q ResourceNotFoundException <<<"$POLICY"; then
    echo "{}" > "$BACKUP/policy.json"   # the function really has no resource policy
  else
    echo "cannot read the invoke policy, refusing: $POLICY"; exit 1
  fi
  # The presigned URL carries credentials: hand it to curl on stdin, never on its argv.
  printf 'url = "%s"\n' "$CODE_URL" | (umask 077; curl -sf -o "$BACKUP/function.zip" --config -)
  python3 - "$BACKUP" "$FN_NAME" <<'PY' || { echo "backup incomplete, refusing to deploy"; exit 1; }
import json, sys, zipfile
b, fn = sys.argv[1], sys.argv[2]
assert json.load(open(f"{b}/configuration.json"))["FunctionName"] == fn
p = json.load(open(f"{b}/policy.json"))
if "Policy" in p:  # the policy document itself is a JSON string inside the response
    assert isinstance(json.loads(p["Policy"])["Statement"], list)
assert zipfile.is_zipfile(f"{b}/function.zip")
PY
  chmod 600 "$BACKUP"/*
  echo "backed up live function -> $BACKUP (restore: lambda/restore.sh $BACKUP)"
fi

# 1. IAM role (basic Lambda execution = CloudWatch Logs only, least privilege) — idempotent
if ! "${AWS[@]}" iam get-role --role-name "$ROLE_NAME" >/dev/null 2>&1; then
  "${AWS[@]}" iam create-role --role-name "$ROLE_NAME" \
    --assume-role-policy-document '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole"}]}' >/dev/null
  "${AWS[@]}" iam attach-role-policy --role-name "$ROLE_NAME" \
    --policy-arn arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole
  echo "created role $ROLE_NAME; waiting for IAM propagation..."
  sleep 12
fi
ROLE_ARN="arn:aws:iam::${ACCOUNT_ID}:role/${ROLE_NAME}"

# 2. package (single-file ES module)
TMP="$(mktemp -d)"
cp "$ROOT/lambda/index.mjs" "$TMP/"
( cd "$TMP" && zip -q function.zip index.mjs )

# 3. create or update function
if [ "$EXISTS" = 1 ]; then
  "${AWS[@]}" lambda update-function-code --function-name "$FN_NAME" \
    --zip-file "fileb://$TMP/function.zip" >/dev/null
  echo "updated function code"
  # wait for update to settle before config update
  "${AWS[@]}" lambda wait function-updated --function-name "$FN_NAME" 2>/dev/null || sleep 5
else
  "${AWS[@]}" lambda create-function --function-name "$FN_NAME" \
    --runtime nodejs22.x --handler index.handler --role "$ROLE_ARN" \
    --timeout 10 --memory-size 256 \
    --zip-file "fileb://$TMP/function.zip" >/dev/null
  echo "created function"
  "${AWS[@]}" lambda wait function-active-v2 --function-name "$FN_NAME" 2>/dev/null || sleep 8
fi

# 4. environment (secret + url) — NOT stored in git; redacted from output
#
# The base is the variable map read into the backup BEFORE anything changed (step 0), never a
# second read here: a failed read used to be swallowed and replaced with defaults, which would
# have renamed the Intercom endpoint and reset its display category (Grok). Values are MERGED,
# so a variable set by hand on the function (ACV_ENDPOINT_HEALTH, ACV_TV_NAME, ...) survives.
#
# The friendly name is what the user SAYS OUT LOUD ("Alexa, talk to the intercom"), so the live
# value is kept unless ACV_FRIENDLY_NAME=... is given; the same for ACV_DISPLAY_CATEGORIES=...
# (an experimental variable). A brand-new function starts from Intercom / CAMERA.
# The TV key comes from tvd's own key file; without that file the TV endpoint stays off.
TV_KEY_FILE="$HOME/.config/tvd/signing.key"
BASE_CFG=""
[ "$EXISTS" = 1 ] && BASE_CFG="$BACKUP/configuration.json"
ENV_JSON="$(umask 077; mktemp)"
UBUNTU_URL="$UBUNTU_URL" SIGNING_KEY="$SIGNING_KEY" BASE_CFG="$BASE_CFG" \
  NAME_OVERRIDE="${ACV_FRIENDLY_NAME:-}" CATS_OVERRIDE="${ACV_DISPLAY_CATEGORIES:-}" \
  TV_KEY_FILE="$TV_KEY_FILE" TV_NAME="${ACV_TV_NAME:-}" python3 -c '
import json, os, sys
env = os.environ
live = {}
if env["BASE_CFG"]:
    live = (json.load(open(env["BASE_CFG"])).get("Environment") or {}).get("Variables") or {}
    if not live:
        sys.exit("the backup has no environment variables; refusing to write a map from scratch")
live["ACV_UBUNTU_URL"] = env["UBUNTU_URL"]
live["ACV_SIGNING_KEY"] = env["SIGNING_KEY"]
live["ACV_ENDPOINT_ID"] = "acv-chatgpt-voice-1"
live["ACV_FRIENDLY_NAME"] = env["NAME_OVERRIDE"] or live.get("ACV_FRIENDLY_NAME") or "Intercom"
live["ACV_DISPLAY_CATEGORIES"] = env["CATS_OVERRIDE"] or live.get("ACV_DISPLAY_CATEGORIES") or "CAMERA"
if os.path.exists(env["TV_KEY_FILE"]):
    live["ACV_TV_KEY"] = open(env["TV_KEY_FILE"]).read().strip()
if env.get("TV_NAME"):  # what he says out loud; set once with ACV_TV_NAME=..., then kept
    live["ACV_TV_NAME"] = env["TV_NAME"]
json.dump({"Variables": live}, sys.stdout)
' > "$ENV_JSON"
echo "friendly name: $(python3 -c 'import json,sys; v=json.load(open(sys.argv[1]))["Variables"]; print(v["ACV_FRIENDLY_NAME"], "  display categories:", v["ACV_DISPLAY_CATEGORIES"], "  tv name:", v.get("ACV_TV_NAME", "(default)"))' "$ENV_JSON")"
"${AWS[@]}" lambda update-function-configuration --function-name "$FN_NAME" \
  --environment "file://$ENV_JSON" >/dev/null
echo "set environment: $(python3 -c 'import json,sys; print(" ".join(sorted(json.load(open(sys.argv[1]))["Variables"])))' "$ENV_JSON") (values redacted)"

# 5. allow Alexa Smart Home to invoke it, from the one skill checked at the top.
#    Fail closed and never leave a gap (Codex/Grok): a policy that cannot be read is left as it
#    is; a missing restricted grant is ADDED under a new statement id first, and only then are the
#    other Alexa grants removed. An identical grant already in place is left alone.
set +e
POLICY_NOW="$("${AWS[@]}" lambda get-policy --function-name "$FN_NAME" --query Policy --output text 2>&1)"; RC=$?
set -e
if [ $RC -ne 0 ]; then
  grep -q ResourceNotFoundException <<<"$POLICY_NOW" || { echo "cannot read the invoke policy; leaving it as it is: $POLICY_NOW"; exit 1; }
  POLICY_NOW='{"Statement":[]}'
fi
# "<match|nomatch> <sid of every other Alexa grant...>"
read -r HAVE OTHERS <<<"$(SKILL_ID="$SKILL_ID" python3 -c '
import json, os, sys
pol = json.loads(sys.stdin.read())
have, others = "nomatch", []
for s in pol.get("Statement", []):
    if s.get("Principal", {}).get("Service") != "alexa-connectedhome.amazon.com":
        continue
    tok = s.get("Condition", {}).get("StringEquals", {}).get("lambda:EventSourceToken", "")
    if tok == os.environ["SKILL_ID"] and have == "nomatch":
        have = "match"
    else:
        others.append(s["Sid"])
print(have, " ".join(others))
' <<<"$POLICY_NOW")"
if [ "$HAVE" != match ]; then
  NEW_SID="alexa-smarthome-$(date -u +%Y%m%d%H%M%S)"
  "${AWS[@]}" lambda add-permission --function-name "$FN_NAME" \
    --statement-id "$NEW_SID" --action lambda:InvokeFunction \
    --principal alexa-connectedhome.amazon.com \
    --event-source-token "$SKILL_ID" >/dev/null \
    || { echo "add-permission failed; the existing grant was NOT touched"; exit 1; }
  echo "alexa trigger restricted to skill $SKILL_ID ($NEW_SID)"
else
  echo "alexa trigger already restricted to skill $SKILL_ID (unchanged)"
fi
KEPT=""
for sid in $OTHERS; do
  if "${AWS[@]}" lambda remove-permission --function-name "$FN_NAME" --statement-id "$sid" >/dev/null; then
    echo "removed other Alexa grant $sid"
  else
    KEPT="$KEPT $sid"
  fi
done
# An extra grant left behind may be an unrestricted one: that is a failure, not a footnote.
[ -z "$KEPT" ] || { echo "FAILED to remove Alexa grant(s):$KEPT (still in the invoke policy)"; exit 1; }

ARN="arn:aws:lambda:${REGION}:${ACCOUNT_ID}:function:${FN_NAME}"
echo
echo "LAMBDA_ARN=$ARN"
echo "$ARN" > "$HOME/.config/acv/lambda-arn"
# record created resources for cleanup
cat > "$HOME/.config/acv/aws-resources.txt" <<EOR
# AWS resources created by ACV (for cleanup). Region: $REGION  Profile: $PROFILE
lambda_function=$FN_NAME
lambda_arn=$ARN
iam_role=$ROLE_NAME
iam_policy_attached=arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole
log_group=/aws/lambda/$FN_NAME
EOR
echo "saved ARN -> ~/.config/acv/lambda-arn ; resources -> ~/.config/acv/aws-resources.txt"
echo "next: put ARN into alexa-skill/skill.json (both endpoint.uri) and deploy the skill via ASK CLI."
