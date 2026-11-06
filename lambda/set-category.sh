#!/usr/bin/env bash
# Switch the deployed Lambda's Discovery display category WITHOUT redeploying code.
#
#   lambda/set-category.sh DOORBELL          # prior-art configuration
#   lambda/set-category.sh CAMERA            # July configuration (the 26-28 s baseline)
#   lambda/set-category.sh CAMERA,DOORBELL   # the other documented option
#
# After changing it you MUST re-run discovery, otherwise Alexa keeps the old endpoint:
#   say "Alexa, discover devices"  (or Alexa app -> Devices -> + -> Add device -> discover)
# Requires an authenticated AWS session:
#   aws login --profile alexa-chatgpt-voice --region us-east-1
set -euo pipefail
CATS="${1:?usage: set-category.sh CAMERA|DOORBELL|CAMERA,DOORBELL [--no-endpoint-health]}"
case "$CATS" in CAMERA|DOORBELL|CAMERA,DOORBELL|DOORBELL,CAMERA) ;; *) echo "invalid display categories" >&2; exit 2;; esac
HEALTH=1
[ "${2:-}" = "--no-endpoint-health" ] && HEALTH=0
PROFILE=alexa-chatgpt-voice
REGION=us-east-1
FN=acv-chatgpt-voice
AWS=(aws --profile "$PROFILE" --region "$REGION")

cur=$("${AWS[@]}" lambda get-function-configuration --function-name "$FN" \
        --query 'Environment.Variables' --output json)
echo "current env: $(echo "$cur" | python3 -c 'import json,sys; d=json.load(sys.stdin) or {}; print({k:("<redacted>" if "KEY" in k else v) for k,v in d.items()})')"
new=$(echo "$cur" | python3 -c "
import json,sys
d=json.load(sys.stdin) or {}
d['ACV_DISPLAY_CATEGORIES']=sys.argv[1]
d['ACV_ENDPOINT_HEALTH']=sys.argv[2]
print(json.dumps({'Variables': d}))
" "$CATS" "$HEALTH")
"${AWS[@]}" lambda update-function-configuration --function-name "$FN" \
  --environment "$new" --query 'LastUpdateStatus' --output text
"${AWS[@]}" lambda wait function-updated --function-name "$FN"
echo "set ACV_DISPLAY_CATEGORIES=$CATS ACV_ENDPOINT_HEALTH=$HEALTH"
echo 'NOW SAY: "Alexa, discover devices"  -- the change is inert until discovery re-runs.'
