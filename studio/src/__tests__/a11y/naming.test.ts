import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";

import { primitiveSites, repeatedRowActions, unnamedInteractives } from "./_interactive";
import { files } from "./_scanner";

// One scan for the whole file: parsing every .tsx costs ~250 ms, and both
// guards below judge the same immutable list.
const ALL = unnamedInteractives();

// RGAA 11.1's real net (#2591 leg 2): axe 4.13 passes placeholder-only
// inputs outright, so the source scan is the class-level guard for
// interactive elements with NO naming mechanism at all. Placeholder is
// deliberately not one. Surfaces whose naming is pinned by colocated
// role-name tests list their entries here with the reason — a waiver
// names a test, never a shrug.
const WAIVED: { path: string; tag: string; reason: string }[] = [
  // The shared form primitives are named by their CONSUMERS: FieldRow's
  // useId htmlFor/id pair and the enclosing-label slots decide the name at
  // each call site — undecidable from the primitive's own source, pinned by
  // the forms' colocated role-name tests (FormField/QuestionInput suites).
  { path: "/src/components/ui/Input.tsx", tag: "input", reason: "named by the consumer's FieldRow label (useId htmlFor/id)" },
  { path: "/src/components/ui/Radio.tsx", tag: "input", reason: "named by the consumer's OptionRow label" },
  { path: "/src/components/ui/Checkbox.tsx", tag: "input", reason: "named by the consumer's OptionRow label" },
  { path: "/src/components/ui/Textarea.tsx", tag: "textarea", reason: "named by the consumer's FieldRow label (useId htmlFor/id)" },
  // Select belongs in this list with its siblings: it renders
  // `<select>{children}</select>` and the children are its OPTIONS, which
  // name nothing. It was absent only because the scan credited any paired
  // interactive element for having children at all.
  { path: "/src/components/ui/Select.tsx", tag: "select", reason: "named by the consumer's FieldRow label or aria-label — its <option> children are not a name" },
  { path: "/src/components/ui/MarkdownPreview.tsx", tag: "textarea", reason: "a read-only preview pane, not a labelled control" },
  { path: "/src/components/ui/TagInput.tsx", tag: "input", reason: "named by the TagInput wrapper's own label region" },
  // MarkdownText renders markdown-provided links: the link text is the
  // markdown's own content, decided at runtime — the source scan cannot
  // see it, and MarkdownText's suite covers the rendering.
  { path: "/src/components/Runs/conversation/MarkdownText.tsx", tag: "a", reason: "link text comes from the rendered markdown itself" },
];

describe("interactive elements carry a naming mechanism (RGAA 11.1)", () => {
  it("GateInboundFile's download link names the file via aria-label (#2510)", () => {
    const src = readFileSync(
      resolve(process.cwd(), "src/components/Runs/conversation/GateInboundFile.tsx"),
      "utf8",
    );
    expect(src).toContain('aria-label={`Download ${label}`}');
  });

  it("leaves no interactive element without any naming mechanism", () => {
    const hits = ALL.filter(
      (h) => !WAIVED.some((w) => w.path === h.path && w.tag === h.tag),
    );
    if (hits.length) {
      throw new Error(
        `interactive elements with no naming mechanism (visible text, aria-label, aria-labelledby, title, or an enclosing label — placeholder is NOT one):\n${hits
          .map((h) => `  ${h.path}:${h.line}  <${h.tag}> ${h.classes}`)
          .join("\n")}`,
      );
    }
    expect(hits).toHaveLength(0);
  });

  // A waiver is a live claim about a site. One that matches nothing is a
  // sentence nobody can check, and the entry it hides behind is how a list
  // like this one stops describing the tree it guards.
  it("carries no waiver the tree no longer needs", () => {
    const dead = WAIVED.filter(
      (w) => !ALL.some((h) => h.path === w.path && h.tag === w.tag),
    );
    if (dead.length) {
      throw new Error(
        `waivers that match nothing — the element is named now, drop them:\n${dead
          .map((w) => `  ${w.path}  <${w.tag}>: ${w.reason}`)
          .join("\n")}`,
      );
    }
    expect(dead).toHaveLength(0);
  });
});

