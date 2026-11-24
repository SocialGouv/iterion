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
  migrate-ids --dir D (--only X ... | --all) (--stage | --prune) (--yes | --dry-run)
                           move the retired review-pr and evolve node
                           selectors onto their role names (ADR-169), in two
                           phases around the roll, then read the block back

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

An integration with no `mono_family` launch var is AMBIGUOUS (the old engine
resolved the family from credentials at launch): where the two readings
migrate its block differently, migrate-ids prints an AMBIGUOUS line, writes
nothing and exits non-zero unless `--assume-family claude|gpt` answers it.

migrate-ids exists because a cell whose selector names no node decides
nothing: the launch falls back on the bot's own literals, so the routing AND
the effort the cell carried go inert without a word. It maps, per bot and
per the topology the integration RAN (its launch_vars: the node it ran as
its reviewer becomes `reviewer`, the other `reviewer_alternate`), review-pr's
reviewer_claude/reviewer_gpt/converge_gpt and their glance variants, and
evolve's review_claude/review_gpt; where the reviewer crosses from one route
to the other (mono-claude), review-pr's two pacer cells swap with it. It
drops the cells no node takes any more (the alternate's glance, the `#glm`
routes of nodes on claw). Where two cells land on one selector, the one kept
describes the merge the integration ran: the gpt twin's on mono-gpt, the
claude `converge`'s everywhere else. Every other cell, its order and its
fields stay as stored.

The catalogue serves ONE review-pr version to every integration, so the roll
that ships the rename moves them all at once, and a block spelled for either
side is inert on the other. Hence two phases:

  --stage  before the roll: the role cells are written BESIDE the retired
           ones, so both bundles find theirs while the fleet moves (a
           selector the running bundle does not declare warns at launch and
           pins nothing).
  --prune  after the roll: the block becomes the role view alone.

Both start from the block as it was before the first write — the backup
`<id>.pre-migrate-ids.json` in D, which --stage (or a --prune with no
stage before it) saves, launch vars and --assume-family answer included. A
stored block that is neither that origin nor its staged form was changed by
someone else and is refused; one staged from another D is refused too.

A failure on one integration (an HTTP error, a refusal, a read-back that
differs) is reported and the command goes on with the next; the exit code is
non-zero when any failed. A PATCH the org parks for an admin's approval (202,
pending_approval) is reported as parked, not applied.
"""

import argparse
import copy
import fnmatch
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


def same_block(a, b):
    """same() over routing blocks, where an absent block and an empty one
    both say nothing."""
    return same(a or {}, b or {})


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
                    "launch_vars": integ.get("launch_vars") or {},
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


# The retired node names of ADR-169, per bot and per the topology the
# integration RAN. A bot's nodes were renamed from vendor words to roles; a
# cell still naming the old id decides nothing, so the launch falls back on
# the bot's literals — silently, with the routing and the effort both gone.
#
# Which old node becomes `reviewer` is not a property of the bot: it is
# whichever node the integration ran as its reviewer. The old default was
# mono-claude (`mono_family` unset = claude), so on most integrations the
# claude node was THE reviewer and its cell must land on `reviewer` — the
# node a single review runs — not on `reviewer_alternate`, which only a
# paired one reaches. A paired run (`review_mode: dual`, or the audit tier)
# ran both; there the bot's own literal decides (review-pr's `reviewer` is
# its gpt route, evolve's its claude one).
SLOTS = {
    "review-pr": {
        "claude": {"reviewer": "reviewer_claude", "glance": "reviewer_claude_glance"},
        "gpt": {"reviewer": "reviewer_gpt", "glance": "reviewer_gpt_glance"},
        "literal": "gpt",
        "merge": {"claude": "converge", "gpt": "converge_gpt"},
    },
    "evolve": {
        "claude": {"reviewer": "review_claude"},
        "gpt": {"reviewer": "review_gpt"},
        "literal": "claude",
    },
}

# Supervisors keep their names across the rename, and a name says which ROUTE
# its supervisor watches, not which role: review-pr's `pacer` watches the node
# on the claude route (reviewer_claude, then reviewer_alternate) and
# `pacer_gpt` the one on the gpt route (reviewer_gpt, then reviewer). Where the
# integration's reviewer moves across that line (mono-claude: reviewer_claude
# becomes `reviewer`, which `pacer_gpt` watches), the two pacer cells swap.
SUPERVISORS = {
    "review-pr": {"claude": "supervisor:pacer", "gpt": "supervisor:pacer_gpt"},
}

# Role names the old bundles never declared: a block carrying one beside a
# retired selector it renames from has been staged already.
ROLE_ONLY = {"reviewer", "reviewer_alternate", "reviewer_glance"}

# Names both vocabularies declare, on different nodes: a cell --stage writes
# there is read before the roll too (review-pr's claude merge was `converge`,
# and a pacer may watch another reviewer).
SHARED_NAMES = {
    "review-pr": {"converge", "supervisor:pacer", "supervisor:pacer_gpt"},
}

# Routes retired with their node, per bot: review-pr's `converge` and its
# reviewers read no provider hint any more, so they declare no `#glm` rescue.
# A retired route is dropped whether it is spelled on the old node or the new.
RETIRED_ROUTES = {
    "review-pr": {"converge#glm", "reviewer#glm", "reviewer_glance#glm"},
}


def ran_topology(launch_vars):
    """(primary family, paired?) the integration's launch vars ran."""
    lv = launch_vars or {}
    mode = str(lv.get("review_mode", "")).strip().lower()
    paired = mode in ("dual", "paired") or str(lv.get("review_tier", "")).strip().lower() == "audit"
    family = str(lv.get("mono_family", "")).strip().lower() or "claude"
    return family, paired


