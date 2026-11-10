#!/usr/bin/env bash
# Copies runs, configs and jobs from coldprobe databases into an existing one.
#   scripts/merge-runs.sh <dest.db> <src.db>...
# Run ids are copied as is (a duplicate refuses the whole merge); config and job ids are
# shifted past the destination's max. Only columns both sides have are copied, for older databases.
# Stop any server on the destination first. Exits 2 on usage error or refusal, 1 on sqlite3 failure.
set -euo pipefail

if (($# < 2)); then
  echo "usage: $0 <dest.db> <src.db>..." >&2
  exit 2
fi
command -v sqlite3 >/dev/null || { echo "merge-runs: sqlite3 is required but not on PATH" >&2; exit 2; }
# A .headers or .mode line in ~/.sqliterc would change the output parsed below.
sqlite3() { command sqlite3 -batch -init /dev/null "$@"; }

# A path starting with "-" would be read as a sqlite3 option; "./" keeps it a file.
dash_safe() { if [[ $1 == -* ]]; then printf './%s' "$1"; else printf '%s' "$1"; fi; }
DEST=$(dash_safe "$1")
shift
srcs=()
for src in "$@"; do srcs+=("$(dash_safe "$src")"); done
set -- "${srcs[@]}"

# ATTACH takes no ? parameter, so paths are quoted by hand.
sql_str() { printf "'%s'" "${1//\'/\'\'}"; }

has_tables() {
  [[ -f $1 ]] && [[ $(sqlite3 "$1" "select count(*) from sqlite_master where type = 'table' and name in ('runs', 'configs', 'jobs')") == 3 ]]
}

# shared_cols SRC TABLE: TABLE's columns in both databases, in the destination's order.
shared_cols() {
  sqlite3 "$DEST" "ATTACH $(sql_str "$1") AS s;
    select group_concat(name, ', ') from (
      select name from pragma_table_info('$2', 'main')
      where name in (select name from pragma_table_info('$2', 's'))
      order by cid);"
}

# shift_cols COLS RENAMES: COLS with each "col=expr" in RENAMES substituted.
shift_cols() {
  local out=() cols col pair
  IFS=', ' read -r -a cols <<<"$1"
  for col in "${cols[@]}"; do
    for pair in $2; do
      [[ $col == "${pair%%=*}" ]] && col=${pair#*=}
    done
    out+=("$col")
  done
  local IFS=,
  echo "${out[*]}"
}

# Every check runs before anything is written, so a refusal leaves the destination untouched.
has_tables "$DEST" || { echo "merge-runs: $DEST has no coldprobe tables (create it with internal/store/schema.sql)" >&2; exit 2; }
# Checked up front too, so the final check can only fail because of the copy.
if [[ -n $(sqlite3 "$DEST" "pragma foreign_key_check") ]]; then
  echo "merge-runs: $DEST fails PRAGMA foreign_key_check; fix it before merging into it" >&2
  exit 2
fi
all_ids=$(sqlite3 "$DEST" "select id from runs")
for src in "$@"; do
  has_tables "$src" || { echo "merge-runs: $src is not a coldprobe database" >&2; exit 2; }
  # A source with dangling references would stay dangling after the copy.
  if [[ -n $(sqlite3 "$src" "pragma foreign_key_check") ]]; then
    echo "merge-runs: $src fails PRAGMA foreign_key_check; not copying it" >&2
    exit 2
  fi
  all_ids+=$'\n'$(sqlite3 "$src" "select id from runs")
done
dupes=$(grep -v '^$' <<<"$all_ids" | sort | uniq -d || true)
if [[ -n $dupes ]]; then
  echo "merge-runs: run id already present, nothing written: $(paste -sd ' ' <<<"$dupes")" >&2
  exit 2
fi

for src in "$@"; do
  cfg_off=$(sqlite3 "$DEST" "select coalesce(max(id), 0) from configs")
  job_off=$(sqlite3 "$DEST" "select coalesce(max(id), 0) from jobs")
  run_cols=$(shared_cols "$src" runs)
  cfg_cols=$(shared_cols "$src" configs)
  job_cols=$(shared_cols "$src" jobs)
  cfg_select=$(shift_cols "$cfg_cols" "id=id+$cfg_off")
  job_select=$(shift_cols "$job_cols" "id=id+$job_off config_id=config_id+$cfg_off")

  echo "== $src"
  echo "runs:"
  sqlite3 "$src" "select '  ' || id || '  ' || status from runs order by id"
  echo "configs (old -> new, offset $cfg_off):"
  sqlite3 "$src" "select '  config ' || id || ' -> ' || (id + $cfg_off) || '  ' || run_id || ' ' || pool || ' ' || flashboot from configs order by id"
  echo "jobs: $(sqlite3 "$src" "select count(*) || ' rows, ids ' || coalesce(min(id), '-') || '..' || coalesce(max(id), '-') || ' -> ' || coalesce(min(id) + $job_off, '-') || '..' || coalesce(max(id) + $job_off, '-') from jobs") (offset $job_off)"

  # -bail exits at the first error, rolling back the open transaction: no partial rows.
  sqlite3 -bail "$DEST" <<SQL
ATTACH $(sql_str "$src") AS s;
BEGIN IMMEDIATE;
INSERT INTO main.runs ($run_cols) SELECT $run_cols FROM s.runs;
INSERT INTO main.configs ($cfg_cols) SELECT $cfg_select FROM s.configs;
INSERT INTO main.jobs ($job_cols) SELECT $job_select FROM s.jobs;
COMMIT;
SQL
  echo "copied: $(sqlite3 "$src" "select (select count(*) from runs) || ' runs, ' || (select count(*) from configs) || ' configs, ' || (select count(*) from jobs) || ' jobs'")"
done

echo "== $DEST now has: $(sqlite3 "$DEST" "select (select count(*) from runs) || ' runs, ' || (select count(*) from configs) || ' configs, ' || (select count(*) from jobs) || ' jobs'")"
fk=$(sqlite3 "$DEST" "pragma foreign_key_check")
if [[ -n $fk ]]; then
  echo "merge-runs: PRAGMA foreign_key_check on $DEST is not empty:" >&2
  echo "$fk" >&2
  exit 1
fi
echo "PRAGMA foreign_key_check: empty"
