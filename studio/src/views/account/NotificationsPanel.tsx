import { errorMessage } from "@/lib/errorHints";
import { useEffect, useState } from "react";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Button } from "@/components/ui/Button";
import { Checkbox } from "@/components/ui/Checkbox";
import { Spinner } from "@/components/ui/Spinner";
import { useServerInfoStore } from "@/store/serverInfo";
import { useUIStore } from "@/store/ui";
import {
  getNotificationPrefs,
  sendTestPush,
  setNotificationPrefs,
  type NotificationScope,
} from "@/api/notifications";
import {
  disablePush,
  enablePush,
  getPushStatus,
  isPushSupported,
  type PushStatus,
} from "@/lib/webPush";

// Browser push notifications: enable/disable THIS browser (the server-side
// subscription is the state — no extra flag), team-wide opt-in, test push.
export default function NotificationsPanel() {
  const serverInfo = useServerInfoStore((s) => s.info);
  const addToast = useUIStore((s) => s.addToast);
  const vapidKey = serverInfo?.web_push_vapid_public_key ?? "";

  const [status, setStatus] = useState<PushStatus | "loading">("loading");
  const [scope, setScope] = useState<NotificationScope>("own");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    let alive = true;
    void getPushStatus().then((s) => alive && setStatus(s));
    getNotificationPrefs()
      .then((p) => alive && setScope(p.scope))
      .catch(() => {
        // Prefs default to "own" server-side; a load failure only affects
        // the checkbox's initial render.
      });
    return () => {
      alive = false;
    };
  }, []);

  const subscribed = status === "subscribed";

  const toggle = async (on: boolean) => {
    setBusy(true);
    setErr(null);
    try {
      if (on) {
        await enablePush(vapidKey);
        addToast("Notifications enabled for this browser", "success");
      } else {
        await disablePush();
        addToast("Notifications disabled for this browser", "info");
      }
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setStatus(await getPushStatus());
      setBusy(false);
    }
  };

  const changeScope = async (next: NotificationScope) => {
    const prev = scope;
    setScope(next);
    try {
      await setNotificationPrefs(next);
    } catch (e) {
      setScope(prev);
      setErr(errorMessage(e));
    }
  };

  const test = async () => {
    setBusy(true);
    setErr(null);
    try {
      await sendTestPush();
      addToast("Test notification sent — check your system notifications", "success");
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  if (!isPushSupported()) {
    return (
      <InlineBanner tone="info">
        This browser does not support push notifications.
      </InlineBanner>
    );
  }

  return (
    <div className="space-y-6 max-w-xl">
      {err && <InlineBanner tone="danger">{err}</InlineBanner>}

      <section className="space-y-3">
        <h2 className="text-sm font-semibold">Browser notifications</h2>
        <p className="text-sm text-fg-muted">
          Get a system notification when a run pauses waiting for your answer, and
          when a run finishes or fails — even when this tab is closed.
        </p>
        {status === "loading" ? (
          <Spinner />
        ) : status === "denied" ? (
          <InlineBanner tone="warning">
            Notifications are blocked for this site. Allow them in your browser's
            site settings, then come back here to enable.
          </InlineBanner>
        ) : (
          <Checkbox
            label="Enable notifications on this browser"
            checked={subscribed}
            disabled={busy}
            onChange={(e) => void toggle(e.target.checked)}
          />
        )}
      </section>

      {subscribed && (
        <>
          <section className="space-y-3">
            <h2 className="text-sm font-semibold">Which runs</h2>
            <Checkbox
              label="Also notify me for every run of my team"
              help="Off: only runs you launched."
              checked={scope === "team"}
              disabled={busy}
              onChange={(e) => void changeScope(e.target.checked ? "team" : "own")}
            />
          </section>

          <section className="space-y-3">
            <Button variant="secondary" size="sm" disabled={busy} onClick={() => void test()}>
              Send a test notification
            </Button>
          </section>
        </>
      )}
    </div>
  );
}
