#!/usr/bin/env python3
"""Apply the Revi gateway effort cells to the forge integrations that carry the
team-gateway routing block, with a backup, a read-back and a restore (#3283).

The wanted efforts come from ONE file, the routing block the repository's tests
read: pkg/server/cloudpublisher/testdata/revi_gateway_routing.json. For each
integration whose stored routing block routes every effort-bearing node of that
file to the same gateway route (backend, provider, model), the script sets the
cell's `effort` and touches nothing else: the other cells, their order and any
cell a product added stay as stored. An integration that routes none of those
nodes to the gateway is left alone; one that routes some of them is reported
and skipped.

Every call goes through `iterion remote api` with the operator's stored
session (`iterion remote login`); the script never reads, prints or stores a
credential. What it prints and stores is limited to team ids, integration ids,
repository names and routing blocks.

Commands (all but apply and restore are read-only on the instance):

  plan                     list the targets and what apply would change
  backup  --dir D          store each target's current routing block in D
  apply   --dir D (--only X ... | --all) --yes
                           PATCH the wanted block, then read it back
  verify  [--only X ...]   read back: is the wanted block what is stored?
  restore --dir D (--only X ... | --all) --yes
                           PATCH the backed-up block back, then read it back

X is an integration id or a repository full name. apply refuses an
integration with no backup in D, or whose stored block moved since the
backup; both apply and restore are idempotent (a block already in the asked
state is not written again). A PATCH the org parks for an admin's approval
(202, pending_approval) is reported as parked, not applied.
"""

import argparse
import copy
import json
import os
import subprocess
import sys
import tempfile

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
DEFAULT_FIXTURE = os.path.join(REPO_ROOT, "pkg", "server", "cloudpublisher", "testdata", "revi_gateway_routing.json")
ROUTE_FIELDS = ("backend", "provider", "model")


class Refusal(Exception):
    """A condition that stops the command with a message and exit 2."""


def iterion_api(method, path, body=None):
    """Call the instance through `iterion remote api`; answer the decoded JSON
    body (None when empty). The CLI exits non-zero on a non-2xx answer."""
    cmd = ["iterion", "remote", "api", method, path]
    tmp = None
    try:
        if body is not None:
            fd, tmp = tempfile.mkstemp(prefix="revi-effort-", suffix=".json")
            with os.fdopen(fd, "w") as f:
                json.dump(body, f)
            cmd += ["--data", "@" + tmp]
        # cwd outside any checkout: the CLI does not read a project `.env`.
        proc = subprocess.run(cmd, capture_output=True, text=True, cwd="/")
    finally:
        if tmp:
            os.unlink(tmp)
    if proc.returncode != 0:
        detail = (proc.stdout.strip() + " " + proc.stderr.strip()).strip()
        raise Refusal("%s %s failed (exit %d): %s" % (method, path, proc.returncode, detail[-2000:]))
    out = proc.stdout.strip()
    return json.loads(out) if out else None


def normalized(value):
    """The block as the server's decoder reads it: no empty field, keys sorted
    by json.dumps — two blocks are equal when this is."""
    if isinstance(value, dict):
        return {k: normalized(v) for k, v in value.items() if v not in (None, "", [], {})}
    if isinstance(value, list):
        return [normalized(v) for v in value]
    return value


def same(a, b):
    return json.dumps(normalized(a), sort_keys=True) == json.dumps(normalized(b), sort_keys=True)


def load_fixture(path):
    with open(path) as f:
        fixture = json.load(f)
    cells = {}
    for cell in fixture["routing"]["overrides"]:
        if cell.get("effort"):
            cells[cell["selector"]] = cell
    if not cells:
        raise Refusal("%s pins no effort" % path)
    return cells


def discover():
    """Every forge integration the operator can read: (team, integration)."""
    orgs = iterion_api("GET", "/api/admin/orgs")
    orgs = orgs if isinstance(orgs, list) else orgs.get("orgs") or orgs.get("items") or []
    found = []
    for org in orgs:
        teams = iterion_api("GET", "/api/admin/orgs/%s/teams" % org["id"])
        teams = teams if isinstance(teams, list) else teams.get("teams") or teams.get("items") or []
        for team in teams:
            listing = iterion_api("GET", "/api/teams/%s/forge/repo-bots" % team["id"]) or {}
            for integ in listing.get("integrations") or []:
                found.append({
                    "team": team.get("name") or team.get("slug") or team["id"],
                    "team_id": team["id"],
                    "id": integ["id"],
                    "repo": integ.get("repo_full_name", ""),
                    "routing": integ.get("routing"),
                })
    return found


