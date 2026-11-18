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
                           set the fixture's efforts, then read the block back
  verify  [--only X ...]   read back: does every target carry the efforts?
  restore --dir D (--only X ... | --all) --yes
                           put each effort cell back to its backed-up effort,
                           then read the block back
  migrate-ids --dir D (--only X ... | --all) (--yes | --dry-run)
                           rename the retired review-pr node selectors to
                           their role names (ADR-169), then read the block back

X is an integration id, or a repository full name that names one integration
only (a name two teams share is refused: name the id).

backup never overwrites a backup: one already in D is kept when the stored
block is that backup with this procedure's efforts on it, refused otherwise;
and it refuses to write a new one for a block that already carries a fixture
effort — that backup would restore the efforts, not the block before them.

apply refuses an integration with no backup in D, one that is no longer a
target, or one whose stored block moved since the backup — the efforts of
the fixture's cells set aside, so a fixture update applies over the original
backup dir. restore reverses the effort fields only, on the block as it is
stored now: a cell or a change made after the apply stays; an effort cell
whose effort is neither the fixture's nor the backup's was changed by someone
else, and one off the gateway route that carries the fixture's effort
(re-routed since the apply, or set by hand) cannot be judged: both are
refused, not overwritten. Both are idempotent: a block already in the asked
state is not written again. verify reads every integration back and exits 1
when a target
is behind, or when one that is not a target carries a fixture effort on a
fixture selector's cell (re-routed since the apply, or set by hand).

Each integration is read again just before its check and its PATCH. That
narrows, without closing, the window in which another writer's change lands
between the read and the PATCH and is replaced: closing it needs a version the
server checks, and the repo-bots PATCH takes none (RepoIntegration.Version is
not exposed, the request has no expected-version field).

migrate-ids exists because a cell whose selector names no node decides
nothing: the launch falls back on the bot's own literals, so the routing AND
the effort the cell carried go inert without a word. It renames
reviewer_gpt/reviewer_claude/converge_gpt (and their glance variants) to
reviewer/reviewer_alternate/reviewer_glance/converge, drops the cells whose
node no longer exists (reviewer_claude_glance, and converge#glm — the merge
runs on claw, which reads no provider hint). Two cells landing on one
selector — a block that carried both `converge` and `converge_gpt` — leave
the renamed one: it names the route the node takes now. Every other cell, its
order and its fields stay as stored; the backup holds the block as it was,
retired names included.

A failure on one integration (an HTTP error, a refusal, a read-back that
differs) is reported and the command goes on with the next; the exit code is
non-zero when any failed. A PATCH the org parks for an admin's approval (202,
pending_approval) is reported as parked, not applied.
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
    """A condition that stops the command, or one integration, with a message."""


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
    """Every forge integration the operator can read."""
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


def gateway_cells(routing, cells):
    """The index, in the block, of each effort node the block routes to the
    fixture's gateway route (same backend, provider and model)."""
    matched = {}
    for i, o in enumerate((routing or {}).get("overrides") or []):
        cell = cells.get(o.get("selector"))
        if cell and all(o.get(f, "") == cell.get(f, "") for f in ROUTE_FIELDS):
            matched[o["selector"]] = i
    return matched


def wanted_block(routing, cells):
    """(verdict, wanted, changes): verdict is "target", "none" (no effort
    node routed to the gateway) or "partial: ..."; changes lists
    (selector, stored effort, wanted effort) for the cells that move."""
    matched = gateway_cells(routing, cells)
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


def restored_block(stored, backup, cells):
    """(block, changes): stored with each cell of a fixture selector put
    back to its backed-up effort — only where it carries the fixture's
    effort on the gateway route, every other field and cell as stored. A
    cell carrying neither effort is a change someone else made, and one off
    the gateway route that carries the fixture's effort cannot be judged
    (re-routed since the apply, or set by hand): both are refused."""
    backed = {}
    for o in (backup or {}).get("overrides") or []:
        backed[o.get("selector")] = o.get("effort", "")
    block = copy.deepcopy(stored)
    on_gateway = gateway_cells(block, cells)
    changes = []
    for i, cell in enumerate((block or {}).get("overrides") or []):
        sel = cell.get("selector")
        if sel not in cells:
            continue
        have, original = cell.get("effort", ""), backed.get(sel, "")
        if have == original:
            continue
        if have != cells[sel]["effort"]:
            raise Refusal("cell %s carries effort %r, neither the fixture's %r nor the backup's %r: changed since the apply, not restored" % (sel, have, cells[sel]["effort"], original))
        if sel not in on_gateway or on_gateway[sel] != i:
            raise Refusal("cell %s carries the fixture's effort %r but is not routed to the gateway: re-routed since the apply, or set by hand — not restored" % (sel, have))
        if original:
            cell["effort"] = original
        else:
            cell.pop("effort", None)
        changes.append((sel, have, original or "unset"))
    return block, changes


