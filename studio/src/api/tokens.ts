import { request } from "./client";

// createPAT mints a personal access token (iap_…) for the signed-in user. The
// plaintext is returned ONCE — the CLI browser-auth flow hands it to the local
// loopback listener. expires_in_days 0 = no expiry (clamped by the platform
// ceiling ITERION_PAT_MAX_TTL if set). teamID pins the token to a team (the
// `teams switch` approval passes one — #2674); empty pins the account's active
// team server-side. The server states the team's org as org_id.
export async function createPAT(
  name: string,
  expiresInDays = 0,
  teamID = "",
): Promise<{ token: string; org_id?: string }> {
  return request(`/me/tokens`, {
    method: "POST",
    body: JSON.stringify({
      name,
      expires_in_days: expiresInDays,
      ...(teamID ? { team_id: teamID } : {}),
    }),
  });
}
