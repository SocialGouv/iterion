// Builds the shareable acceptance URL for an invitation token — the
// artifact an admin actually hands to the invitee. On deployments
// without outbound email the raw token alone strands the recipient
// (they'd have to know the /invitations/accept route by heart).
export function inviteAcceptURL(token: string): string {
  return `${window.location.origin}/invitations/accept?token=${encodeURIComponent(token)}`;
}
