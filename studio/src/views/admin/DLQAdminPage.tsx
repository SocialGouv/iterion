import { Fragment, useCallback, useEffect, useState } from "react";
import { ReloadIcon } from "@radix-ui/react-icons";

import { formatDateTime } from "@/lib/format";

import {
  discardDLQ,
  FeatureUnavailableError,
  listDLQ,
  peekDLQ,
  replayDLQ,
  type DLQMessage,
} from "@/api/dlq";
import { useAuth } from "@/auth/AuthContext";
import { CloudOnlyNotice } from "@/components/shared/CloudOnlyNotice";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import { Button } from "@/components/ui/Button";
import { EmptyState } from "@/components/ui/EmptyState";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Table, THead, Th, TBody, Tr, Td, TableSkeleton } from "@/components/ui/Table";
import { useConfirm } from "@/hooks/useConfirm";
import { errorMessage, toastError } from "@/lib/errorHints";
import { useServerInfoStore } from "@/store/serverInfo";
import { useUIStore } from "@/store/ui";

import AdminNav from "./AdminNav";

const PAGE = 50;

// Dead-letter queue console: runs whose queue message exhausted its
// redelivery budget (or hit a poison failure) and got parked instead of
// lost. An incident tool — readable list, manual refresh (no polling),
// per-message inspect / replay / discard.
export default function DLQAdminPage() {
  const { user: me } = useAuth();
  const isSuper = me?.is_super_admin ?? false;
  // Cloud-mode console: the /api/admin/dlq routes are registered only
  // when the NATS queue is wired — gate on server_info BEFORE fetching
  // so local mode never fires a doomed 404 request.
  const serverInfo = useServerInfoStore((s) => s.info);
  const isCloud = serverInfo?.mode === "cloud";
  const addToast = useUIStore((s) => s.addToast);
  const { confirm, dialog: confirmDialog } = useConfirm();

  const [messages, setMessages] = useState<DLQMessage[]>([]);
  const [nextCursor, setNextCursor] = useState(0); // 0 = exhausted
  const [loaded, setLoaded] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [unavailable, setUnavailable] = useState(false);
  // Inspect drawer state: which seq is expanded + its fetched payload.
  const [openSeq, setOpenSeq] = useState<number | null>(null);
  const [payloads, setPayloads] = useState<Record<number, string>>({});

  const refresh = useCallback(async () => {
    setBusy(true);
    setErr(null);
    try {
      const r = await listDLQ(0, PAGE);
      setMessages(r.messages ?? []);
      setNextCursor(r.next_cursor);
      setUnavailable(false);
      setOpenSeq(null);
      setPayloads({});
    } catch (e) {
      if (e instanceof FeatureUnavailableError) setUnavailable(true);
      else setErr(errorMessage(e));
    } finally {
      setBusy(false);
      setLoaded(true);
    }
  }, []);

  const loadMore = async () => {
    if (nextCursor === 0) return;
    setBusy(true);
    setErr(null);
    try {
      const r = await listDLQ(nextCursor, PAGE);
      setMessages((cur) => [...cur, ...(r.messages ?? [])]);
      setNextCursor(r.next_cursor);
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  useEffect(() => {
    if (isSuper && isCloud) void refresh();
  }, [isSuper, isCloud, refresh]);

  useHeaderSlot({
    left: <span className="text-sm font-semibold">Dead-letter queue</span>,
    right: loaded && !unavailable ? (
      <span className="text-xs text-fg-muted">{messages.length} parked message(s)</span>
    ) : null,
  });

  if (!isSuper) {
    return (
      <div className="p-6">
        <p className="text-sm text-fg-muted">Super-admin only.</p>
      </div>
    );
  }

  // Deliberate local-mode gate: no fetch fired. While server_info is still
  // loading we fall through to the skeleton instead of flashing this notice
  // on cloud.
  if (serverInfo && !isCloud) {
    return (
      <div className="h-full overflow-auto">
        <div className="max-w-5xl mx-auto p-3 sm:p-6">
          <CloudOnlyNotice feature="Dead-letter queue administration" />
        </div>
      </div>
    );
  }

  const inspect = async (seq: number) => {
    if (openSeq === seq) {
      setOpenSeq(null);
      return;
    }
    setOpenSeq(seq);
    if (payloads[seq] !== undefined) return;
    try {
      const r = await peekDLQ(seq);
      setPayloads((cur) => ({
        ...cur,
        [seq]: JSON.stringify(r.payload, null, 2),
      }));
    } catch (e) {
      setPayloads((cur) => ({
        ...cur,
        [seq]: `Failed to load payload: ${errorMessage(e)}`,
      }));
    }
  };

  const replay = async (m: DLQMessage) => {
    const ok = await confirm({
      title: "Replay dead-lettered run?",
      message: (
        <>
          Re-enqueues message <code>#{m.seq}</code>
          {m.run_id ? (
            <>
              {" "}(run <code className="break-all">{m.run_id}</code>)
            </>
          ) : null}{" "}
          onto the live runs queue and removes it from the DLQ. A runner will
          pick it up like a fresh submission.
        </>
      ),
      confirmLabel: "Replay",
    });
    if (!ok) return;
    setBusy(true);
    try {
      const r = await replayDLQ(m.seq);
      addToast(
        r.run_id ? `Run ${r.run_id} re-enqueued` : `Message #${m.seq} re-enqueued`,
        "success",
      );
      await refresh();
    } catch (e) {
      toastError(addToast, e, "Replay failed");
    } finally {
      setBusy(false);
    }
  };

  const discard = async (m: DLQMessage) => {
    const ok = await confirm({
      title: "Delete dead-lettered message?",
      message: (
        <>
          Permanently deletes message <code>#{m.seq}</code>
          {m.run_id ? (
            <>
              {" "}(run <code className="break-all">{m.run_id}</code>)
            </>
          ) : null}{" "}
          from the DLQ. The parked payload cannot be recovered or replayed
          afterwards.
        </>
      ),
      confirmLabel: "Delete",
      confirmVariant: "danger",
    });
    if (!ok) return;
    setBusy(true);
    try {
      await discardDLQ(m.seq);
      addToast(`Message #${m.seq} deleted`, "success");
      await refresh();
    } catch (e) {
      toastError(addToast, e, "Delete failed");
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="h-full overflow-auto">
      <div className="max-w-6xl mx-auto p-3 sm:p-6 space-y-4">
        <AdminNav />

        <div className="flex items-start justify-between gap-3">
          <p className="text-caption text-fg-subtle max-w-3xl">
            Queue messages parked after exhausting their redelivery budget (a
            runner crash loop, a poison payload). Replay re-enqueues the exact
            original message; delete drops it for good. This list does not
            auto-refresh.
          </p>
          <Button
            size="sm"
            variant="ghost"
            leadingIcon={<ReloadIcon />}
            loading={busy && loaded}
            disabled={unavailable}
            onClick={() => void refresh()}
          >
            Refresh
          </Button>
        </div>

        {err && (
          <InlineBanner tone="danger" layout="inline">
            {err}
          </InlineBanner>
        )}

        {unavailable ? (
          <EmptyState
            title="Dead-letter queue not available"
            message="The /api/admin/dlq endpoints require the cloud-mode NATS queue; this server has no queue connection wired."
          />
        ) : (
          <section className="bg-surface-1 border border-border-subtle rounded-[var(--radius-lg)] shadow-[var(--shadow-sm)] overflow-hidden">
            {!loaded ? (
              <div className="p-3">
                <TableSkeleton rows={4} cols={6} />
              </div>
            ) : messages.length === 0 ? (
              <EmptyState
                title="No dead-lettered messages"
                message="The queue is healthy — nothing has exhausted its redelivery budget."
              />
            ) : (
              <Table caption="Dead-lettered queue messages">
                <THead>
                  <Th>Seq</Th>
                  <Th>Run</Th>
                  <Th>Reason</Th>
                  <Th>Parked at</Th>
                  <Th align="right">Actions</Th>
                </THead>
                <TBody>
                  {messages.map((m) => (
                    <Fragment key={m.seq}>
                      <Tr className="align-top">
                        <Td className="font-mono text-xs">{m.seq}</Td>
                        <Td className="text-xs">
                          <div className="font-mono break-all">{m.run_id || "—"}</div>
                          {m.tenant_id && (
                            <div className="text-fg-subtle font-mono break-all">
                              tenant {m.tenant_id}
                            </div>
                          )}
                        </Td>
                        <Td className="text-xs">
                          <div className="break-words">{m.reason || "—"}</div>
                          {m.num_delivered && (
                            <div className="text-fg-subtle">
                              {m.num_delivered} deliveries
                            </div>
                          )}
                        </Td>
                        <Td className="text-fg-muted text-xs whitespace-nowrap">
                          <div>{formatDateTime(m.parked_at)}</div>
                          <div className="text-fg-subtle">{formatSize(m.size_bytes)}</div>
                        </Td>
                        <Td align="right" className="space-x-1 whitespace-nowrap">
                          <Button size="sm" variant="ghost" onClick={() => void inspect(m.seq)}>
                            {openSeq === m.seq ? "Hide payload" : "Inspect"}
                          </Button>
                          <Button
                            size="sm"
                            variant="ghost"
                            disabled={busy}
                            onClick={() => void replay(m)}
                          >
                            Replay
                          </Button>
                          <Button
                            size="sm"
                            variant="ghost"
                            className="text-danger"
                            disabled={busy}
                            onClick={() => void discard(m)}
                          >
                            Delete
                          </Button>
                        </Td>
                      </Tr>
                      {openSeq === m.seq && (
                        <Tr>
                          <Td colSpan={5} className="bg-surface-2/50">
                            <div className="text-caption text-fg-subtle mb-1">
                              Parked payload (the serialized run message)
                            </div>
                            <pre className="text-caption font-mono whitespace-pre-wrap break-all max-h-80 overflow-auto">
                              {payloads[m.seq] ?? "Loading payload…"}
                            </pre>
                          </Td>
                        </Tr>
                      )}
                    </Fragment>
                  ))}
                </TBody>
              </Table>
            )}
          </section>
        )}

        {nextCursor !== 0 && !unavailable && (
          <div className="flex justify-center">
            <Button size="sm" variant="ghost" loading={busy} onClick={() => void loadMore()}>
              Load more
            </Button>
          </div>
        )}
      </div>

      {confirmDialog}
    </div>
  );
}

function formatSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KiB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MiB`;
}