// ── The CONSUMER half of RGAA 11.1 (#2591) ──────────────────────────────
//
// `unnamedInteractives` above reads INTRINSIC tags only, and
// source-discipline.test.ts bans a raw <select> or a raw <input
// type="checkbox|radio"> outside the ui primitives — so the whole
// select/checkbox/radio naming class routes through <Input> / <Select> /
// <Textarea> / <Checkbox> / <Radio> / <TagInput>, components that scan
// cannot see. 469 call sites went unjudged, and axe cannot stand in: its
// `label` rule accepts a non-empty `placeholder` outright, which is the hole
// #2591 exists for.
//
// A name a source CAN see is read in every spelling it is written in: a
// `<label htmlFor="email">` beside an `<Input id="email">` names its control
// exactly as `htmlFor={id}` / `id={id}` does, and a `<Field label="Name">`
// whose definition — in this file or imported from the tree — renders a
// `<label>` around its children names it exactly as a written-out `<label>`
// does. A reader that took the braced pair alone would put 79
// correctly-labelled call sites on this ledger, and one that saw only the
// literal `<label>` tag 34 more — a list that describes working code is a
// list nobody can act on.
//
// This guard generalises the "VarFieldInput's callers own the name" contract
// below to all six primitives, and its failure list IS the ticket's
// remaining ledger: a surface is either fixed or declared here with a
// reasoned row, never silently skipped. The row carries the COUNT, so the
// ledger is a ratchet — a new unnamed field on an already-listed surface
// reddens it just as a new surface does.
const PRIMITIVE_SCAN = primitiveSites();

/** Why a surface is still owed, named once rather than paraphrased 40 times. */
const WHY = {
  /**
   * No naming mechanism at all: the visible label is a sibling <div>, or the
   * field is placeholder-only. The fix is a FieldRow / <label htmlFor> or an
   * aria-label on the control (#2591).
   */
  BARE: "no naming mechanism — a sibling <div> or a placeholder is not a label (#2591)",
  /**
   * The control's `id` is this component's OWN prop, so the name is owed by
   * every CALLER and no source scan of this file can decide it. Closing it
   * means a consumer guard, which is exactly the shape of the
   * "VarFieldInput's callers own the name" block below (#2591).
   */
  DELEGATED: "the id is this component's own prop — its callers owe the name, and only a consumer guard holds them (#2591)",
  /**
   * A `{...spread}` could carry the name and this reader refuses to guess in
   * either direction. VarFieldInput's call sites are already held by the
   * consumer guard below; RefAwareInput's by the forms' colocated role-name
   * suites (#2591).
   */
  SPREAD: "a {...spread} may carry the name — this reader refuses to guess; the consumer guard below and the forms' colocated suites hold these sites (#2591)",
} as const;

interface LedgerRow {
  path: string;
  /** How many of its primitive call sites are still unnamed TODAY. */
  sites: number;
  why: string;
}

