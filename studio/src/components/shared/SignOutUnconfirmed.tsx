import { useState } from "react";
import { abandon, signOutMarkScope } from "@/api/signOutMachine";
import { useAuth } from "@/auth/AuthContext";
import { isScopedPane } from "@/lib/scope";
import { Button } from "@/components/ui/Button";
import { InlineBanner } from "@/components/ui/InlineBanner";

// What every page says while the sign-out machine's record stands — the
// signed-out pages, and the authenticated shell of a tab whose session is not
// the one the record is about (I1: the warning stands in EVERY tab/window
// while the session may still be live; I2: an abandonment is a named state
// with a visible notice, never a silent removal):
//
//   - OWED ("Sign-out not confirmed"): the server did not confirm the
//     sign-out, so the session the user asked to end may still be live.
//
//   - ABANDONED: the user chose to go on without the sign-out ("sign in
//     anyway", "continue with the provider anyway"), and the record stays:
//     the session they asked to end stays active until its expiry. The notice
//     says so in every tab.
//
// The banner's action is keyed on the record's HELDNESS — whether this
// browser can still present the logout — and then on its state:
//
//   - not gone (the browser holds the session, or does not know yet): the
//     Retry button presents the logout again, owed and abandoned alike.
//
//   - gone + owed: presenting is impossible from here (`present()` would
//     answer "none" without a wire call), so the button names what is true
//     instead: "Got it — the session stays active" moves the record to
//     ABANDONED, the machine's named end for a record this browser can no
//     longer present. It ends nothing and presents nothing; the notice STAYS
//     under the abandoned title (I2).
//
//   - gone + abandoned: no action at all — the notice stands; there is
//     nothing left to do from this browser.
//
// What follows an OWED warning is what HOLDS the pending sign-out, which
// decides what can be promised:
//   - a workspace pane: the desktop does. It marks the connection's jar
//     sign-out-pending — it stops presenting the token and re-presents the
//     logout itself — so the promise is the desktop's to keep, and the text
//     says whose.
//   - the browser's localStorage: every tab reads it and a reload finds it, so
//     nothing in this browser adopts that session again.
//   - this document alone (storage the browser refuses): the record dies with
//     the tab, and a reload or a new tab before a confirmed retry reads the
//     cookies as any other session's. Saying "iterion signs no one back into
//     it" there would be false.
export default function SignOutUnconfirmed() {
  const { signOutRecord, retrySignOut } = useAuth();
  const [retrying, setRetrying] = useState(false);
  if (!signOutRecord) return null;
  const { record, heldness } = signOutRecord;
  const abandoned = record.state === "abandoned";
  const gone = heldness === "gone";
  const retry = async () => {
    setRetrying(true);
    try {
      await retrySignOut();
    } finally {
      setRetrying(false);
    }
  };
  // The expiry is the session's own bound as the credential answers gave it:
  // the texts name it when it is known, "its expiry" when it is not.
  const until = record.expires_at ? ` until ${record.expires_at}` : " until its expiry";
  const whose = record.account ? ` (${record.account})` : "";
  const action = gone ? (
    abandoned ? undefined : (
      <Button variant="secondary" size="sm" onClick={() => abandon()}>
        Got it — the session stays active
      </Button>
    )
  ) : (
    <Button variant="secondary" size="sm" loading={retrying} onClick={() => void retry()}>
      {retrying ? "Retrying…" : "Retry sign-out"}
    </Button>
  );
  return (
    <InlineBanner
      tone="warning"
      role="alert"
      title={abandoned ? "Sign-out abandoned — that session is still active" : "Sign-out not confirmed"}
      className="fixed inset-x-0 top-0 z-50"
      action={action}
    >
      {abandoned ? (
        gone ? (
          <>
            You went on without this sign-out: the session you asked to end{whose} was not confirmed ended and stays
            active{until}. It is not this sign-in&apos;s session. Nothing here ends it — sign in and sign out again, or
            retry from the browser that holds it.
          </>
        ) : (
          <>
            You went on without this sign-out: the session you asked to end{whose} was not confirmed ended and stays
            active{until}. It is not this sign-in&apos;s session. Retry the sign-out before you leave a shared computer.
          </>
        )
      ) : gone ? (
        <>
          The session you signed out of{whose} is no longer the one this browser holds, but the server did not confirm
          its sign-out: it may still be active elsewhere{until}. Nothing here ends it — sign in and sign out again, or
          retry from the browser that holds it.
        </>
      ) : isScopedPane() ? (
        <>
          The cloud did not confirm it, so you may not be signed out yet: this connection can still hold your session.
          The desktop has stopped using it and keeps presenting the sign-out until the cloud confirms. On a shared
          computer, retry before you leave.
          {record.expires_at ? ` The session stays active until ${record.expires_at}.` : ""}
        </>
      ) : signOutMarkScope() === "browser" ? (
        <>
          The server did not confirm it, so you may not be signed out yet: this device can still hold your session.
          iterion signs no one back into it. On a shared computer, retry before you leave.
          {record.expires_at ? ` The session stays active until ${record.expires_at}.` : ""}
        </>
      ) : (
        <>
          The server did not confirm it, so you may not be signed out yet: this device can still hold your session.
          This browser will not keep that warning past this tab, so a reload or a new tab before a confirmed retry may
          sign you back in. Retry here until it is confirmed, and close the browser before you leave a shared computer.
          {record.expires_at ? ` The session stays active until ${record.expires_at}.` : ""}
        </>
      )}
    </InlineBanner>
  );
}
