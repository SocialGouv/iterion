// @vitest-environment jsdom
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import type { ApiKeyView } from "@/api/byok";
import ApiKeysPanel from "./ApiKeys";

const api = vi.hoisted(() => ({
  listApiKeys: vi.fn(), createApiKey: vi.fn(), deleteApiKey: vi.fn(), updateApiKey: vi.fn(),
}));
vi.mock("@/api/byok", () => api);
vi.mock("@/auth/AuthContext", () => ({ useAuth: () => ({ user: { id: "u1", is_super_admin: true }, orgs: [] }) }));
vi.mock("@/store/serverInfo", () => ({
  useServerInfoStore: (select: (s: { info: { mode: string } }) => unknown) => select({ info: { mode: "cloud" } }),
}));
vi.mock("@/hooks/useCanManageTeam", () => ({ useCanManageTeam: () => true }));
vi.mock("@/hooks/useConfirm", () => ({ useConfirm: () => ({ confirm: async () => true, dialog: null }) }));
afterEach(() => { cleanup(); vi.clearAllMocks(); });

function keys(): ApiKeyView[] {
  const base = { scope_team_id: "team-a", is_default: false, created_at: "2026-10-21T08:00:00Z", bots: [] as string[] };
  return [
    // A gateway key: its endpoint and its repository audience are shown,
    // so the operator can verify what a key is bound to without the API.
    { ...base, id: "gw", provider: "openai_compatible", name: "pliage", last4: "7890", base_url: "https://gw.example.test", repos: ["dnum-socialgouv/aides-jeunes"] },
    // The shape a server one release behind returns (the rollback window):
    // no `repos` field at all. The list must not crash on it.
    { ...base, id: "old", provider: "anthropic", name: "legacy", last4: "1111" } as unknown as ApiKeyView,
  ];
}

type PanelProps = Parameters<typeof ApiKeysPanel>[0];

function setup(props: PanelProps = { team: { id: "team-a", name: "Team A" } }) {
  api.listApiKeys.mockResolvedValue(keys());
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={client}><ApiKeysPanel {...props} /></QueryClientProvider>);
}

it("shows a gateway key's endpoint and repository audience, and survives a row without repos", async () => {
  setup();
  expect(await screen.findByText("https://gw.example.test")).toBeTruthy();
  expect(screen.getByText("repos: dnum-socialgouv/aides-jeunes")).toBeTruthy();
  expect(screen.getByText("legacy")).toBeTruthy();
});

it("says what a gateway key funds, only for that provider", async () => {
  setup();
  await screen.findByText("pliage");
  fireEvent.click(screen.getByRole("button", { name: "Add key" }));
  expect(screen.queryByText(/funds the gateway routes/)).toBeNull();
  const provider = screen.getByRole("combobox") as HTMLSelectElement;
  fireEvent.change(provider, { target: { value: "openai_compatible" } });
  expect(screen.getByText(/funds the gateway routes/)).toBeTruthy();
  expect(screen.queryByText(/no run spends it yet/)).toBeNull();
  expect(screen.getByPlaceholderText(/Gateway base URL/)).toBeTruthy();
});

// A text input followed by a password input is what a browser takes for a
// login form: it fills the saved username and password into them, and the form
// would seal the operator's password as an API key. The panel is one component
// mounted at three scopes, so each of them is opened.
it.each<[string, PanelProps]>([
  ["a team", { team: { id: "team-a", name: "Team A" } }],
  ["the current user", {}],
  ["the platform", { platform: true }],
])("keeps the browser's saved login out of the add-key form of %s", async (_scope, props) => {
  setup(props);
  await screen.findByText("pliage");
  fireEvent.click(screen.getByRole("button", { name: "Add key" }));

  expect(screen.getByPlaceholderText(/^Name/).getAttribute("autocomplete")).toBe("off");
  const secret = screen.getByPlaceholderText(/^API key/) as HTMLInputElement;
  expect(secret.type).toBe("password");
  // Not "off": a browser ignores it on a password input.
  expect(secret.getAttribute("autocomplete")).toBe("new-password");

  // A gateway key adds the base URL right above the secret, in the place a
  // browser reads as the username.
  fireEvent.change(screen.getByRole("combobox"), { target: { value: "openai_compatible" } });
  expect(screen.getByPlaceholderText(/Gateway base URL/).getAttribute("autocomplete")).toBe("off");
});
