#!/usr/bin/env bash
# The rgaa-audit advisory: the studio's rendered a11y regression net, with no
# model in the loop. Serves the built studio (studio/e2e/serve.mjs), audits
# the studio sources with the pinned engine, captures the 21-page sample,
# replays the committed verdict ledger (.ultra11y/verdicts/rgaa.json) against
# the fresh audit, and holds the REPLAYED audit to the engine's completeness
# check (with --require-rendered: a run whose captures never landed is not
# judged at all).
#
# Drift = any of: a replayed verdict gone stale or obsolete (the source under
# a decided criterion changed — re-adjudicate it), a replayed verdict refused
# by the engine's citation gate, a criterion missing from the ledger (never
# adjudicated), or a page cell the completeness check still holds open.
# Exit 1 names them; exit 0 means the studio matches the ledger.
#
# Requires in the environment:
#   ULTRA11Y_BIN  the pinned engine entry (scripts/ultra11y-engine.sh)
#   ITERION_BIN   the built iterion binary with the studio SPA embedded
# and on disk:
#   .ultra11yrc.json                 the 21-page sample (committed, repo root)
#   .ultra11y/verdicts/rgaa.json     the committed verdict ledger
#
# Known noise: capture churn between serve.mjs boots (~a third of the ledger
# per boot) reports as stale in the same DRIFT format as real drift — until
# the served content is pinned, a red run names the criteria but cannot
# distinguish churn from source drift by itself.
set -euo pipefail

: "${ULTRA11Y_BIN:?set ULTRA11Y_BIN (scripts/ultra11y-engine.sh)}"
: "${ITERION_BIN:?set ITERION_BIN (go build ./cmd/iterion with the studio SPA embedded)}"
STD="rgaa"
ROOT=$(pwd)
WORK=$(mktemp -d)
SERV_PID=""
trap '[ -n "$SERV_PID" ] && kill "$SERV_PID" 2>/dev/null || true' EXIT

# The rendered layer: the e2e target serves the seeded studio on 4899. The
# engine's scan navigates it with Chromium, so PLAYWRIGHT_BROWSERS_PATH (or
# the engine package's own browsers) must carry the build it resolves.
export ITERION_BIN ITERION_E2E_FIXED_RUN_IDS=1
node studio/e2e/serve.mjs > "$WORK/serve.log" 2>&1 &
SERV_PID=$!
for _ in $(seq 1 60); do
  if curl -sf -o /dev/null http://127.0.0.1:4899/api/server/info; then break; fi
  sleep 1
done
curl -sf -o /dev/null http://127.0.0.1:4899/api/server/info || {
  echo "advisory: the studio server never came up ($WORK/serve.log)" >&2
  exit 1
}

# 1. The static audit of the studio sources.
node "$ULTRA11Y_BIN" audit studio/src --graph --jsx --out "$WORK" --json --standard "$STD" --lang fr \
  > "$WORK/audit.json" 2> "$WORK/audit.err"

# 2. The rendered layer, merged INTO the audit verify and check will read
#    (scan writes its merge to --out, default `audits/` — a directory
#    nothing downstream read before #2500's review caught it).
node "$ULTRA11Y_BIN" scan --sample --runtime local --cwd . --standard "$STD" \
  --merge "$WORK/audit-latest.json" --out "$WORK" --json > "$WORK/scan.json" 2> "$WORK/scan.err" || {
    echo "advisory: the rendered scan failed:" >&2
    tail -5 "$WORK/scan.err" >&2
    exit 1
  }
# A serve that dies mid-scan still exits 0 with « not recorded » per page —
# a 7-of-21 capture set must fail here, not pass as judged.
if grep -q "not recorded" "$WORK/scan.err"; then
  echo "advisory: DRIFT — the scan could not capture every sample page:" >&2
  grep "non enregistrée" "$WORK/scan.err" | sed 's/^⚠️ ultra11y scan : //' >&2
  exit 1
fi

# 3. The committed ledger replayed against the fresh, captured audit (from
#    the repo root: the ledger's source anchors are root-relative and the
#    capture anchors resolve through the audit's own recorded inputs).
node "$ULTRA11Y_BIN" verify --apply "$ROOT/.ultra11y/verdicts/rgaa.json" \
      --in "$WORK/audit-latest.json" --out "$WORK/replay" --json --standard "$STD" --lang fr \
      > "$WORK/replay.json" 2> "$WORK/replay.err" || true
if [ ! -s "$WORK/replay.json" ]; then
  echo "advisory: the ledger replay produced no accounting (corrupt ledger?):" >&2
  tail -5 "$WORK/replay.err" >&2
  exit 1
fi
node -e '
const j = JSON.parse(require("fs").readFileSync(process.argv[1]));
const r = j.replay ?? j;
const applied = j.applied ?? 0;
const rejected = j.rejected ?? 0;
if (rejected > 0) console.log(`REFUSED: ${rejected} replayed verdict(s) failed the engine\x27s citation gate (${j.rejectedCriteria?.join(", ") ?? ""})`);
for (const key of ["stale", "obsolete", "missing"]) {
  const v = r[key] ?? [];
  if (v.length) console.log(`DRIFT ${key}: ${v.join(", ")}`);
}
if (!rejected && !(r.stale ?? []).length && !(r.obsolete ?? []).length && !(r.missing ?? []).length)
  console.log(`CLEAN: ${applied} verdict(s) replayed, no drift.`);
' "$WORK/replay.json" | tee "$WORK/drift.txt" >&2

# 4. The completeness check over the REPLAYED audit (the audit the verdicts
#    were applied to): page cells held open, and --require-rendered so a run
#    whose captures never landed is refused instead of judged.
CHECK_OK=true
node "$ULTRA11Y_BIN" check --in "$WORK/replay/audit-latest.json" --require-decided=pages --require-rendered --json --standard "$STD" \
  > "$WORK/check.json" 2> "$WORK/check.err" || CHECK_OK=false
if [ -s "$WORK/check.json" ]; then
  node -e '
const j = JSON.parse(require("fs").readFileSync(process.argv[1]));
const d = j.decided ?? {};
for (const p of d.pages ?? []) console.log(`PAGE CELL OPEN: ${p.name ?? p.id} — ${p.undecided.join(", ")}`);
' "$WORK/check.json" | tee "$WORK/cells.txt" >&2
else
  echo "advisory: the completeness check produced no accounting:" >&2
  tail -5 "$WORK/check.err" >&2
  echo "advisory: DRIFT — the studio no longer matches the committed ledger (see above)." >&2
  exit 1
fi

# 5. The verdict: refused verdicts, drift or open cells fail the advisory.
if [ -s "$WORK/drift.txt" ] && \grep -qE "^(REFUSED|DRIFT)" "$WORK/drift.txt"; then
  echo "advisory: DRIFT — the studio no longer matches the committed ledger (see above)." >&2
  exit 1
fi
if [ "$CHECK_OK" != "true" ] || [ -s "$WORK/cells.txt" ]; then
  echo "advisory: DRIFT — page cells are still open (see above)." >&2
  exit 1
fi
echo "advisory: clean — the studio matches the committed ledger."
