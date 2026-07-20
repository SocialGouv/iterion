/**
 * ConfigEditorShell — the limited studio surface for the least-privilege
 * `config_editor` team role.
 *
 * A config_editor is a real, cookie-authenticated team member who may do
 * exactly ONE thing: edit their team's config-shares. They get no Sidebar, no
 * runs/board/launch — just this shell (modeled on RestrictedShell): the brand
 * header + a config-editor workspace.
 *
 * The network layer is @/api/configEditor, which rides the SHARED cookie
 * client (credentials: "include"). This is deliberately NOT the isolated
 * @/api/configShare (iws_-token, credentials:"omit") used by the anonymous
 * /config/:id editor — that boundary is eslint-enforced under
 * src/views/ConfigShare/**; this signed-in shell lives outside it.
 *
 * The field-editing UX mirrors the proven ConfigShare editor: a plain
 * <textarea> for string fields (no markdown preview), a text-input list for
 * array fields (feeds), Save that sends {sha, patch}, and a 409 conflict flow
 * that shows "yours vs theirs" and NEVER auto-clobbers. Unlike ConfigShare
 * (hard-wired to feeds+editorial), this shell renders whatever editable string
 * / string-array leaves the server's projected config contains.
 */

import { useState } from "react";

import { useAuth } from "@/auth/AuthContext";
import { BrandMark } from "@/components/ui/BrandMark";
import { ThemeToggle } from "@/components/ui/ThemeToggle";
import { BrandWordmark, Button, InlineBanner } from "@/components/ui";

import type { Branding } from "./configEditorShell/fieldModel";
import { Workspace } from "./configEditorShell/Workspace";

// ---------------------------------------------------------------------------
// Shell chrome — brand header + Sign out, matching RestrictedShell.
// ---------------------------------------------------------------------------

export default function ConfigEditorShell() {
  const { user, signOut, activeTeamID, activeTeam } = useAuth();
  const [branding, setBranding] = useState<Branding>({});
  const title = branding.title || "Config editor";
  return (
    <div className="flex h-screen min-h-0 flex-col bg-surface-0 text-fg-default">
      <header className="flex items-center justify-between border-b border-border-subtle px-4 py-3 sm:px-6">
        <div className="flex items-center gap-2.5">
          <BrandMark className="h-7 w-7" />
          <BrandWordmark />
          <span aria-hidden className="text-fg-subtle">
            /
          </span>
          <span className="text-sm font-medium text-fg-default">{title}</span>
        </div>
        <div className="flex items-center gap-2 sm:gap-3">
          <ThemeToggle />
          <span className="hidden text-xs text-fg-muted sm:inline">{user?.email}</span>
          <Button variant="secondary" size="sm" onClick={() => void signOut()}>
            Sign out
          </Button>
        </div>
      </header>

      <div className="min-h-0 flex-1 overflow-auto">
        <main className="mx-auto w-full max-w-5xl px-4 py-6 sm:px-6">
          {activeTeamID ? (
            <Workspace
              teamID={activeTeamID}
              teamName={activeTeam?.team_name}
              onBranding={setBranding}
            />
          ) : (
            <InlineBanner tone="warning" layout="inline" title="No active team">
              Your account has no active team, so there are no config-shares to edit.
              Ask an administrator to add you to a team.
            </InlineBanner>
          )}
        </main>
      </div>
    </div>
  );
}