const PRIMITIVE_LEDGER: LedgerRow[] = [
  { path: "/src/components/Dispatcher/SettingsDrawer.tsx", sites: 8, why: WHY.BARE },
  { path: "/src/components/DocumentSaveAs/DocumentSaveAsDialog.tsx", sites: 1, why: WHY.BARE },
  { path: "/src/components/Runs/CommitsPanel.tsx", sites: 3, why: WHY.BARE },
  { path: "/src/components/Runs/MergeConflictView.tsx", sites: 2, why: WHY.BARE },
  { path: "/src/components/Runs/PauseForm.tsx", sites: 1, why: WHY.BARE },
  { path: "/src/components/Runs/launchView/PresetSection.tsx", sites: 1, why: WHY.BARE },
  { path: "/src/components/Runs/launchView/RepoTargetSection.tsx", sites: 1, why: WHY.BARE },
  { path: "/src/components/Runs/launchView/RunSettingsSection.tsx", sites: 6, why: WHY.BARE },
  { path: "/src/components/Settings/ApiKeysEditor.tsx", sites: 1, why: WHY.BARE },
  { path: "/src/components/WhatsNext/AssistantApprovalComposer.tsx", sites: 1, why: WHY.BARE },
  { path: "/src/components/WhatsNext/HumanChatTurn.tsx", sites: 2, why: WHY.BARE },
  { path: "/src/components/WhatsNext/QuestionInput.tsx", sites: 3, why: WHY.DELEGATED },
  { path: "/src/components/WhatsNext/SessionLauncher.tsx", sites: 1, why: WHY.BARE },
  { path: "/src/components/models/ModelPicker.tsx", sites: 2, why: WHY.DELEGATED },
  { path: "/src/components/shared/AgentChatboxInline.tsx", sites: 1, why: WHY.BARE },
  { path: "/src/components/shared/VarFieldInput.tsx", sites: 7, why: WHY.SPREAD },
  { path: "/src/components/ui/RefAwareInput.tsx", sites: 2, why: WHY.SPREAD },
  { path: "/src/hooks/usePromptText.tsx", sites: 1, why: WHY.BARE },
  { path: "/src/views/Board/issueModal/PullRequestsSection.tsx", sites: 2, why: WHY.BARE },
  { path: "/src/views/Board/issueModal/TicketTab.tsx", sites: 4, why: WHY.BARE },
  { path: "/src/views/ConfigShare/index.tsx", sites: 1, why: WHY.BARE },
  { path: "/src/views/Plugins.tsx", sites: 1, why: WHY.BARE },
  { path: "/src/views/ProjectSwitcher/AddProjectDialog.tsx", sites: 1, why: WHY.BARE },
  { path: "/src/views/ProjectSwitcher/index.tsx", sites: 1, why: WHY.BARE },
  { path: "/src/views/Triggers/NewTriggerDialog.tsx", sites: 8, why: WHY.BARE },
  { path: "/src/views/Triggers/ScheduleDialogs.tsx", sites: 5, why: WHY.BARE },
  { path: "/src/views/account/ApiKeys.tsx", sites: 4, why: WHY.BARE },
  { path: "/src/views/account/SharedQuota.tsx", sites: 7, why: WHY.BARE },
  { path: "/src/views/configEditorShell/ShareFields.tsx", sites: 1, why: WHY.BARE },
  { path: "/src/views/integrations/wizard/connect/CreateGitHubAppCard.tsx", sites: 1, why: WHY.BARE },
  { path: "/src/views/integrations/wizard/connect/PatFallback.tsx", sites: 1, why: WHY.BARE },
  { path: "/src/views/plugins/PluginConfigForm.tsx", sites: 2, why: WHY.BARE },
  { path: "/src/views/teams/tabs/BindingsTab.tsx", sites: 1, why: WHY.BARE },
  { path: "/src/views/teams/tabs/CredPoolTab.tsx", sites: 2, why: WHY.BARE },
  { path: "/src/views/teams/tabs/integrations/RegisterOAuthAppForm.tsx", sites: 1, why: WHY.BARE },
  { path: "/src/views/teams/tabs/webhooks/CreateWebhookDialog.tsx", sites: 1, why: WHY.BARE },
];

describe("primitive call sites own their control's name (RGAA 11.1)", () => {
  // The population, not a re-grep of the tree: a reader that counts
  // `<Input` itself is judging a different set than these findings came from.
  it("is not vacuous — it judged every call site of the six primitives", () => {
    expect(PRIMITIVE_SCAN.judged).toBeGreaterThan(400);
  });

  it("leaves no unnamed primitive call site outside the declared ledger", () => {
    const byPath = new Map<string, typeof PRIMITIVE_SCAN.unnamed>();
    for (const site of PRIMITIVE_SCAN.unnamed) {
      const list = byPath.get(site.path) ?? [];
      list.push(site);
      byPath.set(site.path, list);
    }
    const problems: string[] = [];
    for (const [path, sites] of [...byPath.entries()].sort()) {
      const row = PRIMITIVE_LEDGER.find((r) => r.path === path);
      const where = sites
        .map((s) => `      :${s.line} <${s.tag}>${s.why === "unreadable" ? " {...spread}" : ""}`)
        .join("\n");
      if (!row) {
        problems.push(`  ${path} — not on the #2591 ledger:\n${where}`);
        continue;
      }
      if (row.sites !== sites.length) {
        problems.push(
          `  ${path} — the ledger declares ${row.sites} unnamed site(s), the tree has ${sites.length}:\n${where}`,
        );
      }
    }
    if (problems.length) {
      throw new Error(
        `primitive call sites with no name a source can see (aria-label, aria-labelledby, title, a label= prop, a <label htmlFor> in the same file, an enclosing <label> — written out, or rendered by a wrapper around its children — or an id minted by an enclosing render prop; a placeholder is NOT one).\nFix the surface, or add a reasoned row to PRIMITIVE_LEDGER:\n${problems.join("\n")}`,
      );
    }
    expect(problems).toHaveLength(0);
  });

  // A ledger row is a live claim about a surface. One that matches nothing
  // is how a list like this stops describing the tree it guards.
  it("carries no ledger row the tree no longer needs", () => {
    const flagged = new Set(PRIMITIVE_SCAN.unnamed.map((s) => s.path));
    const dead = PRIMITIVE_LEDGER.filter((r) => !flagged.has(r.path));
    if (dead.length) {
      throw new Error(
        `#2591 ledger rows that match nothing — those surfaces name their controls now, drop the rows:\n${dead
          .map((r) => `  ${r.path} (declared ${r.sites})`)
          .join("\n")}`,
      );
    }
    expect(dead).toHaveLength(0);
  });

  it("declares each surface once", () => {
    const seen = PRIMITIVE_LEDGER.map((r) => r.path);
    expect(new Set(seen).size).toBe(seen.length);
  });
});