def stored_routing(team_id, integration_id):
    listing = iterion_api("GET", "/api/teams/%s/forge/repo-bots" % team_id) or {}
    for integ in listing.get("integrations") or []:
        if integ["id"] == integration_id:
            return integ.get("routing")
    raise Refusal("integration %s is no longer listed under team %s" % (integration_id, team_id))


def wanted_block(routing, cells):
    """(verdict, wanted, changes): verdict is "target", "none" (no effort
    node routed to the gateway) or "partial: ..."; changes lists
    (selector, stored effort, wanted effort) for the cells that move."""
    overrides = (routing or {}).get("overrides") or []
    matched = {}
    for i, o in enumerate(overrides):
        cell = cells.get(o.get("selector"))
        if cell and all(o.get(f, "") == cell.get(f, "") for f in ROUTE_FIELDS):
            matched[o["selector"]] = i
    if not matched:
        return "none", None, []
    missing = sorted(set(cells) - set(matched))
    if missing:
        return "partial: no gateway cell for " + ", ".join(missing), None, []
    wanted = copy.deepcopy(routing)
    changes = []
    for sel, i in sorted(matched.items()):
        have = wanted["overrides"][i].get("effort", "")
        want = cells[sel]["effort"]
        if have != want:
            changes.append((sel, have or "unset", want))
            wanted["overrides"][i]["effort"] = want
    return "target", wanted, changes


def selected(targets, only, all_):
    if all_ and only:
        raise Refusal("--all and --only are exclusive")
    if all_:
        return targets
    if not only:
        raise Refusal("name the integrations: --only <integration id|repo> (repeatable), or --all")
    picked = [t for t in targets if t["id"] in only or t["repo"] in only]
    unknown = set(only) - {t["id"] for t in picked} - {t["repo"] for t in picked}
    if unknown:
        raise Refusal("not a target integration: " + ", ".join(sorted(unknown)))
    return picked


def backup_path(directory, integ):
    return os.path.join(directory, integ["id"] + ".json")


def read_backup(directory, integ):
    path = backup_path(directory, integ)
    if not os.path.exists(path):
        raise Refusal("%s (%s): no backup at %s — run backup first" % (integ["repo"], integ["id"], path))
    with open(path) as f:
        return json.load(f)


def cmd_plan(args, cells):
    integs = discover()
    targets = 0
    print("%-18s %-62s %-36s %s" % ("team", "repo", "integration", "verdict"))
    for integ in integs:
        verdict, _, changes = wanted_block(integ["routing"], cells)
        if verdict == "none":
            line = "left alone (no gateway effort cell)"
        elif verdict == "target":
            targets += 1
            line = "UNCHANGED (already applied)" if not changes else "CHANGE " + ", ".join("%s %s->%s" % c for c in changes)
        else:
            line = "SKIP " + verdict
        print("%-18s %-62s %-36s %s" % (integ["team"][:18], integ["repo"][:62], integ["id"], line))
    print("\n%d integration(s) read, %d target(s)" % (len(integs), targets))
    return 0


def targets_of(cells):
    return [i for i in discover() if wanted_block(i["routing"], cells)[0] == "target"]


def cmd_backup(args, cells):
    os.makedirs(args.dir, mode=0o700, exist_ok=True)
    targets = targets_of(cells)
    for integ in targets:
        record = {"team_id": integ["team_id"], "team": integ["team"], "id": integ["id"], "repo": integ["repo"], "routing": integ["routing"]}
        path = backup_path(args.dir, integ)
        if os.path.exists(path):
            with open(path) as f:
                kept = json.load(f)
            if not same(kept["routing"], integ["routing"]):
                raise Refusal("%s already holds a DIFFERENT backup of %s: a backup is never overwritten — use a new --dir" % (path, integ["repo"]))
            print("kept     %s (%s)" % (integ["repo"], integ["id"]))
            continue
        with open(path, "w") as f:
            json.dump(record, f, indent=2, sort_keys=True)
            f.write("\n")
        print("saved    %s (%s) -> %s" % (integ["repo"], integ["id"], path))
    print("\n%d target(s) backed up in %s" % (len(targets), args.dir))
    return 0


def patch_routing(integ, block):
    """PATCH the routing block alone (absent fields keep their stored value);
    answer "parked" when the org queued it for approval, else "written"."""
    answer = iterion_api("PATCH", "/api/teams/%s/forge/repo-bots/%s" % (integ["team_id"], integ["id"]), {"routing": block})
    if isinstance(answer, dict) and answer.get("pending_approval"):
        return "parked"
    return "written"


