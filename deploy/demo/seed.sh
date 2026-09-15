#!/usr/bin/env bash
# Seeds the opsagent demo (reachable via a kubectl port-forward on :8080) with
# test incidents, triggers an agent diagnosis end-to-end, and prints the
# resulting report. Safe to re-run; intake is idempotent.
set -euo pipefail

BASE="${BASE:-http://localhost:8080}"

echo ">> creating test incidents"
curl -s -X POST "$BASE/api/v1/incidents" \
  -H 'content-type: application/json' \
  -d '{"host":"web-01","title":"high CPU","severity":"critical","message":"load 40 on 4 cores","source":"prometheus","external_id":"HighCPU:web-01","labels":{"cluster":"prod","service":"nginx"},"owner":"oncall","team":"web"}'
echo
curl -s -X POST "$BASE/api/v1/incidents/alertmanager" \
  -H 'content-type: application/json' \
  -d '{"status":"firing","alerts":[{"status":"firing","labels":{"alertname":"HighMemory","host":"web-01","severity":"warning","cluster":"prod"},"annotations":{"summary":"memory above threshold"},"startsAt":"2026-01-01T00:00:00Z"},{"status":"firing","labels":{"alertname":"DiskAlmostFull","host":"db-01","severity":"critical","cluster":"prod"},"annotations":{"summary":"disk 92% full"},"startsAt":"2026-01-01T00:00:00Z"}]}'
echo
echo ">> incidents:"
curl -s "$BASE/api/v1/incidents?limit=10" | python3 -m json.tool | head -40

# Trigger a diagnosis on the newest incident.
ID=$(curl -s "$BASE/api/v1/incidents?limit=1" | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["id"])')
echo ">> triggering diagnosis on incident #$ID"
curl -s -X POST "$BASE/api/v1/incidents/$ID/diagnose" >/dev/null

echo ">> waiting for the diagnosis to finish"
for i in $(seq 1 60); do
  OUT=$(curl -s "$BASE/api/v1/incidents/$ID/diagnosis")
  STATUS=$(echo "$OUT" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("diagnosis",{}).get("status",""))')
  RUNNING=$(echo "$OUT" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("running",False))')
  if [ "$STATUS" = "done" ]; then
    break
  fi
  [ "$i" = 60 ] && { echo "diagnosis did not complete"; echo "$OUT"; exit 1; }
  sleep 2
done

echo ">> diagnosis result:"
echo "$OUT" | python3 -m json.tool | head -60

echo
echo "demo ready: dashboard at http://localhost:8080"