// ── #2591 leg 1's remainder: the repeated row action ────────────────────
//
// Two controls answering to ONE accessible name — "Rotate" and "Delete" on
// every row — is the half of #2591 axe has no rule for at any level. Five
// surfaces were qualified in this ticket (Secrets, Skills, the DLQ console,
// the team SecretsTab, the Dispatcher's running table), each pinned by a
// colocated `expectUniqueAccessibleNames` sweep on the RENDERED tree. The
// rest of the class is here: every surface whose row maps still hold a
// statically-named control, with how many they hold today.
//
// "Control" is the whole population a user operates, not the five tags a
// first reading looked for: a `<Checkbox aria-label="Enable trigger">` and a
// `<TextField label="Name">` repeat per row exactly as a `<Button>Delete`
// does, and the form primitives plus the FormField wrappers are in the scan
// for that reason. The name is read in the algorithm's own precedence — the
// aria pair, the consumer's `<label>` (enclosing, through a wrapper, or by
// `htmlFor` in the file), the contents, then `title` — so "GitHub org" on
// every grant row counts, and a `title` beside a per-row content does not.
// A control this reader finds NO name for is the unnamed-control class
// above, not this one.
//
// These rows are the REMAINDER, not waivers: nothing here claims the surface
// is correct, only that the class was measured and this is what is left. A
// map can render its control for one row alone, so a row is a surface to
// AUDIT rather than a verdict — and the count is what makes the list a
// ratchet: a new row action on a listed surface reddens it exactly as a new
// surface does, and a surface leaves the list only by qualifying its
// controls.
const ROW_ACTION_SCAN = repeatedRowActions();

/** The surfaces this ticket qualified: the scan must find none of them. */
const ROW_ACTION_QUALIFIED = [
  "/src/views/Secrets.tsx",
  "/src/views/Skills.tsx",
  "/src/views/admin/DLQAdminPage.tsx",
  "/src/views/teams/tabs/SecretsTab.tsx",
  "/src/views/Dispatcher/RunningTable.tsx",
] as const;

interface RowActionRow {
  path: string;
  /** How many statically-named controls its row maps hold TODAY. */
  sites: number;
}

