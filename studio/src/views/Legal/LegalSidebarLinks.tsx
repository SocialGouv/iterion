import { Link } from "wouter";
import { FileTextIcon } from "@radix-ui/react-icons";

import { legalRoute, useLegalInfo } from "./legalInfo";

// The legal links inside the signed-in shell (RGAA 12: every page reaches
// them): the sidebar foot and the plan du site. Both read the configured
// documents from the server and render nothing when there is none. Both
// render under the studio's router base, hence legalRoute, never a bare
// "/legal/<slug>".

/** The sidebar foot: the full list when expanded, one icon link to the first
 *  configured document when collapsed (the sitemap lists them all). */
export function LegalSidebarLinks({ collapsed }: { collapsed: boolean }) {
  const info = useLegalInfo();
  const first = info?.documents[0];
  if (!info || !first) return null;
  if (collapsed) {
    return (
      <Link
        href={legalRoute(first.slug)}
        className="inline-flex items-center justify-center h-7 w-7 rounded text-fg-muted hover:text-fg-default hover:bg-surface-2"
        title="Informations légales"
        aria-label="Informations légales"
        lang="fr"
      >
        <FileTextIcon className="w-4 h-4 shrink-0" />
      </Link>
    );
  }
  return (
    <nav aria-label="Informations légales" lang="fr" className="flex flex-col gap-0.5 w-full">
      {info.documents.map((d) => (
        <Link
          key={d.slug}
          href={legalRoute(d.slug)}
          className="inline-flex items-center gap-2 text-xs text-fg-muted hover:text-fg-default hover:bg-surface-2 rounded w-full px-2 py-1"
        >
          <FileTextIcon className="w-4 h-4 shrink-0" />
          <span>{d.title}</span>
        </Link>
      ))}
    </nav>
  );
}

/** The plan-du-site group of the configured legal documents. */
export function LegalSitemapGroup() {
  const info = useLegalInfo();
  if (!info || info.documents.length === 0) return null;
  return (
    <section className="mb-6" lang="fr">
      <h2 className="text-body font-semibold text-fg-default mb-2">
        Informations légales
      </h2>
      <ul className="ml-4 list-disc space-y-0.5">
        {info.documents.map((d) => (
          <li key={d.slug}>
            <Link href={legalRoute(d.slug)} className="text-accent-text underline underline-offset-2 hover:decoration-2">
              {d.title}
            </Link>
          </li>
        ))}
      </ul>
    </section>
  );
}
