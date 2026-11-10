#!/usr/bin/env bash
# Tests scripts/merge-runs.sh. Needs sqlite3; run from anywhere.
set -euo pipefail

command -v sqlite3 >/dev/null || { echo "merge-runs_test: sqlite3 is required but not on PATH" >&2; exit 2; }

REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
MERGE=$REPO/scripts/merge-runs.sh
TMP=$(mktemp -d "${TMPDIR:-/tmp}/coldprobe-merge-test.XXXXXX")
trap 'rm -rf "$TMP"' EXIT

FAILS=0
check() { # check NAME WANT GOT
  if [[ $2 == "$3" ]]; then
    echo "PASS $1"
  else
    echo "FAIL $1: want '$2', got '$3'"
    FAILS=$((FAILS + 1))
  fi
}
q() { sqlite3 "$1" "$2"; }

# A: one run, two configs (ids 1, 2), three jobs.
A=$TMP/a.db
sqlite3 "$A" < "$REPO/internal/store/schema.sql"
sqlite3 "$A" <<'SQL'
INSERT INTO runs (id, created_at, status) VALUES ('20261101-000000-aaaa', '2026-11-01T00:00:00.000Z', 'completed');
INSERT INTO configs (id, run_id, pool, flashboot, status, created_at) VALUES
  (1, '20261101-000000-aaaa', 'AMPERE_16', 'OFF', 'completed', '2026-11-01T00:00:00.000Z'),
  (2, '20261101-000000-aaaa', 'AMPERE_16', 'FLASHBOOT', 'completed', '2026-11-01T00:00:00.000Z');
INSERT INTO jobs (id, config_id, phase, status, intended_at, sent_at, tflops) VALUES
  (1, 1, 'first_boot', 'COMPLETED', 't', 't', 50.5),
  (2, 1, 'warm', 'COMPLETED', 't', 't', NULL),
  (3, 2, 'warm', 'COMPLETED', 't', 't', NULL);
SQL

# B: an older database (no matmul columns) whose config and job ids overlap A's.
B=$TMP/b.db
sqlite3 "$B" < "$REPO/internal/store/schema.sql"
sqlite3 "$B" <<'SQL'
ALTER TABLE jobs DROP COLUMN matmul_ms;
ALTER TABLE jobs DROP COLUMN tflops;
ALTER TABLE jobs DROP COLUMN cuda_init_ms;
INSERT INTO runs (id, created_at, status) VALUES
  ('20261102-000000-bbbb', '2026-11-02T00:00:00.000Z', 'completed'),
  ('20261103-000000-cccc', '2026-11-03T00:00:00.000Z', 'failed');
INSERT INTO configs (id, run_id, pool, flashboot, status, created_at) VALUES
  (1, '20261102-000000-bbbb', 'ADA_24', 'OFF', 'completed', '2026-11-02T00:00:00.000Z'),
  (2, '20261103-000000-cccc', 'AMPERE_48', 'OFF', 'failed', '2026-11-03T00:00:00.000Z');
INSERT INTO jobs (id, config_id, phase, status, intended_at, sent_at) VALUES
  (1, 1, 'first_boot', 'COMPLETED', 't', 't'),
  (2, 1, 'cold', 'COMPLETED', 't', 't'),
  (3, 2, 'first_boot', 'FAILED', 't', 't'),
  (4, 2, 'warm', 'FAILED', 't', 't');
SQL

echo "--- merging b.db into a.db"
set +e
OUT=$("$MERGE" "$A" "$B" 2>&1)
RC=$?
set -e
echo "$OUT"
check "merge exits 0" 0 "$RC"
check "runs = 1 + 2" 3 "$(q "$A" 'select count(*) from runs')"
check "configs = 2 + 2" 4 "$(q "$A" 'select count(*) from configs')"
check "jobs = 3 + 4" 7 "$(q "$A" 'select count(*) from jobs')"
check "no job points at a missing config" 0 \
  "$(q "$A" 'select count(*) from jobs where config_id not in (select id from configs)')"
check "no config points at a missing run" 0 \
  "$(q "$A" 'select count(*) from configs where run_id not in (select id from runs)')"
check "foreign_key_check is empty" "" "$(q "$A" 'pragma foreign_key_check')"
# Renumbered by A's max: configs 1,2 -> 3,4; jobs 1..4 -> 4..7.
check "B's config 1 is now 3" "3|20261102-000000-bbbb|ADA_24" \
  "$(q "$A" "select id, run_id, pool from configs where pool = 'ADA_24'")"
