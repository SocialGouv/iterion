// Web Push enrollment for browser notifications (a run pausing on a human
// form, run outcomes). The service worker at /service-worker.js is
// registered ONLY here, on the user's explicit toggle — never at page
// load — and the permission prompt likewise.
import { registerPushSubscription, unregisterPushSubscription } from "@/api/notifications";

export type PushStatus =
  | "unsupported"
  | "denied"
  | "not-subscribed"
  | "subscribed";

export function isPushSupported(): boolean {
  return (
    typeof window !== "undefined" &&
    "serviceWorker" in navigator &&
    "PushManager" in window &&
    "Notification" in window
  );
}

// The applicationServerKey must be a Uint8Array of the raw P-256 point
// (Chrome rejects the bare base64url string in older versions).
function urlBase64ToUint8Array(base64: string): Uint8Array {
  const padding = "=".repeat((4 - (base64.length % 4)) % 4);
  const b64 = (base64 + padding).replace(/-/g, "+").replace(/_/g, "/");
  const raw = window.atob(b64);
  const out = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i);
  return out;
}

async function swRegistration(): Promise<ServiceWorkerRegistration> {
  const reg = await navigator.serviceWorker.register("/service-worker.js");
  await navigator.serviceWorker.ready;
  return reg;
}

export async function getPushStatus(): Promise<PushStatus> {
  if (!isPushSupported()) return "unsupported";
  if (Notification.permission === "denied") return "denied";
  const reg = await navigator.serviceWorker.getRegistration("/");
  if (!reg) return "not-subscribed";
  const sub = await reg.pushManager.getSubscription();
  return sub ? "subscribed" : "not-subscribed";
}

// enablePush registers the SW, asks permission (explicit user gesture
// context), subscribes with the server's VAPID key and stores the
// subscription server-side. Throws with a readable message on refusal.
export async function enablePush(vapidPublicKey: string): Promise<void> {
  if (!isPushSupported()) {
    throw new Error("This browser does not support push notifications.");
  }
  const permission = await Notification.requestPermission();
  if (permission !== "granted") {
    throw new Error(
      "Notification permission was not granted. Enable notifications for this site in your browser settings, then try again.",
    );
  }
  const reg = await swRegistration();
  let sub = await reg.pushManager.getSubscription();
  if (sub) {
    // A subscription minted under a previous VAPID key is unusable by the
    // server — drop it and re-subscribe under the current key.
    const current = sub.options?.applicationServerKey;
    if (current) {
      const want = urlBase64ToUint8Array(vapidPublicKey);
      const got = new Uint8Array(current);
      const same = got.length === want.length && got.every((b, i) => b === want[i]);
      if (!same) {
        await sub.unsubscribe();
        sub = null;
      }
    }
  }
  if (!sub) {
    sub = await reg.pushManager.subscribe({
      userVisibleOnly: true,
      applicationServerKey: urlBase64ToUint8Array(vapidPublicKey).buffer as ArrayBuffer,
    });
  }
  await registerPushSubscription(sub.toJSON());
}

// disablePush unsubscribes this browser and removes the server-side
// registration. The server record is removed first so a failure leaves a
// still-working subscription rather than an orphaned server row.
export async function disablePush(): Promise<void> {
  if (!isPushSupported()) return;
  const reg = await navigator.serviceWorker.getRegistration("/");
  const sub = await reg?.pushManager.getSubscription();
  if (!sub) return;
  await unregisterPushSubscription(sub.endpoint);
  await sub.unsubscribe();
}
