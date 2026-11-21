// Extracted from LaunchView.tsx to keep that file focused.
// useLaunchDoc loads the workflow document behind the launch form — a named
// bot's program as the server resolves it, the ?file= path, otherwise the
// in-memory editor buffer — and owns the var-values form state plus the field
// buckets derived from the parsed document.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { getBotLaunchSource } from "@/api/bots";
import * as filesApi from "@/api/client";
import { errorMessage } from "@/lib/errorHints";
import type { IterDocument, Preset } from "@/api/types";

import { defaultStringFor } from "@/components/shared/VarFieldInput";
import {
  launchRefusal,
  namedProgramRefusal,
  pristineBuffer,
  unitRefusal,
} from "@/lib/launch";
import { useDocumentStore } from "@/store/document";

import { type LLMNode } from "./ModelOverridesSection";
import { pickAttachments, pickVars } from "./utils";

/** What useLaunchDoc loads besides a ?file= path or the editor buffer. */
export interface LaunchDocTarget {
  /** A registry bot launched BY NAME (cloud): its program is read from the
   *  server's own resolution, and the launch posts its id and the digest that
   *  read issued — never the text. Excludes a `filePath`. */
  bot?: string;
  /** The launch's target is not known yet (a `?bot=` slug still being
   *  resolved): nothing is loaded and nothing is offered, so the editor's
   *  buffer is never rendered under a bot's URL meanwhile. */
  pending?: boolean;
}

/** One landing of a load: the document the form describes, tagged with the
 *  target it was asked for. `presets` rides the same tag for a named bot — the
 *  preset set the server read with the program, which the digest covers. */
export interface LoadedDoc {
  path: string;
  bot: string;
  doc: IterDocument;
  presets?: Preset[];
}

