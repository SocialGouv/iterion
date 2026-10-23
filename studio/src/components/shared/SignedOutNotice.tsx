import { useAuth } from "@/auth/AuthContext";
import { InlineBanner } from "@/components/ui/InlineBanner";

// What a signed-out page says when the browser was signed out by something the
// user has to read before signing in again (AuthState.signedOutNotice): a
// password change the server COMMITTED without being able to open a session
// for it, whose message says to sign in with the new password. The form that
// learned it has unmounted with the session, so the sentence is pinned over
// the page the user lands on, beside the sign-in form it tells them to use.
//
// The server's own message, not a copy of it: the reason is the server's to
// state, and a second wording here would drift from it.
export default function SignedOutNotice() {
  const { status, signedOutNotice } = useAuth();
  if (status !== "anonymous" || !signedOutNotice) return null;
  return (
    <InlineBanner tone="warning" role="alert" title="Sign in again" className="fixed inset-x-0 top-0 z-50">
      {signedOutNotice}
    </InlineBanner>
  );
}
