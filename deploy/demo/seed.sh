#!/usr/bin/env bash
# Runs the opsagent demo lifecycle end-to-end against a deployed cluster
# (exposed via kubectl port-forwards: opsagent on :8080, fake gitlab on :9080).
#
#  1. fire an alert burst (web-01 CPU + memory, db-01 disk) -> correlation
#  2. diagnose the web-01 CPU incident (real SSH + repos + docs + grounded LLM)
#  3. the agent proposes a GitLab MR (fake gitlab)
#  4. merge the MR -> MR poller resolves the incident via mr_merged
#  5. re-fire the same alert -> recurrence is recorded
set -euo pipefail

BASE="${BASE:-http://localhost:8080}"
GITLAB_BASE="${GITLAB_BASE:-http://localhost:9080}"

jq() { python3 -c 'import json,sys; d=json.load(sys.stdin); print(json.dumps(d) if isinstance(d,(dict,list)) else d)'; }
get() { curl -s "$BASE$1"; }

echo "== 1. firing an alert burst =="
curl -s -X POST "$BASE/api/v1/incidents" -H 'content-type: application/json' \
  -d '{"host":"web-01","title":"high CPU","severity":"critical","message":"load 40 on 4 cores","source":"prometheus","external_id":"HighCPU:web-01","labels":{"cluster":"prod","service":"nginx"},"owner":"oncall","team":"web"}' >/dev/null
curl -s -X POST "$BASE/api/v1/incidents/alertmanager" -H 'content-type: application/json' \
  -d '{"status":"firing","alerts":[
    {"status":"firing","labels":{"alertname":"HighMemory","host":"web-01","severity":"warning","cluster":"prod"},"annotations":{"summary":"memory above threshold"},"startsAt":"2026-01-01T00:00:00Z"},
    {"status":"firing","labels":{"alertname":"DiskAlmostFull","host":"db-01","severity":"critical","cluster":"prod"},"annotations":{"summary":"disk 92% full"},"startsAt":"2026-01-01T00:00:00Z"}]}' >/dev/null

INC_ID=$(get "/api/v1/incidents?q=high+CPU&limit=5" | python3 -c 'import json,sys; print([i["id"] for i in json.load(sys.stdin) if i["title"].lower()=="high cpu"][0])')
echo "burst incident (HighCPU) id=$INC_ID"
echo "correlation groups:"
get "/api/v1/incidents/$INC_ID/related" | python3 -m json.tool | head -30

echo
echo "== 2. diagnosing incident #$INC_ID (agent drives SSH + repos + docs) =="
curl -s -X POST "$BASE/api/v1/incidents/$INC_ID/diagnose" >/dev/null
for i in $(seq 1 90); do
  OUT=$(get "/api/v1/incidents/$INC_ID/diagnosis")
  STATUS=$(echo "$OUT" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("diagnosis",{}).get("status",""))')
  [ "$STATUS" = "done" ] && break
  [ "$STATUS" = "error" ] && { echo "diagnosis errored:"; echo "$OUT" | python3 -m json.tool; exit 1; }
  [ "$i" = 90 ] && { echo "diagnosis did not complete"; exit 1; }
  sleep 2
done
echo "diagnosis summary:"
echo "$OUT" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["diagnosis"].get("summary",""))'
echo "command runs executed:"
echo "$OUT" | python3 -c 'import json,sys; d=json.load(sys.stdin); [print(" -", r["command_id"], "on", r["host"], "=>", r["status"]) for r in d.get("command_runs",[])]'

INC=$(get "/api/v1/incidents/$INC_ID")
MRURL=$(echo "$INC" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("mr_url",""))')
echo "incident status: $(echo "$INC" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status",""))')"
echo "MR proposed by the agent: $MRURL"

echo
echo "== 3. merging the MR on the (fake) gitlab =="
if [ -n "$MRURL" ]; then
  IID=$(echo "$MRURL" | python3 -c 'import sys; print(sys.stdin.read().rstrip("/").split("/merge_requests/")[-1])')
  curl -s -X POST "$GITLAB_BASE/_mock/merge/$IID" >/dev/null
  echo "merged MR #$IID"
else
  echo "no MR was created; skipping merge"
fi

echo
echo "== 4. waiting for the MR poller to mark the incident resolved =="
for i in $(seq 1 90); do
  INC=$(get "/api/v1/incidents/$INC_ID")
  ST=$(echo "$INC" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("status",""))')
  VIA=$(echo "$INC" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("resolved_via",""))')
  [ "$ST" = "resolved" ] && break
  [ "$i" = 90 ] && { echo "incident not resolved"; exit 1; }
  sleep 2
done
echo "incident #$INC_ID resolved via $VIA"
echo "timeline:"
get "/api/v1/incidents/$INC_ID/events" | python3 -c 'import json,sys; [print(" -", e["kind"], ":", e["detail"]) for e in json.load(sys.stdin)]'

echo
echo "== 5. re-firing the same alert (recurrence) =="
curl -s -X POST "$BASE/api/v1/incidents" -H 'content-type: application/json' \
  -d '{"host":"web-01","title":"high CPU","severity":"critical","message":"load 40 on 4 cores","source":"prometheus","external_id":"HighCPU:web-01","labels":{"cluster":"prod","service":"nginx"}}' >/dev/null
NEW_ID=$(get "/api/v1/incidents?q=high+CPU&limit=5" | python3 -c 'import json,sys; print(max(i["id"] for i in json.load(sys.stdin) if i["title"].lower()=="high cpu"))')
echo "new incident id=$NEW_ID (status: $(get "/api/v1/incidents/$NEW_ID" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("status",""))'))"
get "/api/v1/incidents/$NEW_ID/events" | python3 -c 'import json,sys; [print(" -", e["kind"], ":", e["detail"]) for e in json.load(sys.stdin)]'

echo
echo "demo complete."
echo "dashboard:  $BASE"
echo "incidents:  $BASE/api/v1/incidents"
echo "MR #$IID (fake gitlab): $GITLAB_BASE/projects/acme%2Finfra/merge_requests/$IID"