const ROW_ACTION_LEDGER: RowActionRow[] = [
  { path: "/src/components/Catalog/BotCatalogDialog.tsx", sites: 2 },
  { path: "/src/components/Dispatcher/SettingsDrawer.tsx", sites: 1 },
  { path: "/src/components/Panels/forms/ComputeForm.tsx", sites: 3 },
  { path: "/src/components/Panels/forms/FallbacksFields.tsx", sites: 6 },
  { path: "/src/components/Panels/SchemaEditor.tsx", sites: 2 },
  { path: "/src/components/Panels/VarsEditor.tsx", sites: 4 },
  { path: "/src/components/Panels/WorkflowSettingsForm.tsx", sites: 2 },
  { path: "/src/components/Runs/ArtifactFilesPanel.tsx", sites: 6 },
  { path: "/src/components/Runs/conversation/GateAttachments.tsx", sites: 1 },
  { path: "/src/components/Runs/launchView/AutoManagedVarsSection.tsx", sites: 2 },
  { path: "/src/components/Settings/ApiKeysEditor.tsx", sites: 2 },
  { path: "/src/components/shared/AgentChatboxInline.tsx", sites: 1 },
  { path: "/src/components/shared/Toast.tsx", sites: 1 },
  { path: "/src/views/account/ApiKeys.tsx", sites: 1 },
  { path: "/src/views/account/OAuthConnections.tsx", sites: 10 },
  { path: "/src/views/account/SettingsPage.tsx", sites: 1 },
  { path: "/src/views/account/TokensPanel.tsx", sites: 1 },
  { path: "/src/views/admin/PlatformBotsPage.tsx", sites: 1 },
  { path: "/src/views/admin/PlatformForgeAppsPage.tsx", sites: 1 },
  { path: "/src/views/admin/settings/BotRolesPage.tsx", sites: 1 },
  { path: "/src/views/admin/settings/BotVarsPage.tsx", sites: 3 },
  { path: "/src/views/admin/spend/CredentialSpendPage.tsx", sites: 1 },
  { path: "/src/views/admin/UsersAdminPage.tsx", sites: 7 },
  { path: "/src/views/Board/Fields.tsx", sites: 4 },
  { path: "/src/views/Board/Labels.tsx", sites: 3 },
  { path: "/src/views/integrations/ConnectionsTab.tsx", sites: 2 },
  { path: "/src/views/Marketplace/ModerationQueue.tsx", sites: 2 },
  { path: "/src/views/orgs/GovernanceTab.tsx", sites: 2 },
  { path: "/src/views/orgs/OrgPage.tsx", sites: 2 },
  { path: "/src/views/ProjectSwitcher/index.tsx", sites: 1 },
  { path: "/src/views/RepoDetail/index.tsx", sites: 3 },
  { path: "/src/views/SettingsDialog/ProjectsTab.tsx", sites: 3 },
  { path: "/src/views/teams/tabs/BindingsTab.tsx", sites: 4 },
  { path: "/src/views/teams/tabs/integrations/OAuthAppsSection.tsx", sites: 3 },
  { path: "/src/views/teams/tabs/sso/DomainsSection.tsx", sites: 3 },
  { path: "/src/views/teams/tabs/sso/GitHubSection.tsx", sites: 4 },
  { path: "/src/views/teams/tabs/sso/KeycloakSection.tsx", sites: 6 },
  { path: "/src/views/teams/tabs/webhooks/CreateWebhookDialog.tsx", sites: 1 },
  { path: "/src/views/teams/tabs/WebhooksTab.tsx", sites: 3 },
  { path: "/src/views/teams/TeamPage.tsx", sites: 2 },
  { path: "/src/views/Triggers/ScheduleDialogs.tsx", sites: 1 },
  { path: "/src/views/Triggers/SchedulesTab.tsx", sites: 2 },
  { path: "/src/views/Triggers/TriggerList.tsx", sites: 3 },
  { path: "/src/views/Welcome/steps/CliCheck.tsx", sites: 1 },
];

