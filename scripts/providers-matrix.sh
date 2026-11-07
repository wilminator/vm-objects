#!/usr/bin/env bash
# Copyright (c) 2026 Michael Allen Wilmes
#
# This Source Code Form is subject to the terms of the Mozilla Public
# License, v. 2.0. If a copy of the MPL was not distributed with this
# file, You can obtain one at https://mozilla.org/MPL/2.0/.
#
# Tests vmobj against several S3 services at once, and copies between them.
# Run with --help.
set -uo pipefail

usage() {
  cat <<'EOF2'
Usage: scripts/providers-matrix.sh [--vmobj PATH] [--no-lock NAME ...] NAME=ADDRESS NAME=ADDRESS ...

Each NAME=ADDRESS is a service and an s3:// address of a bucket (or of a
prefix in one) to test in, for example

  aws='s3://my-vmobj/test?region=us-west-2&profile=vmobj-my-vmobj'
  b2='s3://my-vmobj-b2/test?endpoint=https://s3.us-west-004.backblazeb2.com&region=us-west-004&profile=vmobj-my-vmobj-b2'
  quobjects='s3://vmobj/test?endpoint=https://nas2:8010&region=us-east-1&profile=quobjects'

Everything goes under a new prefix per run (RUN = the start time), so the
buckets can be the ones that hold your real backups: real backups live
under another prefix and are never touched. Backups use the "test"
profile (Standard, locked one day), so a run's objects can be pruned two
days later.

For each service:
  1. scripts/e2e.sh: full, incremental and differential backups, deep
     verify, byte-exact restores, prune, copy to a local folder, wrong key
  2. the same with Object Lock (skipped for --no-lock services)
Then, for every ordered pair of services (A to B):
  3. a source repository on A (a full and an incremental), copied to B with
     vmobj copy, restored from B and compared, and a second copy that must
     send nothing

Prints a table of results at the end; exit status 0 when all passed.
One key file is made for the run; keep it if you want to look around
afterwards (--keep).

Options:
  --vmobj PATH     the vmobj binary (default: vmobj on PATH)
  --no-lock NAME   that service has no Object Lock (repeatable)
  --only-pairs     skip steps 1 and 2
  --keep           keep the run's key and scratch folder
  -h, --help       this text
EOF2
}

vmobj=vmobj nolock=() pairs_only=false keep=false names=() addrs=()
while [ $# -gt 0 ]; do
  case $1 in
    --vmobj) vmobj=$2; shift 2 ;;
    --no-lock) nolock+=("$2"); shift 2 ;;
    --only-pairs) pairs_only=true; shift ;;
    --keep) keep=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *=*) names+=("${1%%=*}"); addrs+=("${1#*=}"); shift ;;
    *) echo "unknown argument: $1 (see --help)" >&2; exit 2 ;;
  esac
