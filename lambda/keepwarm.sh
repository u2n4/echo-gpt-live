#!/usr/bin/env bash
# Keep the Smart Home Lambda and its connection to tvd warm, so a voice command does not pay a
# cold start (~130 ms) and a fresh TLS handshake to the house (~0.7-1 s measured).
#   lambda/keepwarm.sh enable    EventBridge rule acv-tv-warm: {"warm":"tv"} every minute
#   lambda/keepwarm.sh disable   remove the rule, its target and its invoke permission
#   lambda/keepwarm.sh status
# Each ping is one read-only state query to tvd. Cost: ~43k invocations a month, inside the
# Lambda free tier; scheduled EventBridge rules are free.
set -euo pipefail
REGION="us-east-1"
PROFILE="alexa-chatgpt-voice"
AWS=(aws --profile "$PROFILE" --region "$REGION")
FN_NAME="acv-chatgpt-voice"
RULE="acv-tv-warm"
SID="acv-tv-warm-events"
cmd="${1:?usage: keepwarm.sh enable|disable|status}"

case "$cmd" in
  enable)
    FN_ARN="$("${AWS[@]}" lambda get-function --function-name "$FN_NAME" --query Configuration.FunctionArn --output text)"
    RULE_ARN="$("${AWS[@]}" events put-rule --name "$RULE" --schedule-expression "rate(1 minute)" \
      --state ENABLED --description "keep acv-chatgpt-voice and its tvd connection warm" --query RuleArn --output text)"
    # Only this rule may invoke the function through EventBridge.
    "${AWS[@]}" lambda remove-permission --function-name "$FN_NAME" --statement-id "$SID" 2>/dev/null || true
    "${AWS[@]}" lambda add-permission --function-name "$FN_NAME" --statement-id "$SID" \
      --action lambda:InvokeFunction --principal events.amazonaws.com --source-arn "$RULE_ARN" >/dev/null
    "${AWS[@]}" events put-targets --rule "$RULE" \
      --targets "[{\"Id\":\"acv-lambda\",\"Arn\":\"$FN_ARN\",\"Input\":\"{\\\"warm\\\":\\\"tv\\\"}\"}]" >/dev/null
    echo "keep-warm enabled: $RULE_ARN -> $FN_NAME every minute"
    ;;
  disable)
    "${AWS[@]}" events remove-targets --rule "$RULE" --ids acv-lambda >/dev/null 2>&1 || true
    "${AWS[@]}" events delete-rule --name "$RULE" 2>/dev/null || true
    "${AWS[@]}" lambda remove-permission --function-name "$FN_NAME" --statement-id "$SID" 2>/dev/null || true
    echo "keep-warm disabled"
    ;;
  status)
    "${AWS[@]}" events describe-rule --name "$RULE" --query '[Name,State,ScheduleExpression]' --output text 2>/dev/null \
      || echo "no rule $RULE"
    ;;
  *) echo "usage: keepwarm.sh enable|disable|status"; exit 2 ;;
esac
