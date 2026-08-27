import { useLocation } from "wouter";

import { Tabs } from "@/components/ui/Tabs";

// Cross-navigation between the super-admin consoles. Mounted at the top
// of each /admin/* page (UsersAdminPage, AuditAdminPage, DLQAdminPage;
// OrgsAdminPage should adopt it too — see the route list below).
const ITEMS = [
  { value: "/admin/orgs", label: "Organizations" },
  { value: "/admin/users", label: "Users" },
  { value: "/admin/llm-credentials", label: "LLM credentials" },
  { value: "/admin/bots", label: "Bot overrides" },
  { value: "/admin/audit", label: "Audit" },
  { value: "/admin/dlq", label: "Dead-letter queue" },
];

export default function AdminNav() {
  const [location, navigate] = useLocation();
  // "/admin" routes to the orgs console, so it highlights that tab.
  const value = ITEMS.some((i) => i.value === location)
    ? location
    : "/admin/orgs";
  return (
    <Tabs
      variant="underline"
      value={value}
      onValueChange={(v) => navigate(v)}
      items={ITEMS}
    />
  );
}
