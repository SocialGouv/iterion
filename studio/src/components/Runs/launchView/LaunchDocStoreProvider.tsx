import { useMemo } from "react";

import { DocumentStoreProvider, getOrCreateDocumentStore } from "@/store/document";
import { useTabsStore } from "@/store/tabs";

// The launch route reads the editor's document store (useLaunchDoc): the
// ACTIVE editor tab's store, because that is where the buffer Run was
// clicked on lives — every authoring path (toolbar ops, canvas, import,
// draft hydration) writes a per-tab store, and nothing ever writes the
// module default. Without this bridge the inline launch always read the
// default's pristine scaffold: the Run button enabled on the tab's buffer,
// the view answering "no workflow to launch" (#1326). No active tab — a
// bare deep link to /runs/new — has no buffer, and the default's pristine
// scaffold reading as noSource is the honest answer for it.
export default function LaunchDocStoreProvider({ children }: { children: React.ReactNode }) {
  const activeEditorTabId = useTabsStore((s) => s.activeEditorTabId);
  const store = useMemo(
    () => (activeEditorTabId ? getOrCreateDocumentStore(activeEditorTabId) : null),
    [activeEditorTabId],
  );
  // One tree shape, with or without an active tab: returning the children
  // bare when the last tab closes would remount the whole launch form — its
  // typed values, its preset, its focus gone for an editor event that is not
  // about the launch.
  return <DocumentStoreProvider store={store}>{children}</DocumentStoreProvider>;
}
