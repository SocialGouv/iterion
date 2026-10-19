#!/usr/bin/env bash
# The rgaa-audit advisory: the studio's rendered a11y regression net, with no
# model in the loop. Serves the built studio (studio/e2e/serve.mjs), audits
# the studio sources with the pinned engine, captures the sample pages,
# replays the committed verdict ledger (.ultra11y/verdicts/rgaa.json) against
# the fresh audit, and holds the run to the engine's completeness check.
#
# Drift = any of: a replayed verdict gone stale or obsolete (the source under
# a decided criterion changed — re-adjudicate it), a criterion missing from
# the ledger (never adjudicated), or a page cell the completeness check still
# holds open. Exit 1 names them; exit 0 means the studio matches the ledger.
#
# Requires in the environment:
#   ULTRA11Y_BIN  the pinned engine entry (scripts/ultra11y-engine.sh)
#   ITERION_BIN   the built iterion binary with the studio SPA embedded
# and on disk:
#   studio/.ultra11yrc.json          the 21-page sample (committed)
#   .ultra11y/verdicts/rgaa.json     the committed verdict ledger
set -euo pipefail

: "${ULTRA11Y_BIN:?set ULTRA11Y_BIN (scripts/ultra11y-engine.sh)}"
: "${ITERION_BIN:?set ITERION_BIN (go build ./cmd/iterion with the studio SPA embedded)}"
STD="rgaa"
ROOT=$(pwd)
WORK=$(mktemp -d)
trap 'kill "${SERV_PID:-0}" 2>/dev/null || true' EXIT

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

# 2. The rendered layer, merged into the audit (scan navigates the sample).
# From the repo root: the committed .ultra11yrc.json carries the 21-page
# sample (the e2e server on 4899) and the studio/src sources.
node "$ULTRA11Y_BIN" scan --sample --runtime local --cwd . --standard "$STD" \
  --merge "$WORK/audit-latest.json" --json > "$WORK/scan.json" 2> "$WORK/scan.err" || {
    echo "advisory: the rendered scan failed:" >&2
    tail -5 "$WORK/scan.err" >&2
    exit 1
  }
cd studio

# 3. The committed ledger replayed against the fresh audit. From studio/:
#    the ledger's capture anchors (.ultra11y/pages/<id>/…) resolve there, and
#    the sources ride the fresh evidence, not the filesystem.
REPLAYED=""; STALE=""; OBSOLETE=""; MISSING=""
if node "$ULTRA11Y_BIN" verify --apply "$ROOT/.ultra11y/verdicts/rgaa.json" \
      --in "$WORK/audit-latest.json" --out "$WORK/replay" --json --standard "$STD" --lang fr \
      > "$WORK/replay.json" 2> "$WORK/replay.err"; then
  REPLAYED=$(node -e 'const j=JSON.parse(require("fs").readFileSync(process.argv[1]));process.stdout.write(String(j.replayed ?? ""))' "$WORK/replay.json")
else
  # Exit 1 (every fresh verdict refused) still prints the accounting.
  REPLAYED=$(node -e 'const j=JSON.parse(require("fs").readFileSync(process.argv[1]));process.stdout.write(String(j.replayed ?? ""))' "$WORK/replay.json" 2>/dev/null || echo "?")
fi
node -e '
const j = JSON.parse(require("fs").readFileSync(process.argv[1]));
const r = j.replay ?? j;
for (const [k, key] of [["stale", "stale"], ["obsolete", "obsolete"], ["missing", "missing"]]) {
  const v = r[key] ?? [];
  if (v.length) console.log(`DRIFT ${k}: ${v.join(", ")}`);
}
' "$WORK/replay.json" | tee "$WORK/drift.txt" >&2
STALE=$(grep -c "DRIFT" "$WORK/drift.txt" || true)

# 4. The completeness check: page cells held open.
CHECK_OK=true
node "$ULTRA11Y_BIN" check --in "$WORK/audit-latest.json" --require-decided=pages --json --standard "$STD" \
  > "$WORK/check.json" 2> "$WORK/check.err" || CHECK_OK=false
node -e '
const j = JSON.parse(require("fs").readFileSync(process.argv[1]));
const d = j.decided ?? {};
for (const p of d.pages ?? []) console.log(`PAGE CELL OPEN: ${p.name ?? p.id} — ${p.undecided.join(", ")}`);
' "$WORK/check.json" | tee "$WORK/cells.txt" >&2

# 5. The verdict: drift or open cells fail the advisory.
if [ "$STALE" -gt 0 ] || [ -s "$WORK/cells.txt" ] || [ "$CHECK_OK" != "true" ]; then
  echo "advisory: DRIFT — the studio no longer matches the committed ledger (see above)." >&2
  exit 1
fi
echo "advisory: clean — $REPLAYED verdict(s) replayed, no drift, no open page cell."