check "B's jobs follow their config" "AMPERE_48|2" \
  "$(q "$A" "select c.pool, count(*) from jobs j join configs c on c.id = j.config_id where j.status = 'FAILED' group by c.pool")"
check "B's jobs renumbered from 4" "4|7" "$(q "$A" "select min(id), max(id) from jobs where config_id in (3, 4)")"
check "A's own rows untouched" "1|1|50.5" "$(q "$A" "select id, config_id, tflops from jobs where id = 1")"
check "columns B lacks are NULL" 0 "$(q "$A" "select count(tflops) from jobs where config_id in (3, 4)")"
check "output prints the config mapping" yes "$(grep -q 'config 1 -> 3' <<<"$OUT" && echo yes || echo no)"

echo "--- merging b.db again (run ids collide)"
set +e
OUT=$("$MERGE" "$A" "$B" 2>&1)
RC=$?
set -e
echo "$OUT"
check "collision exits 2" 2 "$RC"
check "collision names the problem" yes "$(grep -q 'run id already present' <<<"$OUT" && echo yes || echo no)"
check "nothing written on collision" "3|4|7" \
  "$(q "$A" 'select (select count(*) from runs), (select count(*) from configs), (select count(*) from jobs)')"

# mkdb PATH RUNID: a fresh database with one run, one config and one job.
mkdb() {
  sqlite3 "$1" < "$REPO/internal/store/schema.sql"
  sqlite3 "$1" "INSERT INTO runs (id, created_at, status) VALUES ('$2', 't', 'completed');
    INSERT INTO configs (id, run_id, pool, flashboot, status, created_at) VALUES (1, '$2', 'ADA_24', 'OFF', 'completed', 't');
    INSERT INTO jobs (id, config_id, phase, status, intended_at, sent_at) VALUES (1, 1, 'warm', 'COMPLETED', 't', 't');"
}

echo "--- every sqlite3 call skips ~/.sqliterc"
# sqlite3 finds ~/.sqliterc via passwd, not $HOME, so a PATH shim logs each call's arguments instead.
mkdir -p "$TMP/bin"
printf '#!/usr/bin/env bash\nprintf "%%s %%s %%s\\n" "$1" "$2" "$3" >>"%s"\nexec "%s" "$@"\n' "$TMP/calls.log" "$(command -v sqlite3)" >"$TMP/bin/sqlite3"
chmod +x "$TMP/bin/sqlite3"
mkdb "$TMP/rc-dest.db" 20261104-000000-dddd
mkdb "$TMP/rc-src.db" 20261105-000000-eeee
set +e
OUT=$(PATH=$TMP/bin:$PATH "$MERGE" "$TMP/rc-dest.db" "$TMP/rc-src.db" 2>&1)
RC=$?
set -e
check "isolated: merge exits 0" 0 "$RC"
check "isolated: the shim saw the calls" yes "$([[ -s $TMP/calls.log ]] && echo yes || echo no)"
check "isolated: no call without -batch -init /dev/null" 0 "$(grep -vcx -- '-batch -init /dev/null' "$TMP/calls.log" || true)"

echo "--- paths starting with \"-\""
mkdb "$TMP/-dest.db" 20261106-000000-ffff
mkdb "$TMP/-src.db" 20261107-000000-gggg
set +e
OUT=$(cd "$TMP" && "$MERGE" -dest.db -src.db 2>&1)
RC=$?
set -e
echo "$OUT"
check "dash paths: merge exits 0" 0 "$RC"
check "dash paths: both runs in the destination" 2 "$(q "$TMP/-dest.db" 'select count(*) from runs')"

echo "--- a destination with a dangling reference is refused up front"
mkdb "$TMP/bad-dest.db" 20261108-000000-hhhh
q "$TMP/bad-dest.db" "INSERT INTO jobs (id, config_id, phase, status, intended_at, sent_at) VALUES (2, 99, 'warm', 'COMPLETED', 't', 't')"
mkdb "$TMP/ok-src.db" 20261109-000000-iiii
set +e
OUT=$("$MERGE" "$TMP/bad-dest.db" "$TMP/ok-src.db" 2>&1)
RC=$?
set -e
echo "$OUT"
check "bad destination exits 2" 2 "$RC"
check "bad destination names the check" yes "$(grep -q 'fix it before merging' <<<"$OUT" && echo yes || echo no)"
check "nothing written to a bad destination" 1 "$(q "$TMP/bad-dest.db" 'select count(*) from runs')"

echo
if ((FAILS > 0)); then
  echo "$FAILS check(s) failed"
  exit 1
fi
echo "all checks passed"
