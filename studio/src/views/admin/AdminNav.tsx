import { useLocation } from "wouter";

// Cross-navigation for the super-admin consoles, mounted at the top of every
// /admin/* page. Grouped into sections so the (now dozen) consoles stay
// readable instead of a single overflowing tab row:
//   - Tenancy   — the orgs/users the platform hosts
//   - Platform  — deployment-wide credentials, runtime settings and bot catalog
//   - Operations — audit, incident queue and spend
// Each entry drives an /admin/* route (see App.tsx). Grouping is presentation
// only; the routes and their guard (RequireSuperAdmin) are unchanged.

interface NavItem {
  value: string;
  label: string;
}
interface NavSection {
  label: string;
  items: NavItem[];
}

const SECTIONS: NavSection[] = [
  {
    label: "Tenancy",
    items: [
      { value: "/admin/orgs", label: "Organizations" },
      { value: "/admin/users", label: "Users" },
    ],
  },
  {
    label: "Platform",
    items: [
      { value: "/admin/llm-credentials", label: "LLM credentials" },
      { value: "/admin/settings/usage-caps", label: "Usage caps" },
      { value: "/admin/settings/bot-roles", label: "Bot roles" },
      { value: "/admin/settings/sandbox", label: "Sandbox" },
      { value: "/admin/settings/bot-vars", label: "Bot vars" },
      { value: "/admin/settings/platform-credentials", label: "Platform creds" },
      { value: "/admin/bots", label: "Bot overrides" },
    ],
  },
  {
    label: "Operations",
    items: [
      { value: "/admin/audit", label: "Audit" },
      { value: "/admin/dlq", label: "Dead-letter queue" },
      { value: "/admin/spend", label: "Spend" },
    ],
  },
];

// The flat list of every value, kept for the active-tab resolution.
const ALL_VALUES = SECTIONS.flatMap((s) => s.items.map((i) => i.value));

export default function AdminNav() {
  const [location, navigate] = useLocation();
  // "/admin" (bare) routes to the orgs console, so it highlights that entry.
  const active = ALL_VALUES.includes(location) ? location : "/admin/orgs";

  return (
    <nav
      aria-label="Admin console sections"
      className="flex flex-wrap items-center gap-x-5 gap-y-2 border-b border-border-default pb-2"
    >
      {SECTIONS.map((section) => (
        <div key={section.label} className="flex items-center gap-1.5">
          <span className="text-micro font-semibold uppercase tracking-wide text-fg-subtle">
            {section.label}
          </span>
          <div className="flex items-center gap-1">
            {section.items.map((item) => {
              const isActive = item.value === active;
              return (
                <button
                  key={item.value}
                  type="button"
                  aria-current={isActive ? "page" : undefined}
                  onClick={() => navigate(item.value)}
                  className={`rounded-md px-2 py-1 text-xs font-medium transition-colors ${
                    isActive
                      ? "bg-surface-2 text-fg-default"
                      : "text-fg-muted hover:bg-surface-1 hover:text-fg-default"
                  }`}
                >
                  {item.label}
                </button>
              );
            })}
          </div>
        </div>
      ))}
    </nav>
  );
}