describe("a row map's repeated action names the row it acts on (#2591)", () => {
  // The population, not a re-grep of the tree: a reader that counts
  // `<Button` itself is judging a different set than these findings came from.
  it("is not vacuous — it judged every row control of the tree", () => {
    expect(ROW_ACTION_SCAN.judged).toBeGreaterThan(250);
  });

  it("leaves no statically-named row action outside the declared remainder", () => {
    const problems: string[] = [];
    for (const surface of ROW_ACTION_SCAN.surfaces) {
      const row = ROW_ACTION_LEDGER.find((r) => r.path === surface.path);
      const where = surface.sites
        .map(
          (s) =>
            `      :${s.line} <${s.tag}> ${JSON.stringify(s.name)}${s.unreadable ? " {...spread}" : ""}`,
        )
        .join("\n");
      if (!row) {
        problems.push(`  ${surface.path} — not on the #2591 row-action ledger:\n${where}`);
        continue;
      }
      if (row.sites !== surface.sites.length) {
        problems.push(
          `  ${surface.path} — the ledger declares ${row.sites} site(s), the tree has ${surface.sites.length}:\n${where}`,
        );
      }
    }
    if (problems.length) {
      throw new Error(
        `controls inside a row map whose accessible name is the same on every row — every row announces one name, and the user hears which one nowhere.\nQualify the control (the name carries the row, the visible word stays first), or add its surface to ROW_ACTION_LEDGER:\n${problems.join("\n")}`,
      );
    }
    expect(problems).toHaveLength(0);
  });

  // A remainder row is a live claim about a surface. One that matches
  // nothing is how a list like this stops describing the tree it measures.
  it("carries no remainder row the tree no longer needs", () => {
    const found = new Set(ROW_ACTION_SCAN.surfaces.map((s) => s.path));
    const dead = ROW_ACTION_LEDGER.filter((r) => !found.has(r.path));
    if (dead.length) {
      throw new Error(
        `#2591 row-action rows that match nothing — those surfaces qualify their row controls now, drop the rows:\n${dead
          .map((r) => `  ${r.path} (declared ${r.sites})`)
          .join("\n")}`,
      );
    }
    expect(dead).toHaveLength(0);
  });

  it("declares each surface once", () => {
    const seen = ROW_ACTION_LEDGER.map((r) => r.path);
    expect(new Set(seen).size).toBe(seen.length);
  });

  // The other direction of the ratchet: the five surfaces this ticket
  // qualified answer to the scan too, so a row action going back to a static
  // name reddens HERE as well as in its colocated sweep.
  it("holds the surfaces this ticket qualified to their row-carrying names", () => {
    // A path nothing in the tree answers to would make the claim below
    // unfalsifiable — a renamed surface has to redden here, not go quiet.
    const known = new Set(files.map(([path]) => path));
    expect(ROW_ACTION_QUALIFIED.filter((p) => !known.has(p))).toEqual([]);
    const regressed = ROW_ACTION_SCAN.surfaces.filter((s) =>
      (ROW_ACTION_QUALIFIED as readonly string[]).includes(s.path),
    );
    if (regressed.length) {
      throw new Error(
        `surfaces #2591 qualified whose row controls carry a static name again:\n${regressed
          .map((s) => `  ${s.path}: ${s.sites.map((x) => `:${x.line} ${JSON.stringify(x.name)}`).join(", ")}`)
          .join("\n")}`,
      );
    }
    expect(regressed).toHaveLength(0);
  });
});

// VarFieldInput renders a bare control for every var type and leaves the name
// to its caller's `<label htmlFor>` — the `id` prop's contract. `id` stays
// optional in the props (a control inside an enclosing label needs none), so
// the contract is a guard here rather than a type: a `bool` var whose caller
// forgot it renders a checkbox announcing nothing, and the rendered nets only
// cover the surfaces that happen to carry an axe entry.
describe("VarFieldInput's callers own the name (RGAA 11.1)", () => {
  it("hands every call site's id to a label in the same file", () => {
    const problems: string[] = [];
    let sites = 0;
    for (const [path, src] of files) {
      const opened = src.match(/<VarFieldInput\b/g)?.length ?? 0;
      if (opened === 0) continue;
      const tags = src.match(/<VarFieldInput\b[\s\S]*?\/>/g) ?? [];
      if (tags.length !== opened) {
        // A paired `<VarFieldInput>…</VarFieldInput>` would slip past the
        // reader below, so the shape fails instead of going unread.
        problems.push(`${path}  a <VarFieldInput> is not self-closing — this guard cannot read it`);
        continue;
      }
      for (const tag of tags) {
        sites += 1;
        // The match runs to the FIRST `/>`, so any nested JSX — a child under
        // a paired form, a `prop={<Icon />}` — means the span read here is
        // not the call site's own attributes. Such a shape fails rather than
        // being read wrong in either direction.
        if (tag.slice("<VarFieldInput".length).includes("<")) {
          problems.push(`${path}  a <VarFieldInput> carries nested JSX — this guard cannot read it`);
          continue;
        }
        // Anchored: `\b` makes `data-row-id={rowId}` an id, and a call site
        // with no id of its own would then be credited with another
        // attribute's expression.
        const id = /(?<![\w-])id=\{(`[^`]*`|[^}]*)\}/.exec(tag)?.[1];
        if (!id) {
          problems.push(`${path}  <VarFieldInput> with no id — nothing can point a label at it`);
          continue;
        }
        if (!src.includes(`htmlFor={${id}}`)) {
          problems.push(`${path}  <VarFieldInput id={${id}}> — no <label htmlFor={${id}}> in this file`);
        }
      }
    }
    if (problems.length) {
      throw new Error(
        `VarFieldInput call sites whose control has no name:\n${problems.join("\n")}`,
      );
    }
    // Non-vacuous: the call sites exist and were read.
    expect(sites).toBeGreaterThanOrEqual(4);
  });
});
