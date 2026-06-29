import { request } from "./client";

// createPAT mints a personal access token (iap_…) for the signed-in user. The
// plaintext is returned ONCE — the CLI browser-auth flow hands it to the local
// loopback listener. expires_in_days 0 = no expiry (clamped by the platform
// ceiling ITERION_PAT_MAX_TTL if set).
export async function createPAT(
  name: string,
  expiresInDays = 0,
): Promise<{ token: string }> {
  return request(`/me/tokens`, {
    method: "POST",
    body: JSON.stringify({ name, expires_in_days: expiresInDays }),
  });
}
