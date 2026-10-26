import { ADMIN_BASE, ADMIN_SECTIONS } from "@/views/admin/adminRoutes";
import {
  NAV_REGISTRY_CLOUD,
  NAV_REGISTRY_LOCAL,
} from "@/components/shared/NavLinks";

// The plan-du-site page (RGAA 12.1): a second navigation system generated
// from the SAME link registry the sidebar renders — plus the admin console
// sections. Coverage is pinned by Sitemap.test.tsx against App.tsx's
// route literals.
export default function Sitemap() {
  const groups = [
    { heading: "Studio", groups: NAV_REGISTRY_LOCAL },
    { heading: "Cloud", groups: NAV_REGISTRY_CLOUD },
    {
      heading: "Admin console",
      groups: ADMIN_SECTIONS.map((s) => ({
        label: s.label,
        hrefs: s.items.map((i) => ({
          href: `${ADMIN_BASE}${i.path}`,
          label: i.label,
        })),
      })),
    },
  ];
  return (
    <div className="p-6 max-w-3xl">
      <h1 className="text-title font-semibold text-fg-default mb-4">
        Plan du site — Sitemap
      </h1>
      {groups.map((g) => (
        <section key={g.heading} className="mb-6">
          <h2 className="text-body font-semibold text-fg-default mb-2">
            {g.heading}
          </h2>
          {g.groups.map((group, i) => (
            <div key={i} className="mb-3">
              {group.label && (
                <div className="text-caption font-semibold uppercase tracking-wider text-fg-subtle mb-1">
                  {group.label}
                </div>
              )}
              <ul className="ml-4 list-disc space-y-0.5">
                {group.hrefs.map(({ href, label }) => (
                  <li key={href}>
                    <a href={href} className="text-accent-text hover:underline">
                      {label}
                    </a>
                  </li>
                ))}
              </ul>
            </div>
          ))}
        </section>
      ))}
    </div>
  );
}