def primary_family(bot, launch_vars):
    """The family whose old node the integration ran as its reviewer."""
    family, paired = ran_topology(launch_vars)
    primary = SLOTS[bot]["literal"] if paired else family
    return primary if primary in ("claude", "gpt") else "claude"


def spells_old_names(bot, routing, launch_vars):
    """Does the block name one of the bot's retired NODES (not a pacer, whose
    names both vocabularies share)? Only such a block is spelled for the
    bundle before the rename, so only its pacer cells may swap: one already
    in the role vocabulary would swap back on every run."""
    nodes, _ = rename_table(bot, launch_vars)
    for o in (routing or {}).get("overrides") or []:
        node, sep, route = o.get("selector", "").partition("#")
        b, slash, name = node.rpartition("/")
        if slash and b == bot and (name in nodes or name + sep + route in RETIRED_ROUTES.get(bot, ())):
            return True
    return False


def rename_table(bot, launch_vars, routing=None):
    """{old node: new node ("" = dropped)} for one bot under one integration,
    and the merge target whose RENAMED cell wins a collision (or None). The
    pacer swap is part of it only for a block that spells the old names
    (spells_old_names); routing=None leaves it out."""
    slots = SLOTS.get(bot)
    if not slots:
        return {}, None
    family, paired = ran_topology(launch_vars)
    primary = primary_family(bot, launch_vars)
    other = "gpt" if primary == "claude" else "claude"
    table = {
        slots[primary]["reviewer"]: "reviewer",
        slots[other]["reviewer"]: "reviewer_alternate",
    }
    sup = SUPERVISORS.get(bot)
    if sup and primary != slots["literal"] and routing is not None and spells_old_names(bot, routing, launch_vars):
        # `reviewer` sits on the literal's route, so its supervisor is the
        # literal's pacer: the cell of the pacer that watched the old reviewer
        # moves there, and the other one onto the pacer of the alternate.
        table[sup[primary]] = sup[slots["literal"]]
        table[sup[other]] = sup[primary]
    if "glance" in slots[primary]:
        table[slots[primary]["glance"]] = "reviewer_glance"
    if "glance" in slots[other]:
        table[slots[other]["glance"]] = ""  # the alternate has no glance tier
    rename_wins = None
    merge = slots.get("merge")
    if merge:
        if not paired and family == "gpt":
            # mono-gpt merged on the gpt twin: its cell is the live route.
            table[merge["gpt"]] = merge["claude"]
            rename_wins = merge["claude"]
        else:
            # Every other topology merged on the claude `converge`; the gpt
            # twin never ran, so its cell must not start deciding a route.
            table[merge["gpt"]] = ""
    return table, rename_wins


