#!/usr/bin/env bash
# Checks pass "$(cond; echo $?)" on purpose.
# shellcheck disable=SC2319
#
# End-to-end test of the real binary in fake mode (nothing is billed). Needs go, curl and jq.
#   scripts/smoke.sh           # everything, about 3 min
#   scripts/smoke.sh --quick   # skip the completion, forced and SIGTERM runs
set -euo pipefail

QUICK=0
case "${1:-}" in
  "") ;;
  --quick) QUICK=1 ;;
  *) echo "usage: $0 [--quick]" >&2; exit 2 ;;
esac

for tool in go curl jq; do
  command -v "$tool" >/dev/null || { echo "smoke: $tool is required but not on PATH" >&2; exit 2; }
done

# The child must never see a real key, whatever the caller's shell holds.
unset RUNPOD_API_KEY

REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
TMP=$(mktemp -d "${TMPDIR:-/tmp}/coldprobe-smoke.XXXXXX")
BIN=$TMP/coldprobe
DB=$TMP/smoke.db
SERVER_PID=""
T0=$EPOCHREALTIME

cleanup() {
  if [[ -n $SERVER_PID ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill -TERM "$SERVER_PID" 2>/dev/null || true
    for _ in $(seq 1 150); do kill -0 "$SERVER_PID" 2>/dev/null || break; sleep 0.2; done
    kill -KILL "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  local jobs_left
  jobs_left=$(jobs -p) || true
  if [[ -n $jobs_left ]]; then
    # shellcheck disable=SC2086 # word splitting wanted: a list of PIDs
    kill $jobs_left 2>/dev/null || true
  fi
  rm -rf "$TMP" || true
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# ---- results ---------------------------------------------------------------

NAMES=() RESULTS=() REASONS=()
record() { # record PASS|FAIL name reason
  NAMES+=("$2") RESULTS+=("$1") REASONS+=("$3")
  printf '%-4s  %-34s %s\n' "$1" "$2" "$3"
}
pass() { record PASS "$1" "$2"; }
fail() { record FAIL "$1" "$2"; }
check() { # check name condition-result(0/1) pass-reason fail-reason
  if [[ $2 == 0 ]]; then pass "$1" "$3"; else fail "$1" "$4"; fi
}

# ---- HTTP helpers -----------------------------------------------------------

BODY=$TMP/body
CODE=""
# req METHOD PATH [curl args...]: sets CODE, body in $BODY; a curl error becomes CODE=curl-exit-N.
req() {
  local method=$1 path=$2; shift 2
  CODE=$(curl -sS --max-time 15 -o "$BODY" -w '%{http_code}' -X "$method" "$@" "$BASE$path" 2>"$TMP/curl.err") \
    || CODE="curl-exit-$?"
}
J=(-H 'Content-Type: application/json')
detail() { jq -r '.detail // empty' "$BODY" 2>/dev/null || head -c 200 "$BODY"; }
# jqt FILTER [file]: exit 0 if the filter is true on $BODY or file.
jqt() { jq -e "$1" "${2:-$BODY}" >/dev/null 2>&1; }

# ---- server -----------------------------------------------------------------

start_server() { # start_server LOGFILE [COLDPROBE_FAKE_NOSTOCK value]
  COLDPROBE_FAKE=1 COLDPROBE_FAKE_NOSTOCK=${2:-} COLDPROBE_DB=$DB COLDPROBE_ADDR=127.0.0.1:$PORT "$BIN" serve >"$1" 2>&1 &
  SERVER_PID=$!
  local deadline=$((SECONDS + 20))
  until curl -sf --max-time 2 -o /dev/null "$BASE/healthz"; do
    if ! kill -0 "$SERVER_PID" 2>/dev/null || ((SECONDS > deadline)); then
      echo "smoke: server did not become healthy; log tail:" >&2
      tail -n 20 "$1" >&2
      exit 1
    fi
    sleep 0.2
  done
}

stop_server() {
  kill -TERM "$SERVER_PID" 2>/dev/null || true
  local deadline=$((SECONDS + 30))
  while kill -0 "$SERVER_PID" 2>/dev/null && ((SECONDS < deadline)); do sleep 0.1; done
  kill -KILL "$SERVER_PID" 2>/dev/null || true
  wait "$SERVER_PID" 2>/dev/null || true
  SERVER_PID=""
}

free_port() {
  local p
  for _ in $(seq 1 50); do
    p=$((20000 + RANDOM % 20000))
    if ! (echo >"/dev/tcp/127.0.0.1/$p") 2>/dev/null; then echo "$p"; return; fi
  done
  echo "smoke: no free port found" >&2; exit 1
}

# wait_status ID TIMEOUT_S: poll until the run isn't "running"; last snapshot left in $BODY.
wait_status() {
  local deadline=$((SECONDS + $2))
  while ((SECONDS < deadline)); do
    req GET "/api/runs/$1"
    if [[ $CODE == 200 ]] && ! jqt '.run.status == "running"'; then return 0; fi
    sleep 1
  done
  return 1
}

wait_endpoints() {
  local deadline=$((SECONDS + $2))
  while ((SECONDS < deadline)); do
    req GET "/api/runs/$1"
    if [[ $CODE == 200 ]] && jqt '(.configs | length) == 2 and all(.configs[]; (.config.endpointId // "") != "")'; then
      return 0
    fi
    sleep 0.5
  done
  return 1
}

# start_run: sets RUN_ID ("" on failure).
start_run() {
  req POST /api/runs "${J[@]}" --data-binary @"$PARAMS"
  RUN_ID=""
  if [[ $CODE == 201 ]]; then RUN_ID=$(jq -r .id "$BODY"); fi
}

sse_start() {
  curl -sN --max-time 240 "$BASE/api/runs/$1/events" >"$2" 2>/dev/null &
  SSE_PID=$!
}
# sse_check NAME FILE WANT_STATUS: the stream must end by itself (not a curl timeout) on WANT_STATUS.
sse_check() {
  local rc=0
  wait "$SSE_PID" || rc=$?
  local n last
  n=$(grep -c '^event: ' "$2" || true)
  last=$(grep '^data: ' "$2" | tail -n 1 | cut -c7- | jq -r 'select(.type=="run_status") | .data.status' 2>/dev/null || true)
  if [[ $rc == 0 && $n -gt 1 && $last == "$3" ]]; then
    pass "$1" "$n events, ended after run_status $last"
  else
    fail "$1" "curl exit $rc, $n events, last run_status '${last}' (want $3)"
  fi
}

# ---- go ---------------------------------------------------------------------

echo "building..."
(cd "$REPO" && go build -o "$BIN" ./cmd/coldprobe)
PORT=$(free_port)
BASE=http://127.0.0.1:$PORT
start_server "$TMP/server1.log"
echo "server up on $BASE (pid $SERVER_PID, db $DB)"
echo

# 1 pool x OFF/FLASHBOOT = 2 configs.
PARAMS=$TMP/params.json
req GET /api/defaults
if [[ $CODE == 200 ]] && jqt '.levelsRps == [1,3,8] and .budgetCapUsd > 0'; then
  pass "GET /api/defaults" "200, levelsRps [1,3,8], budgetCapUsd $(jq .budgetCapUsd "$BODY")"
else
  fail "GET /api/defaults" "HTTP $CODE, levelsRps $(jq -c .levelsRps "$BODY" 2>/dev/null || echo '?'), budgetCapUsd $(jq .budgetCapUsd "$BODY" 2>/dev/null || echo '?')"
fi
# The defaults object is sent back whole, budgetCapUsd and all, as the form does.
jq -c '.pools=["AMPERE_16"] | .coldStarts=2 | .warmRequests=5 | .levelsRps=[1,4]' "$BODY" >"$PARAMS" 2>/dev/null \
  || { echo "smoke: /api/defaults was not JSON; cannot continue" >&2; exit 1; }

req GET /api/gpus
check "GET /api/gpus" "$( [[ $CODE == 200 ]] && jqt 'length > 0 and any(.[]; .id == "AMPERE_16" and .pricePerHr > 0)'; echo $?)" \
  "200, $(jq length "$BODY" 2>/dev/null) pools incl. AMPERE_16" "HTTP $CODE: $(head -c 200 "$BODY")"

req POST /api/runs/estimate "${J[@]}" --data-binary @"$PARAMS"
check "POST /api/runs/estimate" \
  "$( [[ $CODE == 200 ]] && jqt '(.configs | length) == 2 and .worstCaseUsd > 0 and .maxSpendUsd > 0 and .maxSpendUsd <= .capUsd and .capUsd == 2'; echo $?)" \
  "200, 2 configs, maxSpend \$$(jq .maxSpendUsd "$BODY" 2>/dev/null) of cap \$$(jq .capUsd "$BODY" 2>/dev/null)" \
  "HTTP $CODE: $(head -c 300 "$BODY")"

# ---- guards (all against /estimate, so a broken guard can't start a run) ----

req POST /api/runs/estimate -H 'Content-Type: text/plain' --data-binary @"$PARAMS"
check "text/plain body => 415" "$( [[ $CODE == 415 ]]; echo $?)" "415: $(detail)" "HTTP $CODE: $(detail)"

req GET /api/defaults -H 'Host: evil.example'
check "Host: evil.example => 403" "$( [[ $CODE == 403 ]] && [[ $(detail) == *"not allowed"* ]]; echo $?)" \
  "403: $(detail)" "HTTP $CODE: $(detail)"

req POST /api/runs/estimate "${J[@]}" -H 'Sec-Fetch-Site: cross-site' -H 'Origin: https://evil.example' --data-binary @"$PARAMS"
check "cross-site POST => 403" "$( [[ $CODE == 403 ]] && [[ $(detail) == *cross-origin* ]]; echo $?)" \
  "403: $(detail)" "HTTP $CODE: $(detail)"

bad_param() { # bad_param NAME JQ-EDIT FIELD-IN-DETAIL
  req POST /api/runs/estimate "${J[@]}" --data-binary "$(jq -c "$2" "$PARAMS")"
  check "$1" "$( [[ $CODE == 400 ]] && [[ $(detail) == *"$3"* ]]; echo $?)" "400: $(detail)" "HTTP $CODE: $(detail)"
}
bad_param "maxWorkers 61 => 400" '.maxWorkers=61' maxWorkers
bad_param "levelsRps [] => 400" '.levelsRps=[]' levelsRps
bad_param "budgetUsd above cap => 400" '.budgetUsd=50' budgetUsd
bad_param "coldStartMode bogus => 400" '.coldStartMode="always"' coldStartMode

req POST /api/runs/estimate "${J[@]}" --data-binary "$(jq -c '.coldStartMode="forced"' "$PARAMS")"
check "estimate coldStartMode forced" "$( [[ $CODE == 200 ]] && jqt '.maxSpendUsd > 0'; echo $?)" \
  "200, maxSpend \$$(jq .maxSpendUsd "$BODY" 2>/dev/null)" "HTTP $CODE: $(detail)"

req POST /api/runs/estimate "${J[@]}" --data-binary '{"image":'
check "malformed JSON => 400" "$( [[ $CODE == 400 ]] && [[ $(detail) == *"invalid JSON"* ]]; echo $?)" \
  "400: $(detail)" "HTTP $CODE: $(detail)"

# Valid-looking JSON, so the size cap rejects it rather than a syntax error.
BIG=$TMP/big.json
{ printf '{"image":"'; head -c 10485760 /dev/zero | tr '\0' a; printf '"}'; } >"$BIG"
t=$EPOCHREALTIME
req POST /api/runs/estimate "${J[@]}" --data-binary @"$BIG"
dt=$(awk -v a="$t" -v b="$EPOCHREALTIME" 'BEGIN{printf "%.2f", b-a}')
rm -f "$BIG"
if [[ $CODE == 4* && $(detail) == *"too large"* ]]; then
  pass "10 MB body => rejected" "$CODE in ${dt}s: $(detail)"
elif [[ $CODE == curl-exit-5[256] ]]; then
  pass "10 MB body => rejected" "connection closed by server in ${dt}s ($CODE)"
else
  fail "10 MB body => rejected" "HTTP $CODE in ${dt}s: $(detail)"
fi

# ---- cancel run (also 409 and the live SSE stream) --------------------------

echo
echo "cancel run..."
start_run
if [[ -z $RUN_ID ]]; then
  fail "start run" "HTTP $CODE: $(detail)"
else
  pass "start run" "201, id $RUN_ID"
  sse_start "$RUN_ID" "$TMP/sse-cancel.txt"

  req POST /api/runs "${J[@]}" --data-binary @"$PARAMS"
  check "second POST /api/runs => 409" "$( [[ $CODE == 409 ]]; echo $?)" "409: $(detail)" "HTTP $CODE: $(detail)"

  if wait_endpoints "$RUN_ID" 30; then
    pass "endpoints created" "both configs have an endpointId"
  else
    fail "endpoints created" "not every config had an endpointId after 30 s: $(jq -c '[.configs[].config | {status, endpointId}]' "$BODY" 2>/dev/null)"
  fi
  req POST "/api/runs/$RUN_ID/cancel"
  check "cancel => 202" "$( [[ $CODE == 202 ]]; echo $?)" "202" "HTTP $CODE: $(detail)"
  t=$SECONDS
  if wait_status "$RUN_ID" 60; then
    st=$(jq -r .run.status "$BODY")
    check "cancelled run is terminal" "$( [[ $st == cancelled ]]; echo $?)" \
      "cancelled after $((SECONDS - t)) s" "status $st (want cancelled)"
    check "cancel deleted every endpoint" \
      "$(jqt '(.configs | length) == 2 and all(.configs[]; .config.deletedAt != null)'; echo $?)" \
      "both configs have deletedAt" \
      "configs: $(jq -c '[.configs[].config | {endpointId, status, deletedAt}]' "$BODY")"
    check "cut-short configs are cancelled, not failed" \
      "$(jqt 'all(.configs[]; .config.status == "cancelled" and .config.error == "run cancelled")'; echo $?)" \
      "both configs cancelled (run cancelled)" \
      "configs: $(jq -c '[.configs[].config | {status, error}]' "$BODY")"
  else
    fail "cancelled run is terminal" "still running 60 s after cancel"
  fi
  sse_check "SSE stream (cancel run)" "$TMP/sse-cancel.txt" cancelled
  CANCEL_ID=$RUN_ID
fi

# ---- completion run ----------------------------------------------------------

DONE_ID=""
if ((QUICK == 0)); then
  echo
  echo "completion run (about 2 min)..."
  start_run
  if [[ -z $RUN_ID ]]; then
    fail "start completion run" "HTTP $CODE: $(detail)"
  else
    DONE_ID=$RUN_ID
    sse_start "$RUN_ID" "$TMP/sse-done.txt"
    t=$SECONDS
    if wait_status "$RUN_ID" 240; then
      st=$(jq -r .run.status "$BODY")
      check "run completes" "$( [[ $st == completed ]]; echo $?)" "completed in $((SECONDS - t)) s" \
        "status $st, error: $(jq -r '.run.error // ""' "$BODY")"
      # ConfigSummary has no "sent", so per config the counts must cover every job.
      check "summaries add up" \
        "$(jqt '(.configs | length) == 2 and all(.configs[];
              (.summary.completed + .summary.failed + .summary.unfinished) == (.jobs | length)
              and (.jobs | length) > 0
              and all(.summary.levels[]; .sent == .completed + .failed + .unfinished))'; echo $?)" \
        "$(jq -r '[.configs[] | "\(.config.flashboot): \(.jobs|length) jobs = \(.summary.completed)c+\(.summary.failed)f+\(.summary.unfinished)u"] | join(", ")' "$BODY")" \
        "$(jq -c '[.configs[] | {jobs: (.jobs|length), c: .summary.completed, f: .summary.failed, u: .summary.unfinished, levels: [.summary.levels[] | {sent, completed, failed, unfinished}]}]' "$BODY")"
    else
      fail "run completes" "still running after 240 s"
    fi
    sse_check "SSE stream (completion run)" "$TMP/sse-done.txt" completed

    req GET /api/runs
    check "GET /api/runs lists it" "$( [[ $CODE == 200 ]] && jq -e --arg id "$DONE_ID" 'any(.[]; .id == $id and .status == "completed")' "$BODY" >/dev/null 2>&1; echo $?)" \
      "$(jq length "$BODY") runs incl. $DONE_ID" "HTTP $CODE: $(head -c 200 "$BODY")"
  fi
fi

# ---- forced cold-start run ------------------------------------------------------

if ((QUICK == 0)); then
  echo
  echo "forced cold-start run (about 1 min)..."
  # Small on purpose: every forced sample is a first boot (about 28 s on the fake).
  req POST /api/runs "${J[@]}" --data-binary "$(jq -c '.coldStartMode="forced" | .coldStarts=1 | .warmRequests=1 | .levelsRps=[1] | .levelSeconds=5 | .drainSeconds=5' "$PARAMS")"
  if [[ $CODE != 201 ]]; then
    fail "start forced run" "HTTP $CODE: $(detail)"
  else
    FORCED_ID=$(jq -r .id "$BODY")
    t=$SECONDS
    if wait_status "$FORCED_ID" 180; then
      check "forced run completes" \
        "$(jqt '.run.status == "completed" and .run.params.coldStartMode == "forced" and all(.configs[]; any(.jobs[]; .phase == "cold" and .status == "COMPLETED"))'; echo $?)" \
        "completed in $((SECONDS - t)) s, a cold sample per config, params.coldStartMode forced" \
        "status $(jq -r .run.status "$BODY"), error: $(jq -r '.run.error // ""' "$BODY"), configs: $(jq -c '[.configs[] | {status: .config.status, error: .config.error, cold: [.jobs[] | select(.phase == "cold") | .status]}]' "$BODY")"
    else
      fail "forced run completes" "still running after 180 s"
    fi
  fi
fi

# ---- events for a finished run -----------------------------------------------

FIN_ID=${DONE_ID:-${CANCEL_ID:-}}
if [[ -n $FIN_ID ]]; then
  SSE=$TMP/sse-finished.txt
  rc=0
  curl -sN --max-time 5 "$BASE/api/runs/$FIN_ID/events" >"$SSE" 2>/dev/null || rc=$?
  want=$( [[ $FIN_ID == "$DONE_ID" ]] && echo completed || echo cancelled)
  got=$(grep '^data: ' "$SSE" | cut -c7- | jq -r .data.status 2>/dev/null || true)
  check "events of finished run" \
    "$( [[ $rc == 0 && $(grep -c '^event: run_status' "$SSE") == 1 && $(grep -c '^event: ' "$SSE") == 1 && $got == "$want" ]] && grep -qx 'id: 0' "$SSE"; echo $?)" \
    "one run_status ($got, id 0), stream closed" "curl exit $rc, stream: $(head -c 300 "$SSE")"
fi

req GET /api/runs/doesnotexist
check "GET /api/runs/doesnotexist => 404" "$( [[ $CODE == 404 ]]; echo $?)" "404: $(detail)" "HTTP $CODE: $(detail)"

# ---- SIGTERM mid-run, then restart -------------------------------------------

SIGTERM_S=""
if ((QUICK == 0)); then
  echo
  echo "SIGTERM run..."
  start_run
  if [[ -z $RUN_ID ]]; then
    fail "start SIGTERM run" "HTTP $CODE: $(detail)"
  else
    TERM_ID=$RUN_ID
    if ! wait_endpoints "$RUN_ID" 30; then
      fail "SIGTERM run endpoints created" "not every config had an endpointId after 30 s"
    fi
    t=$EPOCHREALTIME
    kill -TERM "$SERVER_PID"
    deadline=$((SECONDS + 120))
    while kill -0 "$SERVER_PID" 2>/dev/null && ((SECONDS < deadline)); do sleep 0.1; done
    SIGTERM_S=$(awk -v a="$t" -v b="$EPOCHREALTIME" 'BEGIN{printf "%.1f", b-a}')
    if kill -0 "$SERVER_PID" 2>/dev/null; then
      fail "SIGTERM exit < 120 s" "still alive after 120 s; killing"
      kill -KILL "$SERVER_PID" 2>/dev/null || true
    else
      rc=0; wait "$SERVER_PID" || rc=$?
      check "SIGTERM exit < 120 s" "$( [[ $rc == 0 ]]; echo $?)" "exited 0 in ${SIGTERM_S} s" "exit code $rc after ${SIGTERM_S} s"
    fi
    wait "$SERVER_PID" 2>/dev/null || true
    SERVER_PID=""

    start_server "$TMP/server2.log"
    req GET "/api/runs/$TERM_ID"
    st=$(jq -r '.run.status // empty' "$BODY" 2>/dev/null || true)
    check "interrupted run terminal after restart" \
      "$( [[ $CODE == 200 && -n $st && $st != running ]] && jqt 'all(.configs[]; .config.deletedAt != null)'; echo $?)" \
      "status $st, every endpoint deletedAt set" \
      "HTTP $CODE, status '$st', configs: $(jq -c '[.configs[]?.config | {endpointId, status, deletedAt}]' "$BODY" 2>/dev/null)"
    check "startup janitor ran" "$(grep -q '"msg":"janitor done"' "$TMP/server2.log"; echo $?)" \
      "log: $(grep -o '"msg":"janitor done"[^}]*' "$TMP/server2.log" | head -n 1)" \
      "no 'janitor done' line in the restart log"
  fi
fi

# ---- stock gate (COLDPROBE_FAKE_NOSTOCK) -----------------------------------------

# Each restart sets the fake's catalog stock; POST /api/runs must refuse before creating anything.
echo
echo "stock gate..."
stop_server
start_server "$TMP/server-nostock.log" AMPERE_16
req POST /api/runs "${J[@]}" --data-binary @"$PARAMS"
check "start with no-stock pool => 400" \
  "$( [[ $CODE == 400 && $(detail) == "AMPERE_16 has no stock right now" ]] && jqt '.code == "no_stock"'; echo $?)" \
  "400: $(detail)" "HTTP $CODE: $(head -c 300 "$BODY")"

stop_server
start_server "$TMP/server-lowstock.log" AMPERE_16:LOW
req POST /api/runs "${J[@]}" --data-binary @"$PARAMS"
low_code=$CODE low_detail=$(detail) low_kind=$(jq -r '.code // empty' "$BODY" 2>/dev/null || true)
req POST /api/runs "${J[@]}" --data-binary "$(jq -c '.allowLowStock=true' "$PARAMS")"
RUN_ID=""
if [[ $CODE == 201 ]]; then RUN_ID=$(jq -r .id "$BODY"); fi
check "low stock + allowLowStock => 201" \
  "$( [[ $low_code == 400 && $low_kind == low_stock \
        && $low_detail == "AMPERE_16 is low on stock; start anyway to accept the risk" && -n $RUN_ID ]]; echo $?)" \
  "400 low_stock without it ($low_detail), then 201 id $RUN_ID" \
  "without: HTTP $low_code code '$low_kind' ($low_detail); with: HTTP $CODE: $(head -c 200 "$BODY")"
if [[ -n $RUN_ID ]]; then
  # Not part of the check: ends the script promptly.
  req POST "/api/runs/$RUN_ID/cancel"
  wait_status "$RUN_ID" 60 || echo "smoke: low-stock run still running 60 s after cancel" >&2
fi

# ---- summary -------------------------------------------------------------------

total=$(awk -v a="$T0" -v b="$EPOCHREALTIME" 'BEGIN{printf "%.0f", b-a}')
npass=0 nfail=0
echo
echo "================ summary ================"
for i in "${!NAMES[@]}"; do
  printf '%-4s  %s\n' "${RESULTS[$i]}" "${NAMES[$i]}"
  if [[ ${RESULTS[$i]} == PASS ]]; then npass=$((npass + 1)); else nfail=$((nfail + 1)); fi
done
echo "-----------------------------------------"
echo "$npass passed, $nfail failed in ${total} s${SIGTERM_S:+ (SIGTERM exit ${SIGTERM_S} s)}"
if ((nfail > 0)); then
  echo "server log tail:"; tail -n 15 "$TMP"/server*.log 2>/dev/null || true
  exit 1
fi
