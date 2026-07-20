// Out-of-band handling for run-health alert events
// (pkg/store.EventAlert). They are NEVER persisted to events.jsonl and
// the broker fans them out WITHOUT a seq (Seq=0). Feeding them through
// the seq-ordered event store would (a) get them dropped by
// reduceEvents' `seq <= lastSeq` guard once any real event has
// advanced the high-water mark, and (b) corrupt the WS reconnect
// `from_seq` computation (events[last].seq + 1). So they are handled
// here instead: render the toast + light the notification dot
// directly, and keep them out of the events array entirely. Because
// they are never persisted, they only ever arrive once on the live
// tail — no replay/dedup risk.

import type { RunEvent } from "@/api/runs";
import { toastForEvent } from "@/hooks/useRunToasts";
import { useUIStore } from "@/store/ui";

export function handleAlertEvent(evt: RunEvent): void {
  const ui = useUIStore.getState();
  const toast = toastForEvent(evt);
  if (toast) ui.addToast(toast.message, toast.type);
  ui.bumpAlertUnseen();
}