def renamed_selector(selector, launch_vars=None, routing=None):
    """(new selector, retired?) for one stored selector of the block routing
    under one integration; new is "" when no node can take the cell any
    more."""
    node, sep, route = selector.partition("#")
    bot, slash, name = node.rpartition("/")
    if not slash:
        return selector, False
    routes = RETIRED_ROUTES.get(bot, set())
    if name + sep + route in routes:
        return "", True
    nodes, _ = rename_table(bot, launch_vars, routing)
    if name not in nodes:
        return selector, False
    new = nodes[name]
    if not new:
        return "", True
    if new + sep + route in routes:
        return "", True
    return bot + "/" + new + sep + route, True


def stranded_globs(routing, launch_vars=None):
    """The glob selectors of a migrated bot that matched a retired node but
    do not match its role: `review-pr/reviewer_*` covered every reviewer slot
    and no longer covers the renamed primary `reviewer`. A glob cannot be
    rewritten mechanically — the operator decides what it should cover — so
    it is REPORTED, never edited, and the run does not count as migrated."""
    out = []
    for o in (routing or {}).get("overrides") or []:
        sel = o.get("selector", "")
        node = sel.partition("#")[0]
        bot, slash, name = node.rpartition("/")
        if not slash or not any(c in name for c in "*?["):
            continue
        for old, new in rename_table(bot, launch_vars, routing)[0].items():
            if new and fnmatch.fnmatchcase(old, name) and not fnmatch.fnmatchcase(new, name):
                out.append((sel, old, new))
    return out


def migrated_block(routing, launch_vars=None):
    """The block with every retired selector renamed; answer (block, changes,
    dropped), where a change is (from, to)."""
    out = copy.deepcopy(routing or {})
    dropped = []
    # Pass 1: rename every cell, remembering where each one came from.
    entries = []  # (selector, cell, renamed, original selector)
    for o in out.get("overrides") or []:
        new, retired = renamed_selector(o.get("selector", ""), launch_vars, routing)
        if not retired:
            entries.append((o["selector"], o, False, o["selector"]))
        elif not new:
            dropped.append(o["selector"])
        else:
            entries.append((new, dict(o, selector=new), True, o["selector"]))
    # Pass 2: a rename landing on a cell the block already carries leaves
    # ONE of the two — decided from the whole block, never from the stored
    # order. rename_table names the target whose renamed cell describes the
    # route the node takes now (mono-gpt's merge); everywhere else the cell already spelled
    # with the role name is the current one and the leftover is dropped.
    renamed_to = {sel for sel, _, renamed, _ in entries if renamed}
    carried = {sel for sel, _, renamed, _ in entries if not renamed}
    collisions = renamed_to & carried
    final, changes = [], []
    for sel, o, renamed, origin in entries:
        if sel in collisions:
            bot, _, node = sel.partition("#")[0].rpartition("/")
            rename_wins = rename_table(bot, launch_vars, routing)[1] == node
            if renamed != rename_wins:
                dropped.append(origin)
                continue
        if renamed:
            changes.append((origin, sel))
        final.append(o)
    out["overrides"] = final
    return out, changes, dropped


def stage_keeps_stored(routing, launch_vars=None):
    """The selectors a name-swap re-means whose OLD meaning runs before the
    roll and whose new one idles after it, so staging keeps the stored cell:
    mono-claude's `pacer` watches the reviewer until the roll and only the
    alternate after it."""
    keep = set()
    for bot, sup in SUPERVISORS.items():
        primary = primary_family(bot, launch_vars)
        if primary != SLOTS[bot]["literal"] and spells_old_names(bot, routing, launch_vars):
            keep.add(bot + "/" + sup[primary])
    return keep


def staged_block(routing, launch_vars=None):
    """The role view WITH the retired cells kept, for the window in which
    the catalogue moves (--stage, before the roll): a bundle from before the
    rename finds its retired selectors, one from after finds the roles, and a
    selector neither bundle declares warns at launch and pins nothing.

    The one name both vocabularies give a different node is review-pr's
    `converge`: on mono-gpt it takes the gpt twin's cell (the node it names
    after the roll merges every review; the claude merge it named before only
    ran on the audit tier). A swapped pacer keeps its stored cell where its
    old meaning is the one that runs (stage_keeps_stored)."""
    role, _, _ = migrated_block(routing, launch_vars)
    stored = (routing or {}).get("overrides") or []
    by_sel = {o.get("selector", ""): o for o in stored}
    keep = stage_keeps_stored(routing, launch_vars)
    final = []
    for o in role.get("overrides") or []:
        sel = o["selector"]
        if sel in keep:
            if sel in by_sel:
                final.append(copy.deepcopy(by_sel[sel]))
            continue
        final.append(o)
    taken = {o["selector"] for o in final}
    for o in stored:
        sel = o.get("selector", "")
        if sel in taken:
            continue
        if renamed_selector(sel, launch_vars, routing)[1]:
            final.append(copy.deepcopy(o))
            taken.add(sel)
    out = copy.deepcopy(role)
    out["overrides"] = final
    return out