def masked(routing, cells):
    """routing with the effort of every fixture cell removed: what is left
    when this procedure's own writes — whichever fixture version made them —
    are set aside."""
    block = copy.deepcopy(routing) or {}
    for cell in block.get("overrides") or []:
        if cell.get("selector") in cells:
            cell.pop("effort", None)
    return block


def carries_fixture_efforts(routing, cells):
    """The gateway cells of routing that carry the fixture's own effort."""
    overrides = (routing or {}).get("overrides") or []
    return sorted(sel for sel, i in gateway_cells(routing, cells).items() if overrides[i].get("effort", "") == cells[sel]["effort"])


def describe(changes):
    return ", ".join("%s %s->%s" % c for c in changes)


def selected(targets, only, all_):
    if all_ and only:
        raise Refusal("--all and --only are exclusive")
    if all_:
        return targets
    if not only:
        raise Refusal("name the integrations: --only <integration id|repo> (repeatable), or --all")
    picked = []
    for name in only:
        hits = [t for t in targets if t["id"] == name or t["repo"] == name]
        if not hits:
            raise Refusal("not a target integration: " + name)
        if len(hits) > 1:
            raise Refusal("%s names %d integrations (%s): name one by its id" % (name, len(hits), ", ".join(t["id"] for t in hits)))
        if hits[0] not in picked:
            picked.append(hits[0])
    return picked


def backup_path(directory, integ):
    return os.path.join(directory, integ["id"] + ".json")


def read_backup(directory, integ):
    path = backup_path(directory, integ)
    if not os.path.exists(path):
        raise Refusal("no backup at %s — run backup first" % path)
    with open(path) as f:
        return json.load(f)


# The retired node names of ADR-169, by role. A bot's review-pr nodes were
# renamed from vendor words to roles; a cell still naming the old id decides
# nothing, so the launch falls back on the bot's literals — silently, with the
# routing and the effort both gone. Empty = the node no longer exists.
RETIRED_NODES = {
    "reviewer_gpt": "reviewer",
    "reviewer_gpt_glance": "reviewer_glance",
    "reviewer_claude": "reviewer_alternate",
    "reviewer_claude_glance": "",
    "converge_gpt": "converge",
}

# Routes retired with their node: `converge` runs on claw, which reads no
# provider hint, so it declares no `#glm` rescue any more.
RETIRED_ROUTES = {"converge#glm"}


def renamed_selector(selector):
    """(new selector, changed?) for one stored selector; new is "" when the
    selector names a cell no node can take any more."""
    node, sep, route = selector.partition("#")
    bot, slash, name = node.rpartition("/")
    if not slash:
        return selector, False
    if name + sep + route in RETIRED_ROUTES:
        return "", True
    if name not in RETIRED_NODES:
        return selector, False
    new = RETIRED_NODES[name]
    if not new:
        return "", True
    return bot + "/" + new + sep + route, True


def migrated_block(routing):
    """The block with every retired selector renamed; answer (block, changes,
    dropped), where a change is (from, to)."""
    out = copy.deepcopy(routing or {})
    changes, dropped = [], []
    kept, from_rename = [], set()
    for o in out.get("overrides") or []:
        new, retired = renamed_selector(o.get("selector", ""))
        if not retired:
            kept.append((o["selector"], o, False))
            continue
        if not new:
            dropped.append(o["selector"])
            continue
        if new != o["selector"]:
            changes.append((o["selector"], new))
            from_rename.add(new)
        kept.append((new, dict(o, selector=new), True))
    # A rename landing on a cell the block already carried: the renamed one
    # wins (it names the route the node takes now), the older spelling is
    # dropped rather than left to contend for one selector.
    out["overrides"] = [o for sel, o, renamed in kept if renamed or sel not in from_rename]
    return out, changes, dropped


