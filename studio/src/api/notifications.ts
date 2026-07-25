// Browser push-notification API client (cloud mode, gated on
// server_info.web_push_enabled). Cookie-authed like every other call.
import { apiRequest } from "./client";

export type NotificationScope = "own" | "team";

export async function registerPushSubscription(sub: PushSubscriptionJSON): Promise<void> {
  await apiRequest<{ ok: boolean }>("/api/v1/notifications/push/subscriptions", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(sub),
  });
}

export async function unregisterPushSubscription(endpoint: string): Promise<void> {
  await apiRequest<{ ok: boolean }>("/api/v1/notifications/push/subscriptions", {
    method: "DELETE",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ endpoint }),
  });
}

export async function getNotificationPrefs(): Promise<{ scope: NotificationScope }> {
  return apiRequest<{ scope: NotificationScope }>("/api/v1/notifications/prefs");
}

export async function setNotificationPrefs(scope: NotificationScope): Promise<void> {
  await apiRequest<{ scope: NotificationScope }>("/api/v1/notifications/prefs", {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ scope }),
  });
}

export async function sendTestPush(): Promise<void> {
  await apiRequest<{ ok: boolean }>("/api/v1/notifications/push/test", { method: "POST" });
}