def looks_staged(routing, launch_vars=None):
    """Does the block carry a role name beside a retired selector renamed to
    it — a block a --stage wrote?"""
    sels = {o.get("selector", "") for o in (routing or {}).get("overrides") or []}
    for sel in sels:
        new, retired = renamed_selector(sel, launch_vars, routing)
        if retired and new and new in sels and new.partition("#")[0].rpartition("/")[2] in ROLE_ONLY:
            return True
    return False


def carries_retired(routing, launch_vars=None):
    for o in (routing or {}).get("overrides") or []:
        if renamed_selector(o.get("selector", ""), launch_vars, routing)[1]:
            return True
    return False


def resolved_launch_vars(args, integ):
    """The integration's launch vars with the family it ran made explicit,
    or None (after an AMBIGUOUS line) when the operator must answer it.

    An unset `mono_family` is NOT evidence of claude: the engine before
    ADR-169 resolved it at launch from the credentials, so a GPT-only tenant
    ran its gpt nodes with no launch var saying so. Where the two readings
    migrate the block differently, the run refuses to guess: --assume-family
    is the operator's answer."""
    lv = dict(integ.get("launch_vars") or {})
    _, paired = ran_topology(lv)
    if paired or str(lv.get("mono_family", "")).strip():
        return lv
    if args.assume_family:
        lv["mono_family"] = args.assume_family
        return lv
    stored = integ["routing"]
    if migrated_block(stored, dict(lv, mono_family="claude"))[0] != migrated_block(stored, dict(lv, mono_family="gpt"))[0]:
        print("AMBIGUOUS %s (%s): no mono_family launch var, so the family it ran was resolved from credentials at launch — pass --assume-family claude|gpt"
              % (integ["repo"], integ["id"]))
        return None
    return lv


def describe_stage(origin, staged):
    """What --stage adds to the block: the role cells, each with the retired
    cell it copies, and the stored cells it replaces."""
    before = {o.get("selector", ""): o for o in (origin or {}).get("overrides") or []}
    added, replaced = [], []
    for o in (staged or {}).get("overrides") or []:
        sel = o["selector"]
        bot, _, name = sel.partition("#")[0].rpartition("/")
        shown = sel + (" (the pre-rename bundle reads it too)" if name in SHARED_NAMES.get(bot, ()) else "")
        if sel not in before:
            added.append(shown)
        elif not same(before[sel], o):
            replaced.append(shown)
    return added, replaced