def carries_retired(routing):
    for o in (routing or {}).get("overrides") or []:
        if renamed_selector(o.get("selector", ""))[1]:
            return True
    return False


def migrate_one(args, integ):
    """Rename one integration's cells; answer True when it carries the roles."""
    backup = backup_path(args.dir, integ)
    if not os.path.exists(backup):
        record = {"team_id": integ["team_id"], "team": integ["team"], "id": integ["id"], "repo": integ["repo"], "routing": integ["routing"]}
        with open(backup, "w") as f:
            json.dump(record, f, indent=2, sort_keys=True)
            f.write("\n")
        print("saved    %s (%s) -> %s" % (integ["repo"], integ["id"], backup))
    stored = stored_routing(integ["team_id"], integ["id"])
    wanted, changes, dropped = migrated_block(stored)
    if not changes and not dropped:
        print("unchanged %s (%s): no retired selector" % (integ["repo"], integ["id"]))
        return True
    if args.dry_run:
        print("WOULD    %s (%s): %s%s" % (integ["repo"], integ["id"],
              ", ".join("%s -> %s" % c for c in changes),
              ("" if not dropped else ", drop " + ", ".join(dropped))))
        return True
    if patch_routing(integ, wanted) == "parked":
        print("PARKED   %s (%s): the org queued the change for an admin's approval — not applied" % (integ["repo"], integ["id"]))
        return False
    if not same(stored_routing(integ["team_id"], integ["id"]), wanted):
        raise Refusal("the block read back is not the one written — restore it from %s" % args.dir)
    print("migrated %s (%s): %s%s" % (integ["repo"], integ["id"],
          ", ".join("%s -> %s" % c for c in changes),
          ("" if not dropped else ", dropped " + ", ".join(dropped))))
    return True


def cmd_migrate_ids(args):
    integs = discover()
    targets = [i for i in integs if carries_retired(i["routing"])]
    if not targets:
        # Still answer the names the operator asked about, so a one-name run
        # says which integration it looked at and found nothing to do on.
        for integ in selected(integs, args.only, args.all) if args.only else []:
            print("unchanged %s (%s): no retired selector" % (integ["repo"], integ["id"]))
        print("every integration already names the roles")
        return 0
    picked = selected(targets, args.only, args.all)
    os.makedirs(args.dir, mode=0o700, exist_ok=True)
    failed = each(picked, "migrated", lambda integ: migrate_one(args, integ))
    print("\n%d integration(s) carry a retired selector, %d migrated" % (len(targets), len(picked) - failed))
    return failed


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
            line = "UNCHANGED (already applied)" if not changes else "CHANGE " + describe(changes)
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
            # The block apply leaves over this backup — whichever fixture
            # version wrote the efforts — is the backup's, kept as written.
            if not same(masked(kept["routing"], cells), masked(integ["routing"], cells)):
                raise Refusal("%s already holds a DIFFERENT backup of %s: a backup is never overwritten — use a new --dir" % (path, integ["repo"]))
            print("kept     %s (%s)" % (integ["repo"], integ["id"]))
            continue
        carried = carries_fixture_efforts(integ["routing"], cells)
        if carried:
            raise Refusal("%s (%s) already carries the fixture's effort on %s: a backup now would restore those efforts — back up from the original backup dir, or remove them first" % (integ["repo"], integ["id"], ", ".join(carried)))
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


def apply_one(args, cells, integ):
    """Apply the efforts to one integration; answer True when it holds them."""
    backup = read_backup(args.dir, integ)
    stored = stored_routing(integ["team_id"], integ["id"])
    verdict, wanted, changes = wanted_block(stored, cells)
    if verdict != "target":
        raise Refusal("no longer a target: " + verdict)
    # Moved since the backup, the efforts of the fixture's cells set aside:
    # those are this procedure's own writes, so a fixture update applies
    # over the ORIGINAL backup dir.
    if not same(masked(backup["routing"], cells), masked(stored, cells)):
        raise Refusal("the stored block moved since the backup in %s — back it up again in a new --dir" % args.dir)
    if not changes:
        print("unchanged %s (%s): already applied" % (integ["repo"], integ["id"]))
        return True
    if patch_routing(integ, wanted) == "parked":
        print("PARKED   %s (%s): the org queued the change for an admin's approval — not applied" % (integ["repo"], integ["id"]))
        return False
    if not same(stored_routing(integ["team_id"], integ["id"]), wanted):
        raise Refusal("the block read back is not the one written — restore it from %s" % args.dir)
    print("applied  %s (%s): %s" % (integ["repo"], integ["id"], describe(changes)))
    return True


