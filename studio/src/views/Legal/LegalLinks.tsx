import { Link } from "wouter";

import {
  accessibilityMention,
  legalHasAnything,
  legalRoute,
  useLegalInfo,
} from "./legalInfo";

// The links every public surface owes its visitors (ADR-160): one per
// CONFIGURED document — a document the deployment did not publish has no
// link, never an empty page —, the accessibility mention the statement model
// requires on the home page, and a mailto contact. Each site renders the same
// items; what differs is the wrapper (a footer nav, a stack, a sidebar foot).
//
// The texts are French: they are what the end user reads, and the pages they
// lead to are French documents. `lang="fr"` on the wrapper is what RGAA 8.7
// asks for on a language change inside an English shell.

const LINK_CLASS = "text-fg-muted hover:text-fg-default hover:underline";

/** The anchors alone, for a host that already provides the <nav>. */
export function LegalLinkItems({ className = LINK_CLASS }: { className?: string }) {
  const info = useLegalInfo();
  if (!legalHasAnything(info) || !info) return null;
  const mention = accessibilityMention(info.accessibility);
  const statement = info.documents.find((d) => d.slug === "accessibilite");
  return (
    <>
      {info.documents.map((d) => (
        <Link key={d.slug} href={legalRoute(d.slug)} className={className} lang="fr">
          {d.title}
        </Link>
      ))}
      {mention &&
        (statement ? (
          <Link href={legalRoute(statement.slug)} className={className} lang="fr">
            {mention}
          </Link>
        ) : (
          <span lang="fr">{mention}</span>
        ))}
      {info.contact_email && (
        <a href={`mailto:${info.contact_email}`} className={className} lang="fr">
          Contact
        </a>
      )}
    </>
  );
}

/** A labelled navigation landmark of the legal links, for a page foot. */
export function LegalLinks({ className = "" }: { className?: string }) {
  const info = useLegalInfo();
  if (!legalHasAnything(info)) return null;
  return (
    <nav
      aria-label="Informations légales"
      lang="fr"
      className={`flex flex-wrap items-center justify-center gap-x-4 gap-y-1 text-xs ${className}`.trim()}
    >
      <LegalLinkItems />
    </nav>
  );
}