def migrate_one(args, integ):
    """Stage or prune one integration's cells; answer True when it is done.

    The origin of both phases is the block as it was before the first write:
    the backup when one exists (its launch vars too, the --assume-family
    answer included), the stored block otherwise. --stage writes the role
    view beside the retired cells, --prune the role view alone; a stored
    block that is neither the origin nor what this procedure wrote from it
    was changed by someone else and is refused, not overwritten."""
    integ = dict(integ, routing=stored_routing(integ["team_id"], integ["id"]))
    stored = integ["routing"]
    backup = os.path.join(args.dir, integ["id"] + ".pre-migrate-ids.json")
    kept = None
    if os.path.exists(backup):
        with open(backup) as f:
            kept = json.load(f)
    if kept is not None:
        origin, lv = kept.get("routing"), dict(kept.get("launch_vars") or {})
    else:
        lv = resolved_launch_vars(args, integ)
        if lv is None:
            return False
        origin = stored
        if looks_staged(stored, lv):
            # Staged from another --dir: the block it started from is in that
            # dir's backup, and deriving one from the staged block would lose
            # the cells a pacer swap set aside.
            raise Refusal("%s (%s) carries role cells beside the retired ones — it was staged with another --dir; pass that one"
                          % (integ["repo"], integ["id"]))
    role, changes, dropped = migrated_block(origin, lv)
    staged = staged_block(origin, lv)
    wanted = staged if args.stage else role
    globs = stranded_globs(origin, lv)
    for sel, old, new in globs:
        print("GLOB     %s (%s): `%s` matched the retired `%s` but not its role `%s` — rewrite it by hand"
              % (integ["repo"], integ["id"], sel, old, new))
    if args.stage and same_block(stored, role) and not same_block(role, origin):
        print("unchanged %s (%s): already migrated — --stage has nothing left to add" % (integ["repo"], integ["id"]))
        return not globs
    if same_block(stored, wanted):
        if globs:
            return False
        if not changes and not dropped:
            print("unchanged %s (%s): no retired selector" % (integ["repo"], integ["id"]))
        else:
            print("unchanged %s (%s): already %s" % (integ["repo"], integ["id"], "staged" if args.stage else "migrated"))
        return True
    if kept is not None and not any(same_block(stored, b) for b in (origin, staged)):
        raise Refusal("%s (%s) changed since its pre-migration backup %s was saved: the stored block is neither that block nor its staged form — compare them by hand, or use a new --dir"
                      % (integ["repo"], integ["id"], backup))
    if args.stage:
        added, replaced = describe_stage(origin, staged)
        what = "add " + ", ".join(added)
        if replaced:
            what += "; replace " + ", ".join(replaced)
        what += "; keep the retired cells until --prune"
    else:
        what = ", ".join("%s -> %s" % c for c in changes) + ("" if not dropped else ", drop " + ", ".join(dropped))
    if args.dry_run:
        # A dry run touches NOTHING — not the instance, not the backup dir.
        # Its own line says what --yes would do.
        print("WOULD    %s (%s): %s" % (integ["repo"], integ["id"], what))
        return not globs
    # Its OWN backup file, never the effort procedure's `<id>.json`: that one
    # may hold an earlier state of the block, and reusing it would leave the
    # block this PATCH overwrites saved nowhere.
    if kept is None:
        record = {"team_id": integ["team_id"], "team": integ["team"], "id": integ["id"], "repo": integ["repo"],
                  "launch_vars": lv, "routing": stored}
        with open(backup, "w") as f:
            json.dump(record, f, indent=2, sort_keys=True)
            f.write("\n")
        print("saved    %s (%s) -> %s" % (integ["repo"], integ["id"], backup))
    if patch_routing(integ, wanted) == "parked":
        print("PARKED   %s (%s): the org queued the change for an admin's approval — not applied" % (integ["repo"], integ["id"]))
        return False
    if not same_block(stored_routing(integ["team_id"], integ["id"]), wanted):
        raise Refusal("the block read back is not the one written — restore it from %s" % backup)
    print("%s %s (%s): %s" % ("staged  " if args.stage else "migrated", integ["repo"], integ["id"], what))
    return not globs


def cmd_migrate_ids(args):
    # The selection is over EVERY integration, not only the ones that need it:
    # `--only <a clean one>` must answer "unchanged … no retired selector",
    # not refuse it as "not a target" — the operator asking about a name is
    # asking a question, not asserting that the name needs work.
    integs = discover()
    picked = selected(integs, args.only, args.all)
    dirty = [i for i in picked if carries_retired(i["routing"], i.get("launch_vars")) or stranded_globs(i["routing"], i.get("launch_vars"))]
    if dirty and not args.dry_run:
        os.makedirs(args.dir, mode=0o700, exist_ok=True)
    verb = ("would " if args.dry_run else "") + ("stage" if args.stage else "migrate")
    verb += "" if args.dry_run else "d"
    # Every named integration is answered, clean or not: migrate_one prints
    # its own "unchanged … no retired selector" and writes nothing.
    failed = each(picked, verb, lambda integ: migrate_one(args, integ))
    print("\n%d of %d integration(s) read carry a retired selector" % (len(dirty), len(picked)))
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
    p.add_argument("--assume-family", choices=("claude", "gpt"),
                   help="the family an integration with no mono_family launch var ran")
    p.add_argument("--only", action="append")
    p.add_argument("--all", action="store_true")
    p.add_argument("--yes", action="store_true")
    p.add_argument("--dry-run", action="store_true")
    phase = p.add_mutually_exclusive_group(required=True)
    phase.add_argument("--stage", action="store_true",
                       help="before the roll: write the role cells beside the retired ones")
    phase.add_argument("--prune", action="store_true",
                       help="after the roll: leave the role cells alone")
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