def restore_one(args, cells, record):
    """Put one integration's effort cells back to their backed-up efforts;
    answer True when they carry them."""
    integ = {"team_id": record["team_id"], "id": record["id"], "repo": record["repo"]}
    stored = stored_routing(record["team_id"], record["id"])
    block, changes = restored_block(stored, record["routing"], cells)
    if not changes:
        print("unchanged %s (%s): its effort cells already carry the backed-up efforts" % (record["repo"], record["id"]))
        return True
    if patch_routing(integ, block) == "parked":
        print("PARKED   %s (%s): the org queued the restore for an admin's approval — not restored" % (record["repo"], record["id"]))
        return False
    if not same(stored_routing(record["team_id"], record["id"]), block):
        raise Refusal("the block read back is not the one written")
    print("restored %s (%s): %s" % (record["repo"], record["id"], describe(changes)))
    return True


def each(items, label, run):
    """Run one integration at a time; a failure is reported and the next one
    still runs. Answer the exit code."""
    failed = 0
    for item in items:
        try:
            if not run(item):
                failed += 1
        except Refusal as err:
            print("FAILED   %s (%s): %s" % (item["repo"], item["id"], err))
            failed += 1
    if failed:
        print("\n%d of %d integration(s) not %s" % (failed, len(items), label))
    return 1 if failed else 0


def cmd_apply(args, cells):
    if not args.yes:
        raise Refusal("apply writes to the instance: add --yes")
    picked = selected(targets_of(cells), args.only, args.all)
    return each(picked, "applied", lambda integ: apply_one(args, cells, integ))


def carried_off_target(routing, cells):
    """The fixture selectors whose cell carries the fixture's effort."""
    return sorted(o.get("selector") for o in (routing or {}).get("overrides") or []
                  if o.get("selector") in cells and o.get("effort", "") == cells[o.get("selector")]["effort"])


def cmd_verify(args, cells):
    """Read every integration back. A target holds the efforts or is behind;
    one that is not a target is skipped unless a cell of a fixture selector
    carries the fixture's effort — re-routed since the apply, or set by hand,
    and nothing this script can verify. A target behind and such a cell both
    exit 1."""
    integs = discover()
    if args.only:
        integs = selected(integs, args.only, False)
    behind = 0
    for integ in integs:
        verdict, _, changes = wanted_block(integ["routing"], cells)
        if verdict != "target":
            carried = carried_off_target(integ["routing"], cells)
            if carried:
                behind += 1
                print("RE-ROUTED   %s (%s): %s, yet %s carry the fixture's effort — re-routed since the apply, or set by hand" % (integ["repo"], integ["id"], verdict, ", ".join(carried)))
            elif verdict != "none":
                print("skipped     %s (%s): %s" % (integ["repo"], integ["id"], verdict))
        elif changes:
            behind += 1
            print("NOT APPLIED %s (%s): %s" % (integ["repo"], integ["id"], describe(changes)))
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
    picked = selected(records, args.only, args.all)
    return each(picked, "restored", lambda record: restore_one(args, cells, record))


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
    p = sub.add_parser("migrate-ids")
    p.add_argument("--dir", required=True)
    p.add_argument("--only", action="append")
    p.add_argument("--all", action="store_true")
    p.add_argument("--yes", action="store_true")
    p.add_argument("--dry-run", action="store_true")
    args = ap.parse_args(argv)
    command = args.command or "plan"
    try:
        if command == "migrate-ids":
            if not args.yes and not args.dry_run:
                raise Refusal("migrate-ids writes to the instance: add --yes (or --dry-run)")
            return cmd_migrate_ids(args)
        cells = load_fixture(args.fixture)
        return {"plan": cmd_plan, "backup": cmd_backup, "apply": cmd_apply, "verify": cmd_verify, "restore": cmd_restore}[command](args, cells)
    except Refusal as err:
        print("revi-effort-cells: %s" % err, file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
