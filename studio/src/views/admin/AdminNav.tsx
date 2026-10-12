import { Link, useLocation } from "wouter";

import { ADMIN_SECTIONS, consolePath } from "./adminRoutes";

// Cross-navigation between the super-admin consoles, mounted at the top of
// every /admin page. Entries and their grouping come from the console's route
// table (adminRoutes.ts), so a screen that exists is reachable here.
//
// It only ever renders inside App.tsx's `<Route path="/admin" nest>`: wouter
// reports a location relative to /admin there and resolves every href against
// it, which is why both the current-entry test and the link targets are the
// table's relative paths.
export default function AdminNav() {
  const [location] = useLocation();
  // consolePath, not the raw location: the route a trailing-slash address
  // matches and the entry marked current have to be the same screen.
  const here = consolePath(location);
  return (
    <nav aria-label="Admin console" className="border-b border-border-default">
      <div className="flex flex-wrap items-center gap-x-5">
        {ADMIN_SECTIONS.map((section) => (
          <div key={section.id} className="flex items-center gap-2">
            <span
              id={`admin-nav-${section.id}`}
              className="text-caption uppercase tracking-wide text-fg-subtle"
            >
              {section.label}
            </span>
            <ul
              aria-labelledby={`admin-nav-${section.id}`}
              className="flex flex-wrap items-center"
            >
              {section.items.map((item) => {
                const current = item.path === here;
                return (
                  <li key={item.path}>
                    <Link
                      href={item.path}
                      aria-current={current ? "page" : undefined}
                      className={`relative block px-2.5 py-2 text-xs font-medium ${
                        current
                          ? "text-fg-default after:content-[''] after:absolute after:left-2 after:right-2 after:bottom-0 after:h-0.5 after:bg-accent"
                          : "text-fg-subtle hover:text-fg-default"
                      }`}
                    >
                      {item.label}
                    </Link>
                  </li>
                );
              })}
            </ul>
          </div>
        ))}
      </div>
    </nav>
  );
}