export function useLaunchDoc(
  filePath: string,
  onError: (msg: string) => void,
  { bot: namedBot = "", pending = false }: LaunchDocTarget = {},
) {
  // The document THIS launch is about, tagged with the target it was loaded
  // for — the `filePath` and the named bot, "" and "" for the inline buffer
  // — like the source below, and for the same reason: the form's
  // fields, their values and the required-var gate all derive from it, so an
  // untagged answer describes the workflow the view has moved on FROM while
  // the submit names the new one. Unlike the source it is not dropped at the
  // START of a load: the inline branch reloads whenever the active editor tab
  // changes (its tag is always "", so it cannot tell two buffers apart), and
  // blanking the form for that round trip would be worse than holding the
  // last render.
  //
  // A load that ends with NO answer does drop it — a pristine buffer, a
  // refusal, a failed read or a failed render — because nothing retries it:
  // the effect re-runs only when its deps move. Left in place, the form stays
  // armed (`docReady` is `!!doc`) over a program the submit cannot carry.
  //
  const [loadedDoc, setLoadedDoc] = useState<LoadedDoc | null>(null);
  // The in-memory editor buffer, used to launch an UNSAVED workflow (no
  // ?file= path). This is the cloud path: the server pod's rootfs is
  // read-only, so /files/save 500s and there is never an on-disk path to
  // reference — the launch API takes inline `source` instead (see
  // resolveWorkflowPath: cloud mode returns the empty file_path and runs
  // off Source). Also lets a fresh local buffer launch before its first
  // save.
  const storeDocument = useDocumentStore((s) => s.document);
  // Pristine-buffer detection: the document store initializes with a
  // default scaffold document (createEmptyDocument), so `storeDocument`
  // is never null — a bare deep-link to /runs/new would otherwise
  // silently offer launching that implicit scaffold as an "Unsaved
  // workflow". A buffer only counts as a real launch candidate once the
  // user opened a file (currentFilePath set) or edited the scaffold
  // (generation moved past the last-saved mark).
  const pristine = useDocumentStore(pristineBuffer);
  const noSource = !filePath && !namedBot && !pending && pristine;
  // Why the buffer cannot be launched inline — the predicate behind the
  // toolbar's Run button, so this view refuses exactly what that disables.
  const refusal = useDocumentStore(launchRefusal);
  // The editor's buffer is the INLINE road's program and nobody else's. A
  // named bot and a ?file= launch read the server, so nothing that happens in
  // an editor tab — switching it, editing it, closing it — is a reason to
  // read them again: a load resets the form to its declared defaults, and a
  // reset the operator did not ask for takes away what they typed, including
  // the options folded out of sight.
  const inlineRoad = !filePath && !namedBot && !pending;
  const buffer = inlineRoad ? storeDocument : null;
  const bufferRefusal = inlineRoad ? refusal : null;
  const [confirmedDiskPath, setConfirmedDiskPath] = useState<string | null>(null);
  // The text THIS launch sends, kept here and never written into the active
  // editor tab's store — which this route reads. Neither branch's text is
  // that tab's: a ?file= launch is about a file the tab may not hold, and an
  // inline launch's text is the RENDER of the tab's document, which is not
  // what the tab holds either. Written there, it becomes the tab's own
  // text: what its salvage view shows as the file, and what a draft tab
  // compares to know the canvas is still the assistant's.
  //
  // `path` and `bot` are the target the answer was asked for ("" and "" for
  // the inline buffer), so the text of a launch this view has moved on from
  // is never read as the current one's program: this hook outlives one
  // launch — `/runs/new` keeps it while the URL moves from one workflow to
  // another, and `?bot=<slug>` resolves to its target only once the server
  // info or the catalog answers (LaunchView).
  const [launchSource, setLaunchSource] = useState<{
    path: string;
    bot: string;
    source: string;
  } | null>(null);
  // A named bot's program as the server issued it: the canonical id and the
  // digest of the source this form renders. It is what the launch posts in
  // place of the text, tagged with the slug it was read for like the source,
  // and dropped at the start of every load — a digest is never carried from
  // one read to the next.
  const [botProgram, setBotProgram] = useState<{
    bot: string;
    id: string;
    sha256: string;
  } | null>(null);
  // Bumped by `reload`: the operator asks for the program again (the launch
  // was refused because the bot changed under the form).
  const [reloads, setReloads] = useState(0);
  const reload = useCallback(() => setReloads((n) => n + 1), []);
  const [values, setValues] = useState<Record<string, string>>({});
  // A load LANDS here and nowhere else: its document and the form's values,
  // reset to that document's declared defaults, are set together. The wrapper
  // is made anew by every landing — the same document object handed back by a
  // later load is still another landing — so it is the identity of "the
  // values were reset" (`load`, below): what must not outlive a reset keys on
  // it, and cannot disagree with the values about when one happened.
  const land = useCallback((loaded: LoadedDoc) => {
    setLoadedDoc(loaded);
    const initial: Record<string, string> = {};
    for (const f of pickVars(loaded.doc)) initial[f.name] = defaultStringFor(f);
    setValues(initial);
  }, []);

  // Every dep that changes identity per render re-runs the load, so an
  // inline onError would turn it into an unparse-per-render loop. The
  // callback is invoked, never captured: the latest one reaches the effect
  // through a ref.
  const onErrorRef = useRef(onError);
  useEffect(() => {
    onErrorRef.current = onError;
  }, [onError]);

  useEffect(() => {
    // A confirmation belongs to one exact ?file= load. Never carry it over
    // to an inline editor buffer or to a different path while async reads
    // are in flight.
    setConfirmedDiskPath(null);
    setLaunchSource(null);
    setBotProgram(null);
    if (pending) {
      // No target yet: nothing to load, and no document to keep — the last
      // one belongs to a launch this view has moved on from.
      setLoadedDoc(null);
      return;
    }
    if (namedBot) {
      // A bot launched by name: the form renders the program the SERVER
      // resolves for that name — the tier a launch naming it runs — and the
      // launch posts the id and the digest this read issued, so the server
      // refuses it if the bot changed in between. The text is rendered and
      // priced here, never posted.
      let cancelled = false;
      getBotLaunchSource(namedBot)
        .then((res) => {
          if (cancelled) return;
          // Same refusals as the two other roads: a salvage is refused as
          // the editor's buffer is, the unit this answer carries as the
          // ?file= road refuses it — and a program with no digest is no
          // program to pin, said as such rather than left "being prepared".
          const refused =
            namedProgramRefusal(res) ??
            unitRefusal(res.unit) ??
            (res.bot_id && res.sha256
              ? null
              : "The server answered this bot's program without the id and digest a launch from this form is pinned to, so it cannot be launched here. Reload; if it persists the server is older than this studio — launch the bot by id from the CLI.");
          if (refused) {
            setLoadedDoc(null);
            onErrorRef.current(refused);
            return;
          }
          land({
            path: "",
            bot: namedBot,
            doc: res.document,
            presets: res.presets?.entries ?? [],
          });
          setLaunchSource({ path: "", bot: namedBot, source: res.source });
          setBotProgram({ bot: namedBot, id: res.bot_id, sha256: res.sha256 });
        })
        .catch((e) => {
          if (cancelled) return;
          // Same rule as the other branches: a read that failed answers for
          // this bot too, and its answer is nothing. A refusal is said in the
          // server's own sentence — who may read this bot's program, and that
          // it can still be launched by id — not in the transport's wrapping.
          setLoadedDoc(null);
          onErrorRef.current(
            e instanceof filesApi.ApiError && e.detail ? e.detail : errorMessage(e),
          );
        });
      return () => {
        cancelled = true;
      };
    }
    if (!filePath) {
      // No ?file= path — launch the unsaved editor buffer via inline
      // source. The launch API (resolveWorkflowPath) runs off Source when
      // file_path is empty, so this is a first-class path, not a fallback
      // hack; it is the ONLY way to launch in cloud mode, where the pod
      // rootfs is read-only and a workflow can never be saved to disk.
      //
      // The live edit buffer is the document store's AST (currentSource is
      // only the last opened/saved file's text, stale after edits), so we
      // derive the source from it via /api/unparse before launching.
      //
      // A pristine store buffer (never opened, never edited) is NOT a
      // launchable workflow — the render below shows a picker empty
      // state instead, so don't unparse the scaffold here.
      if (noSource) {
        setLoadedDoc(null);
        return;
      }
      // A salvaged buffer is the program MINUS the region the parser could
      // not read. Launching it loses no bytes — it RUNS a workflow the
      // author never wrote, at real cost, and the run's report gives no
      // sign of what is missing. The harshest of the six sites that hand
      // the document out as the program, so the same refusal covers it; a
      // document with error diagnostics is refused here too, before the
      // form, with the count the server would refuse it with after.
      if (bufferRefusal) {
        // The store keeps living while the form is up (this route reads the
        // active tab's store): a refusal that arrives after the form
        // mounted clears it, instead of leaving a refused document
        // launchable under the error banner.
        setLoadedDoc(null);
        onErrorRef.current(bufferRefusal);
        return;
      }
      // Narrowing only: launchRefusal already refused a missing document.
      if (!buffer) return;
      let cancelled = false;
      filesApi
        .unparse(buffer)
        .then(({ source: src }) => {
          if (cancelled) return;
          setLaunchSource({ path: filePath, bot: "", source: src });
          land({ path: filePath, bot: "", doc: buffer });
        })
        .catch((e) => {
          if (cancelled) return;
          // No render, no program: the form goes back to loading and the
          // banner carries the cause, instead of the submit gate speaking
          // for a failure it did not cause.
          setLoadedDoc(null);
          onErrorRef.current(errorMessage(e));
        });
      return () => {
        cancelled = true;
      };
    }
    let cancelled = false;
    filesApi
      .openFile(filePath)
      .then((res) => {
        if (cancelled) return;
        // A bot in several files is launched from its files as they are
        // stored, and this road reads them: files that do not parse, or do
        // not load as one bot, are what the launch refuses once the form is
        // filled in. The refusal is the toolbar's, on the unit this answer
        // carries — the roads that land here (the command palette, a repo's
        // bot list, the bind wizard, a bot's home) pass through no editor,
        // so the gate the button holds closed would not be consulted at all.
        const refusedUnit = unitRefusal(res.unit);
        if (refusedUnit) {
          setLoadedDoc(null);
          onErrorRef.current(refusedUnit);
          return;
        }
        land({ path: filePath, bot: "", doc: res.document });
        setLaunchSource({ path: filePath, bot: "", source: res.source });
        setConfirmedDiskPath(res.confirmed_disk_path ?? null);
      })
      .catch((e) => {
        if (cancelled) return;
        // Same rule as the inline branch: a read that failed answers for
        // this path too, and its answer is nothing. The tag alone would not
        // catch it — a reload of the SAME path still matches.
        setLoadedDoc(null);
        onErrorRef.current(errorMessage(e));
      });
    return () => {
      cancelled = true;
    };
  }, [filePath, namedBot, pending, reloads, noSource, bufferRefusal, buffer, land]);

  // An answer is this launch's own only when it was asked for this target.
  const current = (t: { path: string; bot: string } | null): boolean =>
    !pending && t !== null && t.path === filePath && t.bot === namedBot;
  // The landing this form shows, null between two of them.
  const load = loadedDoc && current(loadedDoc) ? loadedDoc : null;
  const doc = load ? load.doc : null;
  // The presets the server read with a named bot's program; null on every
  // other road, where the document's own `presets:` block is all there is.
  const namedPresets =
    namedBot && loadedDoc && current(loadedDoc) ? (loadedDoc.presets ?? []) : null;

  // The full declared field list. Progressive-disclosure bucketing
  // (primary / bot options / auto) happens in LaunchView via
  // applyLaunchHints, because the split also folds in the resolved bot's
  // launch hints — which this hook cannot see (the bot entry is matched
  // by useBotPresets, downstream of the doc loaded here).
  const fields = pickVars(doc);

  const attachmentFields = pickAttachments(doc);

  // LLM nodes (agents + judges) the operator can retarget per run. Names are
  // the exact node ids used as override selectors. Judges first so the review
  // side reads top-down in the section.
  const llmNodes = useMemo<LLMNode[]>(() => {
    const judges: LLMNode[] = (doc?.judges ?? []).map((j) => ({
      name: j.name,
      kind: "judge",
      model: j.model,
      backend: j.backend,
    }));
    const agents: LLMNode[] = (doc?.agents ?? []).map((a) => ({
      name: a.name,
      kind: "agent",
      model: a.model,
      backend: a.backend,
    }));
    return [...judges, ...agents];
  }, [doc]);

  // Surface the worktree config so the user knows whether the
  // finalization fields will have any effect. Only the first workflow
  // is inspected (matches pickVars's selection rule).
  const worktreeMode = doc?.workflows?.[0]?.worktree ?? "";
  const worktreeOn = worktreeMode === "auto";

  return {
    doc,
    load,
    noSource,
    currentSource: launchSource && current(launchSource) ? launchSource.source : null,
    // The named bot's pinned program, null on every other road and until
    // its read lands.
    namedBot:
      botProgram && !pending && !filePath && botProgram.bot === namedBot
        ? { id: botProgram.id, sha256: botProgram.sha256 }
        : null,
    namedPresets,
    reload,
    confirmedDiskPath,
    values,
    setValues,
    fields,
    attachmentFields,
    llmNodes,
    worktreeOn,
  };
}
