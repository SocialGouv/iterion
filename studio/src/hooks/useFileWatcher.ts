import { useEffect, useRef } from "react";
import { stampEditor, stampHolds, useDocumentStoreInstance } from "@/store/document";
import { changedExternally, offerReload } from "@/lib/reloadOffer";
import { useUIStore } from "@/store/ui";
import { fileWatcher } from "@/api/ws";
import * as api from "@/api/client";
import { errorMessage } from "@/lib/errorHints";
import { touchesOpenUnit, unitAwaitsFragment } from "@/hooks/unitFiles";
import { applyOpenedFile } from "@/lib/openedFile";
import type { FileEvent, ServerWsEvent } from "@/api/types";

const RELOAD_DEBOUNCE_MS = 500;

// useFileWatcher subscribes the surrounding editor subtree to the
// server's file-changed stream. Multiple editor tabs each call this
// hook from their own EditorTabHost subtree, so each one only acts on
// events for the file it owns. The per-tab document store is captured
// via the Context-resolved instance — i.e. each watcher writes back
// to its own store, never to a sibling tab's.
export function useFileWatcher() {
  const reloadTimerRef = useRef<ReturnType<typeof setTimeout>>(undefined);
  // Capture the tab's document store. Stable across renders for one
  // tab mount; refreshed via useRef so the WS callback sees the same
  // identity even if Context shifts mid-flight.
  const docStore = useDocumentStoreInstance();
  const docStoreRef = useRef(docStore);
  docStoreRef.current = docStore;

  useEffect(() => {
    fileWatcher.connect();
    const deferred = new Set<() => void>();
    // An event that arrives while an explicit replacement is pending is
    // re-read once that replacement ends, against the tab as it left it:
    // a replacement that failed, was refused, or read the file before this
    // write leaves the tab on the file the event is about.
    const afterPending = (event: FileEvent) => {
      const unsub = docStoreRef.current.subscribe((s) => {
        if (s._pendingIntent !== null) return;
        unsub();
        deferred.delete(unsub);
        handle(event);
      });
      deferred.add(unsub);
    };

    const onEvent = (event: ServerWsEvent) => {
      if (event.type !== "file_created" && event.type !== "file_modified" && event.type !== "file_deleted") {
        return;
      }
      if (event.type === "file_created" || event.type === "file_deleted") {
        useUIStore.getState().notifyFilesChanged();
      }
      handle(event);
    };

    const handle = (event: FileEvent) => {
      const store = docStoreRef.current.getState();
      // An explicit replacement is in flight: the event waits for it to end
      // — an event about the very file being opened included, which the path
      // below does not name yet — and is then read against the tab as it
      // was left.
      if (store._pendingIntent !== null) {
        afterPending(event);
        return;
      }
      const { addToast } = useUIStore.getState();
      const filePath = store.currentFilePath;
      // An open Source-view edit counts, dirty or not: an auto-reload would
      // swap the document and the revision under what the author is typing,
      // and the Apply that follows would then land on top of whoever wrote
      // the file meanwhile. `hasUnsavedWork()` is the narrower question —
      // it does not hold for an editor opened and not yet typed in — so the
      // gate here stays the buffer's PRESENCE.
      const dirty = store.isDirty() || !!store.sourceBuffer;

      // An event is about the open document when it names the file itself,
      // or — for a bot in several files — one of the fragments its imports
      // reach: the merged document and the revision a save presents follow
      // either. A fragment the load TRIED to read and could not is a file of
      // no list, and the gate refuses on that load (#1532), so it counts too:
      // writing it is the repair the refusal asks for. The answer names
      // those (`unread`) — an import it refused by path names none.
      const aboutTheOpenUnit = (path: string) =>
        !!filePath && (touchesOpenUnit(path, filePath, store.unit) || unitAwaitsFragment(path, store.unit));

      const name = (path: string) => path.split("/").pop() ?? path;

      // What an offer about this event SAYS: the file that changed, which
      // is not always the one the offer would reload. A write to a
      // fragment of the open unit is a change to the open document — that
      // is why the offer is raised at all — and naming the open file over
      // it asks the author to discard their work on the strength of a
      // statement about a file nobody touched. Named the way the deletion
      // warning names it.
      const changeOffer = (targetPath: string) =>
        event.path === targetPath
          ? changedExternally(event.path)
          : `${changedExternally(event.path)} — it is one of this bot's files`;

      // The AUTOMATIC reload: only while nothing of the author's could be
      // lost, read again at the answer — the round trip is a window in
      // which they can open a Source-view edit, and `applyOpenedFile`
      // drops the buffer. It stands down while an explicit replacement
      // is pending: that request speaks for the author, and a background
      // answer landing first would make its answer look refused for
      // edits nobody made. Through the shared helper: a reload of a file
      // that stopped parsing must mark the document a SALVAGE too, and
      // the revision a save presents follows the files on disk.
      const autoReload = (path: string) => {
        const asked = stampEditor(docStoreRef.current.getState());
        api
          .openFile(path)
          .then((result) => {
            const st = docStoreRef.current.getState();
            if (st.currentFilePath !== path) return;
            if (st._pendingIntent !== null) {
              afterPending({ type: "file_modified", path: event.path });
              return;
            }
            if (st.isDirty() || !!st.sourceBuffer) {
              offerReload(docStoreRef.current, path, changeOffer(path));
              return;
            }
            // The document moved while the file was read — an edit that
            // was saved since, another reload: what was read may be older
            // than what the tab shows, so the file is read again.
            if (!stampHolds(asked, st).generation) {
              handle({ type: "file_modified", path: event.path });
              return;
            }
            applyOpenedFile(result, st);
            useUIStore.getState().addToast("File reloaded", "info");
          })
          .catch((err) => {
            console.error("Failed to reload file:", err);
            // A silently dropped reload is worse than a sticky,
            // recoverable error that names the file.
            useUIStore.getState().addToast(`Failed to reload ${name(path)}: ${errorMessage(err)}`, "error", {
              persistent: true,
              action: { label: "Retry", onClick: () => autoReload(path) },
            });
          });
      };

      // The read an event about the open unit asks for: what the tab holds
      // of these files — the document, the revision a save presents, the
      // load verdict the gate refuses on — is not what the launch would read
      // any more.
      const readAgain = (targetPath: string) => {
        if (dirty) {
          // The commonest case: the edit was already open when the write
          // landed.
          offerReload(docStoreRef.current, targetPath, changeOffer(targetPath));
          return;
        }
        // Debounce auto-reload to avoid rapid re-parses. The path is
        // re-read inside the timer so a user switching the open file
        // between the event arrival and the 500ms fire doesn't get
        // the OLD file's contents stomped onto the new one.
        clearTimeout(reloadTimerRef.current);
        reloadTimerRef.current = setTimeout(() => {
          const current = docStoreRef.current.getState();
          if (current._pendingIntent !== null) {
            afterPending(event);
            return;
          }
          if (current.currentFilePath !== targetPath) return;
          // Work that appeared inside the window — an edit, or a
          // Source-view edit opened — keeps the reload from running, and
          // the change is offered instead of dropped.
          if (current.isDirty() || !!current.sourceBuffer) {
            offerReload(docStoreRef.current, targetPath, changeOffer(targetPath));
            return;
          }
          autoReload(targetPath);
        }, RELOAD_DEBOUNCE_MS);
      };

      switch (event.type) {
        case "file_deleted":
          // The Source view's picker lists the unit's files, and a fragment
          // deleted from under it would otherwise leave a name in the list
          // that nothing answers for.
          if (!filePath || !aboutTheOpenUnit(event.path)) break;
          // Named, like every message a tab raises app-wide: several tabs
          // can be open, and the one on screen may not be the one concerned.
          addToast(
            event.path === filePath
              ? `${filePath} was deleted externally`
              : `${event.path} was deleted externally — it is one of this bot's files`,
            "warning",
            { persistent: true },
          );
          // A fragment gone is a bot that no longer LOADS: the launch
          // refuses those files and the gate refuses on the load verdict the
          // open answers, so the files are read again rather than left
          // behind a verdict they no longer answer for. Two cases are left
          // to the warning alone: the open file's own deletion, which there
          // is nothing to read for, and a tab holding work of the author's —
          // whose save of a bot whose stored files do not load is refused
          // anyway, so nothing is lost by waiting for it.
          if (event.path === filePath || dirty) break;
          readAgain(filePath);
          break;

        case "file_created":
        case "file_modified":
          // A creation says what a write says: the files on disk are not the
          // ones the tab read. A fragment arrives as one — the author
          // writing what an `import` names, a `git checkout`, an editor
          // saving through a rename — and the server's watcher broadcasts
          // whichever of the two its own debounce saw last.
          if (!filePath || !aboutTheOpenUnit(event.path)) break;
          readAgain(filePath);
          break;
      }
    };
    const unsubscribe = fileWatcher.subscribe(onEvent);

    return () => {
      clearTimeout(reloadTimerRef.current);
      for (const u of deferred) u();
      unsubscribe();
      fileWatcher.disconnect();
    };
  }, []);
}