def cmd_apply(args, cells):
    if not args.yes:
        raise Refusal("apply writes to the instance: add --yes")
    failed = 0
    for integ in selected(targets_of(cells), args.only, args.all):
        backup = read_backup(args.dir, integ)
        if not same(backup["routing"], integ["routing"]):
            _, wanted_from_backup, _ = wanted_block(backup["routing"], cells)
            if not same(integ["routing"], wanted_from_backup):
                raise Refusal("%s (%s): the stored block moved since the backup in %s — back it up again in a new --dir" % (integ["repo"], integ["id"], args.dir))
        _, wanted, changes = wanted_block(integ["routing"], cells)
        if not changes:
            print("unchanged %s (%s): already applied" % (integ["repo"], integ["id"]))
            continue
        outcome = patch_routing(integ, wanted)
        if outcome == "parked":
            print("PARKED   %s (%s): the org queued the change for an admin's approval — not applied" % (integ["repo"], integ["id"]))
            failed += 1
            continue
        if same(stored_routing(integ["team_id"], integ["id"]), wanted):
            print("applied  %s (%s): %s" % (integ["repo"], integ["id"], ", ".join("%s %s->%s" % c for c in changes)))
        else:
            print("MISMATCH %s (%s): the block read back is not the one written — restore it from %s" % (integ["repo"], integ["id"], args.dir))
            failed += 1
    return 1 if failed else 0


def cmd_verify(args, cells):
    integs = targets_of(cells)
    if args.only:
        integs = selected(integs, args.only, False)
    behind = 0
    for integ in integs:
        _, wanted, changes = wanted_block(integ["routing"], cells)
        if changes:
            behind += 1
            print("NOT APPLIED %s (%s): %s" % (integ["repo"], integ["id"], ", ".join("%s %s->%s" % c for c in changes)))
        else:
            print("applied     %s (%s)" % (integ["repo"], integ["id"]))
    return 1 if behind else 0


def cmd_restore(args, cells):
    if not args.yes:
        raise Refusal("restore writes to the instance: add --yes")
    records = []
    for name in sorted(os.listdir(args.dir)):
        if name.endswith(".json"):
            with open(os.path.join(args.dir, name)) as f:
                records.append(json.load(f))
    if not records:
        raise Refusal("no backup in %s" % args.dir)
    if args.all and args.only:
        raise Refusal("--all and --only are exclusive")
    if not args.all and not args.only:
        raise Refusal("name the integrations: --only <integration id|repo> (repeatable), or --all")
    picked = records if args.all else [r for r in records if r["id"] in args.only or r["repo"] in args.only]
    if not picked:
        raise Refusal("no backup in %s names %s" % (args.dir, ", ".join(args.only)))
    failed = 0
    for rec in picked:
        integ = {"team_id": rec["team_id"], "id": rec["id"], "repo": rec["repo"]}
        if same(stored_routing(rec["team_id"], rec["id"]), rec["routing"]):
            print("unchanged %s (%s): already the backed-up block" % (rec["repo"], rec["id"]))
            continue
        # A null block clears the stored one: the backup of an integration
        # that carried none restores none.
        outcome = patch_routing(integ, rec["routing"])
        if outcome == "parked":
            print("PARKED   %s (%s): the org queued the restore for an admin's approval — not restored" % (rec["repo"], rec["id"]))
            failed += 1
            continue
        if same(stored_routing(rec["team_id"], rec["id"]), rec["routing"]):
            print("restored %s (%s)" % (rec["repo"], rec["id"]))
        else:
            print("MISMATCH %s (%s): the block read back is not the backup" % (rec["repo"], rec["id"]))
            failed += 1
    return 1 if failed else 0


def main(argv):
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0], formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--fixture", default=DEFAULT_FIXTURE, help="the routing block whose efforts are applied")
    sub = ap.add_subparsers(dest="command")
    sub.add_parser("plan")
    p = sub.add_parser("backup")
    p.add_argument("--dir", required=True)
    for name in ("apply", "restore"):
        p = sub.add_parser(name)
        p.add_argument("--dir", required=True)
        p.add_argument("--only", action="append")
        p.add_argument("--all", action="store_true")
        p.add_argument("--yes", action="store_true")
    p = sub.add_parser("verify")
    p.add_argument("--only", action="append")
    args = ap.parse_args(argv)
    command = args.command or "plan"
    try:
        cells = load_fixture(args.fixture)
        return {"plan": cmd_plan, "backup": cmd_backup, "apply": cmd_apply, "verify": cmd_verify, "restore": cmd_restore}[command](args, cells)
    except Refusal as err:
        print("revi-effort-cells: %s" % err, file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