done
[ ${#names[@]} -ge 1 ] || { usage >&2; exit 2; }
command -v "$vmobj" >/dev/null || { echo "vmobj not found: $vmobj" >&2; exit 2; }
vmobj=$(command -v "$vmobj")
here=$(cd "$(dirname "$0")" && pwd)
run=$(date -u +%Y%m%dT%H%M%SZ)
work=$(mktemp -d)
$keep || trap 'rm -rf "$work"' EXIT
results=()
record() { results+=("$(printf '%-6s %-28s %s' "$1" "$2" "$3")"); }
step() { printf '\n######## %s\n' "$*"; }
locks() { local n; for n in "${nolock[@]}"; do [ "$n" = "$1" ] && return 1; done; return 0; }

# at ADDRESS SUFFIX: the address with /SUFFIX added to the bucket path,
# keeping the ?query.
at() { local base=${1%%\?*} q=""; [[ $1 == *\?* ]] && q="?${1#*\?}"; printf '%s/%s%s' "${base%/}" "$2" "$q"; }

key="$work/matrix.key"
"$vmobj" key init --key-file "$key" >/dev/null
echo "run $run; scratch $work"

if ! $pairs_only; then
  for i in "${!names[@]}"; do
    n=${names[$i]} a=${addrs[$i]}
    step "$n: end to end"
    if E2E_BACKUP_ARGS="--profile test" "$here/e2e.sh" "$(at "$a" "$run/e2e")" "$vmobj" > "$work/$n-e2e.log" 2>&1; then
      record PASS "$n e2e" "$(at "$a" "$run/e2e")"
    else
      record FAIL "$n e2e" "see $work/$n-e2e.log"; tail -n 20 "$work/$n-e2e.log"
    fi
    if locks "$n"; then
      step "$n: end to end with Object Lock"
      if E2E_BACKUP_ARGS="--profile test" E2E_INIT_ARGS="--object-lock" "$here/e2e.sh" "$(at "$a" "$run/e2e-locked")" "$vmobj" > "$work/$n-locked.log" 2>&1; then
        record PASS "$n e2e, Object Lock" "$(at "$a" "$run/e2e-locked")"
      else
        record FAIL "$n e2e, Object Lock" "see $work/$n-locked.log"; tail -n 20 "$work/$n-locked.log"
      fi
    else
      record SKIP "$n e2e, Object Lock" "no Object Lock"
    fi
  done
fi

# ---- copies between services -------------------------------------------------

disk="$work/disk.raw"
truncate -s 24M "$disk"
dd if=/dev/urandom of="$disk" bs=1M count=16 conv=notrunc status=none
cp "$disk" "$work/want-full.raw"
src_ok=()
for i in "${!names[@]}"; do
  n=${names[$i]} src=$(at "${addrs[$i]}" "$run/src")
  step "$n: source repository"
  args=(); locks "$n" && args=(--object-lock)
  if "$vmobj" init --repo "$src" --key-file "$key" "${args[@]}" >/dev/null 2>"$work/$n-src.log" \
     && cp "$work/want-full.raw" "$disk" \
     && "$vmobj" backup --repo "$src" --key-file "$key" --source "$disk" --vm matrix --type full --state-dir "$work/state-$n" --profile test --progress none >>"$work/$n-src.log" 2>&1 \
     && dd if=/dev/urandom of="$disk" bs=1M seek=20 count=2 conv=notrunc status=none \
     && cp "$disk" "$work/want-inc-$n.raw" \
     && "$vmobj" backup --repo "$src" --key-file "$key" --source "$disk" --vm matrix --type incremental --state-dir "$work/state-$n" --profile test --progress none >>"$work/$n-src.log" 2>&1; then
    src_ok+=("$n")
  else
    record FAIL "$n source repository" "see $work/$n-src.log"; tail -n 20 "$work/$n-src.log"
  fi
done
ok_src() { local x; for x in "${src_ok[@]}"; do [ "$x" = "$1" ] && return 0; done; return 1; }

for i in "${!names[@]}"; do
  for j in "${!names[@]}"; do
    [ "$i" = "$j" ] && continue
    from=${names[$i]} to=${names[$j]}
    ok_src "$from" || { record SKIP "$from -> $to copy" "no source repository"; continue; }
    src=$(at "${addrs[$i]}" "$run/src") dst=$(at "${addrs[$j]}" "$run/copy-from-$from")
    step "copy $from -> $to"
    log="$work/copy-$from-$to.log"
    args=(); locks "$to" && args=(--object-lock)
    fail() { record FAIL "$from -> $to copy" "$1 (see $log)"; tail -n 15 "$log"; }
    "$vmobj" init --repo "$dst" --key-file "$key" "${args[@]}" >"$log" 2>&1 || { fail "init"; continue; }
    "$vmobj" copy --from "$src" --to "$dst" --key-file "$key" --profile test --progress none >>"$log" 2>&1 || { fail "copy"; continue; }
    mapfile -t got < <("$vmobj" list --repo "$dst" --key-file "$key" --vm matrix 2>>"$log" | sed -n 's/^  //p')
    [ ${#got[@]} -eq 2 ] || { fail "expected 2 backups at $to, found ${#got[@]}"; continue; }
    bad=""
    for m in "${got[@]}"; do
      case $m in *-full) want="$work/want-full.raw" ;; *) want="$work/want-inc-$from.raw" ;; esac
      rm -f "$work/got.raw"
      "$vmobj" restore --repo "$dst" --key-file "$key" --vm matrix --manifest "$m" --output "$work/got.raw" --progress none >>"$log" 2>&1 \
        && cmp -s "$want" "$work/got.raw" || bad="restore of $m differs or failed"
    done
    [ -z "$bad" ] || { fail "$bad"; continue; }
    "$vmobj" copy --from "$src" --to "$dst" --key-file "$key" --profile test --progress none > "$work/copy2.txt" 2>>"$log"
    grep -q "Copied:    0 backups" "$work/copy2.txt" || { fail "a second copy copied again"; continue; }
    record PASS "$from -> $to copy" "both backups restore byte-identical; second copy sends nothing"
  done
done

step "results (run $run)"
pass=0 failed=0
for r in "${results[@]}"; do
  echo "$r"
  case $r in PASS*) pass=$((pass + 1)) ;; FAIL*) failed=$((failed + 1)) ;; esac
done
printf '\n%d passed, %d failed. Test data is under the prefix %s in each bucket; the "test" profile locks it one day.\n' "$pass" "$failed" "$run"
$keep && echo "Kept: $work (key: $key)"
[ "$failed" -eq 0 ]
