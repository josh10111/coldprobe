#!/usr/bin/env bash
# Day-1 checks: verify unknowns in the real Runpod REST v2 API before the Go code depends on them.
#
#   ./scripts/day1-checks.sh                                  read-only: catalog, endpoint list, rate-limit headers ($0)
#   ./scripts/day1-checks.sh --probe <poolId> [OFF|FLASHBOOT] [--yes]
#                                                             creates ONE temporary endpoint (billable), measures, deletes it
#
# Env: RUNPOD_API_KEY (loaded from the repo's .env if unset), COLDPROBE_WORKER_IMAGE,
#      RUNPOD_MGMT_URL / RUNPOD_JOB_URL (override only to point at a local mock).
#
# Never add `set -x`: it would print the key when the auth header file is written.
set -euo pipefail
{ set +x; } 2>/dev/null # even `bash -x day1-checks.sh` must not trace lines that expand the key
export LC_ALL=C # EPOCHREALTIME must use "." as the decimal point for the jq maths
umask 077 # temp files, including the auth header, are owner-only

MGMT_URL=${RUNPOD_MGMT_URL:-https://api.runpod.io}
JOB_URL=${RUNPOD_JOB_URL:-https://api.runpod.ai/v2}
HARD_TIMEOUT_S=1500 # 25 min: the whole probe, and the basis of the upper-bound cost
# Teardown after the deadline: lookup (3 x 30 s + 5 s) plus delete (3 x 30 s + 2 s); it may bill too.
TEARDOWN_SLACK_S=150
NAME_PREFIX=bench-coldprobe-

# jq filter applied to every JSON we print: drop keys that could identify the account.
SCRUB='walk(if type == "object" then with_entries(select(.key | test("(account|user|org).*id|email"; "i") | not)) else . end)'

die() { echo "error: $*" >&2; exit 1; }
banner() { # banner <line>...: impossible-to-miss warning on stderr
	local line
	{
		echo
		echo "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!"
		for line in "$@"; do echo "!!! $line"; done
		echo "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!"
	} >&2
}
log() { echo "[$(date -u +%H:%M:%S)] $*"; }

# --- key loading -------------------------------------------------------------------------
if [[ -z ${RUNPOD_API_KEY:-} ]]; then
	env_file="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/.env"
	if [[ -f $env_file ]]; then
		set -a
		# shellcheck disable=SC1090 # the path is computed; the file is gitignored and never printed
		. "$env_file"
		set +a
	fi
fi
[[ -n ${RUNPOD_API_KEY:-} ]] || die "RUNPOD_API_KEY is not set (export it, or put it in the repo's .env)"
IMAGE=${COLDPROBE_WORKER_IMAGE:-ghcr.io/josh10111/coldprobe-worker:latest}

# The key goes into a 600 file read by `curl -H @file`, never into argv, where `ps` would show it.
# printf is a builtin, so writing the file doesn't expose it either.
TMP=$(mktemp -d)
AUTH="$TMP/auth"
BODY="$TMP/body"
HEADERS="$TMP/headers"
{ set +x; } 2>/dev/null # in case someone adds set -x above
printf 'Authorization: Bearer %s\n' "$RUNPOD_API_KEY" >"$AUTH"
chmod 600 "$AUTH"
unset RUNPOD_API_KEY # children (curl, jq) don't need it

# --- teardown ----------------------------------------------------------------------------
# Set before any endpoint can exist: the trap reads only these two, wherever the script dies.
ENDPOINT_ID=""
CREATE_NAME="" # set just before the create POST, so a lost response can still be cleaned up

delete_endpoint() { # delete_endpoint <id>: up to 3 tries; 404 counts as already gone
	local id=$1 try
	for try in 1 2 3; do
		if api DELETE "$MGMT_URL/v2/serverless/$id" || [[ ${HTTP_CODE:-} == 404 ]]; then
			log "deleted endpoint $id" >&2
			return 0
		fi
		log "delete attempt $try for $id failed (HTTP ${HTTP_CODE:-none})" >&2
		sleep 2
	done
	return 1
}

# teardown: delete our endpoint if it (may) exist; returns 1 with a banner if unsure it's gone.
# Logs go to stderr: with `| tee`, stdout may be a dead pipe by now.
teardown() {
	local rc=0 try
	DEADLINE=0 # deletes must still run after the hard timeout
	if [[ -z $ENDPOINT_ID && -n $CREATE_NAME ]]; then
		# Create was sent but no id read; it may appear only after a delay, so retry the name lookup.
		for try in 1 2 3; do
			if api GET "$MGMT_URL/v2/serverless"; then
				ENDPOINT_ID=$(jq -r --arg n "$CREATE_NAME" '[.endpoints[]? | select(.name == $n) | .id][0] // empty' "$BODY")
				[[ -n $ENDPOINT_ID ]] && break
				log "lookup $try: no endpoint named $CREATE_NAME listed yet" >&2
			else
				log "lookup $try: endpoint list failed (HTTP ${HTTP_CODE:-none})" >&2
			fi
			((try < 3)) && sleep 5
		done
		if [[ -z $ENDPOINT_ID ]]; then
			banner "COULD NOT CONFIRM THAT ENDPOINT $CREATE_NAME WAS NOT CREATED." \
				"Check the Runpod console (or run the coldprobe janitor) for that name NOW."
			rc=1
		fi
	fi
	if [[ -n $ENDPOINT_ID ]] && ! delete_endpoint "$ENDPOINT_ID"; then
		banner "FAILED TO DELETE ENDPOINT $ENDPOINT_ID ($CREATE_NAME)" \
			"IT MAY STILL BE BILLING. Delete it in the Runpod console NOW."
		rc=1
	fi
	ENDPOINT_ID="" CREATE_NAME=""
	return "$rc"
}

cleanup() {
	# A second Ctrl-C, HUP or dead `| tee` must not cut the deletes short; curl inherits SIG_IGN too.
	trap '' INT TERM HUP PIPE
	set +e # a failed curl in here must not stop the retries or the warning
	teardown
	rm -rf "$TMP" # last: the deletes need the auth file
}
trap cleanup EXIT
# Exit, not cleanup, so cleanup runs once via EXIT; untrapped HUP and PIPE would skip EXIT.
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP
trap 'exit 141' PIPE

# --- HTTP --------------------------------------------------------------------------------
DEADLINE=0 # SECONDS value after which api() refuses; 0 = no deadline

# api METHOD URL [curl args...]: body -> $BODY, status -> $HTTP_CODE; 1 on curl error or HTTP >= 400.
# API_MAX_TIME (default 30 s) caps the request, clipped to the deadline.
api() {
	local method=$1 url=$2 max=${API_MAX_TIME:-30}
	shift 2
	HTTP_CODE=""
	if ((DEADLINE > 0)); then
		((DEADLINE - SECONDS < max)) && max=$((DEADLINE - SECONDS))
		((max > 0)) || { echo "hard timeout reached, not calling $method $url" >&2; return 1; }
	fi
	: >"$BODY"
	HTTP_CODE=$(curl -sS -X "$method" -H @"$AUTH" -o "$BODY" -w '%{http_code}' \
		--max-time "$max" "$@" "$url") || { echo "curl failed: $method $url" >&2; return 1; }
	if ((HTTP_CODE >= 400)); then
		echo "HTTP $HTTP_CODE from $method $url" >&2
		jq "$SCRUB" "$BODY" >&2 2>/dev/null || { head -c 500 "$BODY" | redact >&2; echo >&2; }
		return 1
	fi
}

show() { jq "$SCRUB" "$BODY"; }

# redact: for non-JSON text that SCRUB can't walk, mask bearer tokens, rpa_ keys and emails.
redact() {
	sed -E 's/Bearer[[:space:]]+[^[:space:]"]+/Bearer [REDACTED]/gI; s/rpa_[[:alnum:]_-]+/[REDACTED]/g;
		s/[[:alnum:]._%+-]+@[[:alnum:].-]+\.[[:alpha:]]+/[REDACTED-EMAIL]/g'
}

# Polls stop early near the hard timeout so the run still reaches delete-and-confirm.
time_left_ok() { ((DEADLINE == 0 || DEADLINE - SECONDS > 45)); }

# --- read-only mode ----------------------------------------------------------------------
read_only() {
	log "GET /v2/catalog/gpus?include=AVAILABILITY&product=SERVERLESS"
	if ! api GET "$MGMT_URL/v2/catalog/gpus?include=AVAILABILITY&product=SERVERLESS" -D "$HEADERS"; then
		log "availability params rejected (HTTP ${HTTP_CODE:-none}); FALLING BACK to GET /v2/catalog/gpus with no params"
		api GET "$MGMT_URL/v2/catalog/gpus" -D "$HEADERS" || die "catalog request failed"
	fi
	echo
	echo "Serverless GPUs (price.serverless is USD per hour per GPU):"
	jq -r "$SCRUB"' | ["POOL", "GPU_ID", "MEM_GB", "USD_PER_HR", "AVAILABILITY", "DATA_CENTERS"],
		(.gpus | map(select(.pool != null)) | sort_by(.pool, .id)[]
		 | [.pool, .id, .memory, (.price.serverless // "-"), (.availability // "-"),
		    ((.dataCenters // []) | map("\(.id):\(.availability)") | join(",") | if . == "" then "-" else . end)])
		| @tsv' "$BODY" | column -t -s $'\t'
	jq -r '"(\([.gpus[] | select(.pool == null)] | length) GPU types with no serverless pool not shown)"' "$BODY"

	echo
	echo "Rate-limit headers from the catalog call:"
	grep -iE '^(ratelimit|retry-after)' "$HEADERS" | tr -d '\r' || echo "  (none)"

	echo
	log "GET /v2/serverless"
	api GET "$MGMT_URL/v2/serverless" || die "endpoint list failed"
	echo "Existing endpoints (id, name):"
	jq -r "$SCRUB"' | .endpoints[] | [.id, .name] | @tsv' "$BODY" | column -t -s $'\t'
	jq -r 'if (.endpoints | length) == 0 then "  (none)" else empty end' "$BODY"
}

# --- probe mode --------------------------------------------------------------------------
# runsync_job <label>: one job via runsync, then poll /status to a terminal state (cap 10 min).
runsync_job() {
	local label=$1 start=$EPOCHREALTIME t0=$SECONDS job_id status
	# A longer wait with no worker placed got a dropped (0-byte) response instead of IN_QUEUE.
	log "($label) POST runsync?wait=60000 {\"input\":{\"sleep_ms\":1000}}"
	API_MAX_TIME=70 api POST "$JOB_URL/$ENDPOINT_ID/runsync?wait=60000" \
		-H 'Content-Type: application/json' --data-binary '{"input":{"sleep_ms":1000}}' ||
		die "($label) runsync failed"
	show
	status=$(jq -r '.status // empty' "$BODY")
	job_id=$(jq -r '.id // empty' "$BODY")
	while [[ $status == IN_QUEUE || $status == IN_PROGRESS ]]; do
		# A worker that can't pull the image never picks the job up.
		((SECONDS - t0 < 600)) ||
			die "($label) job still $status after 10 min; the worker probably can't start. Check that $IMAGE is public on GHCR and pullable"
		sleep 2
		api GET "$JOB_URL/$ENDPOINT_ID/status/$job_id" || die "($label) status poll failed"
		status=$(jq -r '.status // empty' "$BODY")
		log "($label) status: $status"
		show
	done
	log "($label) wall time to terminal status ($status): $(jq -n "$EPOCHREALTIME - $start | . * 1000 | round / 1000") s"
}

# Billing workers, as runpod.WorkerSummary.Active: a stopped worker stays listed as IDLE with null
# uptimeSeconds, so summary.total never reaches 0. No workers array gives "unknown", never 0.
ACTIVE='if (.workers | type) != "array" then "unknown" else
	[.workers[] | select(.status == "RUNNING" or .status == "INITIALIZING"
		or (.status == "IDLE" and .uptimeSeconds != null))] | length end'

wait_scaled_to_zero() {
	local start=$SECONDS active
	log "(b) polling /v2/serverless/$ENDPOINT_ID/workers every 2 s until no active workers (cap 300 s)"
	while ((SECONDS - start < 300)) && time_left_ok; do
		api GET "$MGMT_URL/v2/serverless/$ENDPOINT_ID/workers" || die "(b) workers poll failed"
		active=$(jq -r "$ACTIVE" "$BODY")
		log "(b) +$((SECONDS - start))s active: $active, summary: $(jq -c "$SCRUB | .summary" "$BODY")"
		if [[ $active == 0 ]]; then
			log "(b) scaled to zero after $((SECONDS - start)) s. Full response at zero:"
			show
			return 0
		fi
		sleep 2
	done
	log "(b) WARNING: still ${active:-?} active worker(s) at the cap/deadline; (c) may NOT be a cold start"
}

# wait_for_metrics <window start> <job 1 done (SECONDS)> <job 2 done (SECONDS)>
# Only job 2 gives a real lag; the first poll comes after (b) and (c), so job 1's is an upper bound.
wait_for_metrics() {
	local window_start=$1 start=$SECONDS kind n t
	local -A done_at=([1]=$2 [2]=$3) seen=() # seen[<kind><job>] = 1 once logged
	local -A prev_poll=([cold-starts]=$3 [requests]=$3) # job 2 can't show before it finished
	local -A count_expr=([cold-starts]='[.records[].count] | add // 0' [requests]='[.records[].completed] | add // 0')
	log "(d) polling metrics/cold-starts and metrics/requests (interval=1m, startTime=$window_start) every 30 s (cap 15 min)"
	while ((SECONDS - start < 900)) && time_left_ok; do
		for kind in cold-starts requests; do
			[[ -n ${seen[${kind}2]:-} ]] && continue
			# No endTime: an explicit (exclusive) one can cut the current bucket off.
			t=$SECONDS
			api GET "$MGMT_URL/v2/serverless/$ENDPOINT_ID/metrics/$kind" -G \
				--data-urlencode "startTime=$window_start" \
				--data-urlencode "interval=1m" || die "(d) metrics/$kind failed"
			n=$(jq "${count_expr[$kind]}" "$BODY")
			log "(d) +$((t - start))s metrics/$kind counts $n job(s)"
			if ((n >= 1)) && [[ -z ${seen[${kind}1]:-} ]]; then
				seen[${kind}1]=1
				log "(d) metrics/$kind shows job 1: <= $((t - done_at[1])) s after it finished (upper bound only)"
			fi
			if ((n >= 2)) && [[ -z ${seen[${kind}2]:-} ]]; then
				seen[${kind}2]=1
				log "(d) metrics/$kind shows job 2: between $((prev_poll[$kind] - done_at[2])) and $((t - done_at[2])) s after it finished"
			fi
			prev_poll[$kind]=$t
			[[ -n ${seen[${kind}2]:-} ]] && show
		done
		[[ -n ${seen[cold-starts2]:-} && -n ${seen[requests2]:-} ]] && return 0
		sleep 30
	done
	log "(d) WARNING: metrics incomplete at the cap/deadline (cold-starts saw job 2: ${seen[cold-starts2]:-no}, requests saw job 2: ${seen[requests2]:-no}). Last response:"
	show
}

probe() {
	local pool=$1 flashboot=$2 yes=$3 price bound name body window_start job1_done job2_done
	[[ $flashboot == OFF || $flashboot == FLASHBOOT ]] || die "flashboot must be OFF or FLASHBOOT, got '$flashboot'"

	api GET "$MGMT_URL/v2/catalog/gpus" || die "catalog request failed"
	# Refuse if ANY GPU in the pool lacks a price: the scheduler may pick that card.
	price=$(jq -r --arg p "$pool" '[.gpus[] | select(.pool == $p)]
		| if length == 0 or any(.price.serverless == null) then empty else map(.price.serverless) | max end' "$BODY")
	[[ -n $price ]] || die "pool '$pool' not in the catalog, or a GPU in it has no price.serverless; refusing to run"
	bound=$(jq -n --argjson p "$price" --argjson s "$((HARD_TIMEOUT_S + TEARDOWN_SLACK_S))" '$p * $s / 3600 | . * 10000 | ceil / 10000')
	# A run ID's shape (UTC stamp) so coldprobe's janitor leaves it alone for an hour;
	# any unparseable name would be deleted.
	name="${NAME_PREFIX}$(date -u +%Y%m%d-%H%M%S)-$(printf '%04x' "$RANDOM")-probe"

	cat <<-EOF

		PROBE PLAN (billable)
		  endpoint   $name
		  pool       $pool   (max price.serverless in pool: \$$price/hr per GPU)
		  flashboot  $flashboot
		  image      $IMAGE   env MODEL_LOAD_SECONDS=5
		  workers    min 0, max 1, idleTimeout 5 s; QUEUE, QUEUE_DELAY 4; timeout 60000 ms
		  steps      (a) runsync  (b) wait for scale-to-zero  (c) cold runsync  (d) wait for metrics
		             then delete the endpoint and confirm no ${NAME_PREFIX}* endpoints remain
		  hard cap   ${HARD_TIMEOUT_S} s total
		  UPPER-BOUND COST: (${HARD_TIMEOUT_S} s run + ${TEARDOWN_SLACK_S} s teardown slack) x 1 worker x \$$price/hr / 3600 = \$$bound
		  (list price; account discounts not reflected)

	EOF

	# Fail safe: with no TTY nobody can type "yes", so only an explicit --yes proceeds.
	if [[ $yes != 1 ]]; then
		[[ -t 0 ]] || die "stdin is not a terminal; re-run with --yes to approve the plan above"
		local answer
		read -r -p "Type yes to create the endpoint: " answer
		[[ $answer == yes ]] || die "not confirmed; nothing created"
	fi

	DEADLINE=$((SECONDS + HARD_TIMEOUT_S))
	# Floored to the minute: an unaligned start could drop the 1m bucket holding job 1.
	window_start=$(date -u +%Y-%m-%dT%H:%M:00Z)
	body=$(jq -n --arg name "$name" --arg image "$IMAGE" --arg pool "$pool" --arg fb "$flashboot" '{
		name: $name, type: "QUEUE", scaling: {type: "QUEUE_DELAY", queueDelay: 4},
		image: $image, env: {MODEL_LOAD_SECONDS: "5"}, gpu: {pools: [$pool]},
		workers: {min: 0, max: 1, idleTimeout: 5}, timeout: 60000, flashboot: $fb}')

	log "POST /v2/serverless (create $name)"
	CREATE_NAME=$name # cleanup can find it by name even if the id is never read
	if ! API_MAX_TIME=90 api POST "$MGMT_URL/v2/serverless" -H 'Content-Type: application/json' --data-binary "$body"; then
		# A 4xx means "not created", except 408 and 429, which a proxy can send while the create
		# carries on; those, timeouts and 5xx keep CREATE_NAME so teardown searches for it.
		[[ $HTTP_CODE == 4?? && $HTTP_CODE != 408 && $HTTP_CODE != 429 ]] && CREATE_NAME=""
		die "create failed (HTTP ${HTTP_CODE:-none}); if the pool is out of stock, the error above is that answer"
	fi
	ENDPOINT_ID=$(jq -r '.id // empty' "$BODY")
	[[ -n $ENDPOINT_ID ]] || die "create returned no id"
	log "created $ENDPOINT_ID (HTTP $HTTP_CODE):"
	show

	runsync_job a
	job1_done=$SECONDS
	wait_scaled_to_zero
	runsync_job "c, cold"
	job2_done=$SECONDS
	wait_for_metrics "$window_start" "$job1_done" "$job2_done"

	log "deleting $ENDPOINT_ID"
	teardown || exit 1

	DEADLINE=0
	api GET "$MGMT_URL/v2/serverless" || die "could not list endpoints to confirm the delete"
	local left
	left=$(jq -r --arg p "$NAME_PREFIX" '[.endpoints[] | select(.name | startswith($p)) | "\(.id) \(.name)"] | join("\n")' "$BODY")
	if [[ -n $left ]]; then
		printf '!!! WARNING: %s endpoints still listed:\n%s\n' "$NAME_PREFIX" "$left" >&2
		exit 1
	fi
	log "confirmed: no ${NAME_PREFIX}* endpoints remain"
}

# --- main --------------------------------------------------------------------------------
usage() { sed -n '4,6p' "$0" >&2; exit 2; }

case ${1:-} in
"") read_only ;;
--probe)
	shift
	pool=${1:-}
	[[ -n $pool && $pool != --* ]] || usage
	shift
	flashboot=OFF yes=0
	for arg in "$@"; do
		case $arg in
		--yes) yes=1 ;;
		OFF | FLASHBOOT) flashboot=$arg ;;
		*) usage ;;
		esac
	done
	probe "$pool" "$flashboot" "$yes"
	;;
*) usage ;;
esac
