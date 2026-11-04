import { useEffect, useState } from "react";
import { Link, useParams } from "wouter";
import { ApiError } from "@/api/client";
import { getLegalDocument } from "@/api/legal";
import type { LegalDocument } from "@/api/types";
import MarkdownText from "@/components/Runs/conversation/MarkdownText";
import { PublicTopBar } from "@/views/CloudLanding";
import { LegalLinks } from "./LegalLinks";
import { accessibilityMention } from "./legalInfo";

// A legal page (ADR-160): one of the operator's documents, served public at
// /legal/<slug> to a visitor with or without a session, rendered from the
// markdown the deployment configured through the same MarkdownText the run
// console uses: GFM on, raw HTML escaped (no rehype-raw) — the document is
// the operator's text, not a template the page trusts —, and the document's
// own headings one level below the page's h1 (the title the server sets), so
// a text that opens with a top-level heading does not produce two h1s.
//
// The page is French throughout — the documents are French legal texts —
// hence `lang="fr"` on the main landmark (RGAA 8.7).

type PageState =
  | { kind: "loading" }
  | { kind: "ready"; doc: LegalDocument }
  | { kind: "missing" }
  | { kind: "error"; message: string };

// A settled fetch remembers the slug it answered for: on a navigation to
// another document the page reads "loading" until the new answer lands,
// without resetting state from inside the effect.
type Settled = { slug: string; state: PageState };

// An address under /legal that names no document (/legal/, /legal/a/b) is
// the same answer as an unpublished one — "missing", with a way out — never
// a fetch of an empty slug nor a blank page.
export default function LegalPage({ signedIn = false }: { signedIn?: boolean }) {
  const { slug = "" } = useParams<{ slug?: string }>();
  const [settled, setSettled] = useState<Settled | null>(null);
  const state: PageState = !slug
    ? { kind: "missing" }
    : settled?.slug === slug
      ? settled.state
      : { kind: "loading" };

  useEffect(() => {
    if (!slug) return;
    let cancelled = false;
    const settle = (next: PageState) => {
      if (!cancelled) setSettled({ slug, state: next });
    };
    getLegalDocument(slug)
      .then((doc) => settle({ kind: "ready", doc }))
      .catch((err: unknown) => {
        if (err instanceof ApiError && err.status === 404) {
          settle({ kind: "missing" });
          return;
        }
        settle({ kind: "error", message: err instanceof Error ? err.message : String(err) });
      });
    return () => {
      cancelled = true;
    };
  }, [slug]);

  const title = state.kind === "ready" ? `${state.doc.title} — iterion` : "Informations légales — iterion";
  // The page titles the tab while it is mounted and hands the previous title
  // back on the way out: the next view (the sign-in card) may set none.
  useEffect(() => {
    const previous = document.title;
    document.title = title;
    return () => {
      document.title = previous;
    };
  }, [title]);

  return (
    <div className="min-h-screen flex flex-col bg-surface-0 text-fg-default">
      <PublicTopBar signedIn={signedIn} />
      <main id="main" lang="fr" className="flex-1 w-full mx-auto max-w-3xl px-4 py-8 sm:px-6">
        {state.kind === "loading" && (
          <p role="status" className="text-fg-muted">
            Chargement du document…
          </p>
        )}
        {state.kind === "missing" && (
          <section aria-labelledby="legal-missing">
            <h1 id="legal-missing" className="text-title font-semibold mb-3">
              Document non disponible
            </h1>
            <p className="text-fg-muted">
              Ce document n’est pas publié sur ce déploiement.
            </p>
            <p className="mt-4">
              <Link href="/" className="text-accent-text underline underline-offset-2 hover:decoration-2">
                Retour à l’accueil
              </Link>
            </p>
          </section>
        )}
        {state.kind === "error" && (
          <section aria-labelledby="legal-error">
            <h1 id="legal-error" className="text-title font-semibold mb-3">
              Document indisponible
            </h1>
            <p role="alert" className="text-fg-muted">
              Le document n’a pas pu être chargé : {state.message}
            </p>
          </section>
        )}
        {state.kind === "ready" && (
          <article lang="fr" aria-labelledby="legal-title">
            <h1 id="legal-title" className="text-title font-semibold mb-6">
              {state.doc.title}
            </h1>
            <MarkdownText value={state.doc.markdown} size="lg" />
            <LegalDocumentFooter doc={state.doc} />
          </article>
        )}
      </main>
      <footer className="border-t border-border-subtle px-4 py-4">
        <LegalLinks />
      </footer>
    </div>
  );
}

// The metadata every document carries beside its text: the accessibility
// mention and the contact address, so a reader of the privacy policy finds
// the DPO's door without hunting for another page.
function LegalDocumentFooter({ doc }: { doc: LegalDocument }) {
  const mention = accessibilityMention(doc.accessibility);
  const date = doc.accessibility?.date;
  if (!mention && !doc.contact_email) return null;
  return (
    <footer className="mt-10 border-t border-border-default pt-4 text-sm text-fg-muted space-y-1">
      {mention && (
        <p>
          {mention}
          {date && <> — déclaration établie le {formatFrenchDate(date)}</>}
        </p>
      )}
      {doc.contact_email && (
        <p>
          Contact :{" "}
          <a href={`mailto:${doc.contact_email}`} className="text-accent-text underline underline-offset-2 hover:decoration-2">
            {doc.contact_email}
          </a>
        </p>
      )}
    </footer>
  );
}

// YYYY-MM-DD → the French long date; the raw value when it is not a date
// (the server validates the format, so this is a guard, not a path).
function formatFrenchDate(iso: string): string {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(iso);
  if (!m) return iso;
  const d = new Date(Date.UTC(Number(m[1]), Number(m[2]) - 1, Number(m[3])));
  return d.toLocaleDateString("fr-FR", { day: "numeric", month: "long", year: "numeric", timeZone: "UTC" });
}
