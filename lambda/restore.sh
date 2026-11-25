#!/usr/bin/env bash
# Put the Smart Home Lambda back exactly as a deploy.sh backup recorded it:
#   lambda/restore.sh ~/.config/acv/backups/lambda-<UTC timestamp>
# Restores the code, every environment variable, and the Alexa invoke permission. Afterwards say
# "Alexa, discover devices" so Alexa forgets endpoints the restored code no longer advertises.
# Everything is validated before the first change; an unrestricted grant is never re-created.
set -euo pipefail

REGION="us-east-1"
PROFILE="alexa-chatgpt-voice"
AWS=(aws --profile "$PROFILE" --region "$REGION")
FN_NAME="acv-chatgpt-voice"
B="${1:?usage: restore.sh <backup dir>}"
for f in configuration.json policy.json function.zip; do
  [ -s "$B/$f" ] || { echo "$B/$f missing or empty, refusing"; exit 1; }
done

# The invoke permission as recorded: "token <skill id>", "absent", or "open" (refused).
ENV_JSON="$(umask 077; mktemp)"
trap 'rm -f "$ENV_JSON"' EXIT
STATE="$(python3 - "$B" "$FN_NAME" "$ENV_JSON" <<'PY'
import json, re, sys, zipfile
b, fn, env_out = sys.argv[1], sys.argv[2], sys.argv[3]
c = json.load(open(f"{b}/configuration.json"))
assert c["FunctionName"] == fn
variables = (c.get("Environment") or {}).get("Variables")
assert isinstance(variables, dict) and variables, "backup has no environment variables"
assert all(isinstance(k, str) and isinstance(v, str) for k, v in variables.items())
with open(env_out, "w") as f:  # written now, so nothing can fail after the code is replaced
    json.dump({"Variables": variables}, f)
assert zipfile.is_zipfile(f"{b}/function.zip")
p = json.load(open(f"{b}/policy.json"))
pol = json.loads(p["Policy"]) if "Policy" in p else {"Statement": []}
grants = [s for s in pol["Statement"] if s.get("Principal", {}).get("Service") == "alexa-connectedhome.amazon.com"]
if not grants:
    print("absent")
else:
    tok = grants[0].get("Condition", {}).get("StringEquals", {}).get("lambda:EventSourceToken", "")
    ok = all(g.get("Condition", {}).get("StringEquals", {}).get("lambda:EventSourceToken", "") == tok for g in grants)
    print("token " + tok if ok and re.fullmatch(r"amzn1\.ask\.skill\.[0-9a-f-]{36}", tok) else "open")
PY
)" || { echo "backup does not validate, refusing"; exit 1; }
case "$STATE" in
  token\ *|absent) ;;
  *) echo "the backup's permission was not restricted to a skill; refusing (run deploy.sh for a restricted one)"; exit 1 ;;
esac

"${AWS[@]}" lambda update-function-code --function-name "$FN_NAME" --zip-file "fileb://$B/function.zip" >/dev/null
"${AWS[@]}" lambda wait function-updated --function-name "$FN_NAME" 2>/dev/null || sleep 5
"${AWS[@]}" lambda update-function-configuration --function-name "$FN_NAME" --environment "file://$ENV_JSON" >/dev/null
echo "code and environment restored"

# The current Alexa grants (any statement id), so they can be replaced without a gap: the
# recorded one is added first under a new id, then the others go. Unreadable: stop here.
set +e
POLICY_NOW="$("${AWS[@]}" lambda get-policy --function-name "$FN_NAME" --query Policy --output text 2>&1)"; RC=$?
set -e
if [ $RC -ne 0 ]; then
  grep -q ResourceNotFoundException <<<"$POLICY_NOW" || { echo "code and environment restored, but the invoke policy cannot be read: $POLICY_NOW"; exit 1; }
  POLICY_NOW='{"Statement":[]}'
fi
CURRENT="$(python3 -c '
import json, sys
pol = json.loads(sys.stdin.read())
print(" ".join(s["Sid"] for s in pol.get("Statement", []) if s.get("Principal", {}).get("Service") == "alexa-connectedhome.amazon.com"))
' <<<"$POLICY_NOW")"
if [ "$STATE" != absent ]; then
  NEW_SID="alexa-smarthome-$(date -u +%Y%m%d%H%M%S)"
  "${AWS[@]}" lambda add-permission --function-name "$FN_NAME" --statement-id "$NEW_SID" \
    --action lambda:InvokeFunction --principal alexa-connectedhome.amazon.com \
    --event-source-token "${STATE#token }" >/dev/null \
    || { echo "add-permission failed; the current grants were NOT touched"; exit 1; }
  echo "invoke permission restored (restricted to the recorded skill, $NEW_SID)"
else
  echo "the backup had no Alexa invoke permission"
fi
KEPT=""
for sid in $CURRENT; do
  if "${AWS[@]}" lambda remove-permission --function-name "$FN_NAME" --statement-id "$sid" >/dev/null; then
    echo "removed grant $sid"
  else
    KEPT="$KEPT $sid"
  fi
done
[ -z "$KEPT" ] || { echo "FAILED to remove grant(s):$KEPT (still in the invoke policy)"; exit 1; }
