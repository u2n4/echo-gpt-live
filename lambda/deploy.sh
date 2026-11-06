#!/usr/bin/env bash
# Deploy the ACV Smart Home Lambda to AWS (us-east-1) and wire it to the skill.
# Requires: aws CLI authenticated (profile alexa-chatgpt-voice), signing key, Funnel URL.
# Cost: AWS Lambda free tier covers this easily (a few invocations per session).
# Uses an explicit named profile + region on EVERY call (never the default profile).
set -euo pipefail

SKILL_ID="${1:?a Smart Home skill ID is required}"
[[ "$SKILL_ID" =~ ^amzn1\.ask\.skill\.[A-Za-z0-9-]+$ ]] || { echo "invalid skill ID" >&2; exit 2; }
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
ACCOUNT_ID="$("${AWS[@]}" sts get-caller-identity --query Account --output text)"
echo "account=$ACCOUNT_ID region=$REGION profile=$PROFILE ubuntu=$UBUNTU_URL"

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
trap 'rm -rf -- "$TMP"' EXIT
cp "$ROOT/lambda/index.mjs" "$TMP/"
( cd "$TMP" && zip -q function.zip index.mjs )

# 3. create or update function
if "${AWS[@]}" lambda get-function --function-name "$FN_NAME" >/dev/null 2>&1; then
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
# The friendly name is what the user SAYS OUT LOUD ("Alexa, talk to the intercom"), so it is read
# off the deployed function and preserved. This line used to hardcode "ChatGPT Voice" while the
# deployed value was "Intercom": running the script would have renamed the endpoint out from under
# a working invocation phrase. Same for the display category, which is an experimental variable.
# Override either deliberately with ACV_FRIENDLY_NAME=... / ACV_DISPLAY_CATEGORIES=... in the env.
LIVE_NAME="$("${AWS[@]}" lambda get-function-configuration --function-name "$FN_NAME" \
  --query 'Environment.Variables.ACV_FRIENDLY_NAME' --output text 2>/dev/null || true)"
case "$LIVE_NAME" in ""|None) LIVE_NAME="Intercom";; esac
FRIENDLY_NAME="${ACV_FRIENDLY_NAME:-$LIVE_NAME}"

LIVE_CATS="$("${AWS[@]}" lambda get-function-configuration --function-name "$FN_NAME" \
  --query 'Environment.Variables.ACV_DISPLAY_CATEGORIES' --output text 2>/dev/null || true)"
case "$LIVE_CATS" in ""|None) LIVE_CATS="CAMERA";; esac
DISPLAY_CATS="${ACV_DISPLAY_CATEGORIES:-$LIVE_CATS}"

echo "friendly name: $FRIENDLY_NAME   display categories: $DISPLAY_CATS"
export SIGNING_KEY UBUNTU_URL FRIENDLY_NAME DISPLAY_CATS
python3 - >"$TMP/environment.json" <<'PYENV'
import json, os
print(json.dumps({"Variables": {
    "ACV_UBUNTU_URL": os.environ["UBUNTU_URL"],
    "ACV_SIGNING_KEY": os.environ["SIGNING_KEY"],
    "ACV_ENDPOINT_ID": "acv-chatgpt-voice-1",
    "ACV_FRIENDLY_NAME": os.environ["FRIENDLY_NAME"],
    "ACV_DISPLAY_CATEGORIES": os.environ["DISPLAY_CATS"],
}}))
PYENV
chmod 600 "$TMP/environment.json"
"${AWS[@]}" lambda update-function-configuration --function-name "$FN_NAME" \
  --environment "file://$TMP/environment.json" >/dev/null
echo "set environment (secret redacted)"

# 5. allow Alexa Smart Home to invoke it.
#    If a Skill ID is provided as $1, restrict the trigger to that exact skill (least privilege).
"${AWS[@]}" lambda remove-permission --function-name "$FN_NAME" --statement-id alexa-smarthome 2>/dev/null || true
"${AWS[@]}" lambda add-permission --function-name "$FN_NAME" \
  --statement-id alexa-smarthome --action lambda:InvokeFunction \
  --principal alexa-connectedhome.amazon.com \
  --event-source-token "$SKILL_ID" >/dev/null
echo "alexa trigger restricted to the configured skill"

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
