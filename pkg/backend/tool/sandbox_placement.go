package tool

import (
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Sandbox placement — where a claw tool may execute for a SANDBOXED node
// ---------------------------------------------------------------------------
//
// A sandboxed claw node runs its LLM loop inside the container
// (`iterion __claw-runner`), but the launcher advertises the node's resolved
// tools over the IPC, and a tool the runner hands back executes on the HOST
// (pkg/backend/model/claw_backend.go, OnToolCall → td.Execute), with the
// launcher's process, cwd, filesystem and network position.
//
// This is the single, exhaustive answer to "may this tool cross?". The
// launcher holds the boundary: its IPC handler executes a forwarded call only
// for a launcher-placed tool (claw_backend.go, OnToolCall), ClawBackend.Execute
// refuses a sandboxed task carrying a Refused tool before the runner starts,
// and a sandboxed tool node refuses a registry tool that is not
// launcher-placed (executor_tool.go). The runner routes by the same answer
// (cmd/iterion/claw_runner_proxy.go), but its routing is a request, never a
// verdict: it is the contained side.

// SandboxPlacement says where a tool advertised to a sandboxed claw node may
// execute.
type SandboxPlacement int

const (
	// PlacementRefused is the ZERO VALUE on purpose: a name no rule below
	// recognises is refused, never proxied. Proxying by default is what let
	// every unclassified execution-capable tool run on the host.
	PlacementRefused SandboxPlacement = iota

	// PlacementSandbox — the tool starts a process, touches a filesystem or
	// opens a model-supplied URL, so it must execute inside the container.
	// The runner registers it locally; failing to do so is a fatal error.
	PlacementSandbox

	// PlacementLauncher — the tool only reads or mutates state the launcher
	// owns (an MCP client, an in-memory registry, the operator seam). It has
	// no host process to start and no model-supplied path to open, so the IPC
	// proxy is its correct home.
	PlacementLauncher
)

func (p SandboxPlacement) String() string {
	switch p {
	case PlacementSandbox:
		return "sandbox"
	case PlacementLauncher:
		return "launcher"
	case PlacementRefused:
		return "refused"
	default:
		return "unknown"
	}
}

// placementRule pairs a placement with the reason that goes in the operator's
// error message.
type placementRule struct {
	placement SandboxPlacement
	reason    string
}

// SandboxPlacementOf classifies a tool by the name the registry knows it
// under — a built-in's bare name, or an MCP tool's qualified `mcp.<server>.
// <tool>` name in any of its three spellings (dotted, sanitized `mcp_…` as it
// travels to the model, claude_code's `mcp__…` FQN).
//
// The second return value is a short reason, phrased to be read inside a
// refusal addressed to the workflow author.
func SandboxPlacementOf(name string) (SandboxPlacement, string) {
	// The name is classified exactly as given — no trimming — so the verdict
	// and the runner's registry lookup (which does not trim) see one name.
	if name == "" {
		return PlacementRefused, "empty tool name"
	}
	// The exact table wins over the MCP prefixes: `mcp_auth` is a built-in,
	// not a tool served by a server called "auth".
	if rule, ok := sandboxPlacements[name]; ok {
		return rule.placement, rule.reason
	}
	if isMCPFamilyName(name) {
		return PlacementLauncher, "it is served by an MCP server, and the servers are connected in the launcher process"
	}
	return PlacementRefused, "no sandbox placement is declared for it, so the engine cannot promise it stays in the container"
}

// SandboxPlacedTools returns the names explicitly classified with the given
// placement, sorted. The MCP family is a prefix rule, not a name, so it is
// absent from every result.
func SandboxPlacedTools(p SandboxPlacement) []string {
	out := make([]string, 0, len(sandboxPlacements))
	for name, rule := range sandboxPlacements {
		if rule.placement == p {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// isMCPFamilyName reports whether a name has the shape of an MCP server's
// tool rather than of a claw built-in: `mcp.<server>.<tool>` (the qualified
// name Registry.RegisterMCP enforces), its sanitized `mcp_<server>_<tool>`, or
// claude_code's `mcp__<server>__<tool>` — i.e. after the `mcp` prefix and its
// separator, SOME further separator leaves a non-empty server and a non-empty
// tool. Any split counts: a server name may itself start with `_`
// (`mcp__scratch_x` is server `_scratch`, tool `x`). A name that stops short
// (`mcp_bash`, `mcp_`, `mcp.x`) is not an MCP tool and falls through to the
// refusal. The shape cannot separate a future `mcp_<a>_<b>` built-in from a
// server's tool; the exhaustiveness test over the real registrars is what
// catches that one.
func isMCPFamilyName(name string) bool {
	for _, sep := range []string{".", "_"} {
		rest, ok := strings.CutPrefix(name, "mcp"+sep)
		if !ok {
			continue
		}
		// A separator strictly inside rest splits it into two non-empty parts.
		return len(rest) >= 3 && strings.Contains(rest[1:len(rest)-1], sep)
	}
	return false
}

// sandboxPlacements is the exhaustive classification of claw's built-in
// namespace — every name RegisterClawAll can register, with every optional
// family switched on. A tool added to a registrar without an entry here is
// refused, and TestSandboxPlacementCoversEveryRegisteredClawTool turns red.
var sandboxPlacements = map[string]placementRule{
	// -----------------------------------------------------------------
	// Sandbox — starts a process, reaches a filesystem, or opens a URL the
	// model chose. Executing these on the launcher IS the sandbox escape.
	// -----------------------------------------------------------------
	"bash":              {PlacementSandbox, "it starts a shell, which must be the container's"},
	"diagnostic_shell":  {PlacementSandbox, "it starts a shell through the same executor as bash"},
	"repl":              {PlacementSandbox, "it starts python3/node/bash with no command validation"},
	"read_file":         {PlacementSandbox, "it reads a model-supplied path"},
	"write_file":        {PlacementSandbox, "it writes a model-supplied path"},
	"file_edit":         {PlacementSandbox, "it rewrites a model-supplied path"},
	"notebook_edit":     {PlacementSandbox, "it reads and writes a model-supplied path"},
	"glob":              {PlacementSandbox, "it walks the workspace tree"},
	"grep":              {PlacementSandbox, "it walks the workspace tree"},
	"workspace_grep":    {PlacementSandbox, "it walks the workspace tree"},
	"skill":             {PlacementSandbox, "it reads skill files from the workspace and the config directories"},
	"read_image":        {PlacementSandbox, "it reads a model-supplied path or fetches a model-supplied URL"},
	"web_fetch":         {PlacementSandbox, "it opens a model-supplied URL, so the container's network policy must apply"},
	"remote_trigger":    {PlacementSandbox, "it opens a model-supplied URL, so the container's network policy must apply"},
	"send_user_message": {PlacementSandbox, "it resolves its attachment paths on the filesystem"},
	"sleep":             {PlacementSandbox, "it is a pure pause and belongs to the loop that is waiting, which runs in the container"},
	"structured_output": {PlacementSandbox, "it echoes its input back to the loop that called it, which runs in the container"},
	"agent": {PlacementSandbox,
		"a sandboxed node's subagent belongs in the container: the runner registers claw's agent tool there, never against the launcher's registry"},

	// -----------------------------------------------------------------
	// Launcher — state the launcher owns. No host process, no model-supplied
	// path, nothing the container could serve.
	// -----------------------------------------------------------------
	"ask_user":       {PlacementLauncher, "only the launcher can pause the run for an operator"},
	"ask_user_async": {PlacementLauncher, "only the launcher holds the run's question channel"},
	"await_answers":  {PlacementLauncher, "only the launcher holds the run's question channel"},

	"list_mcp_resources": {PlacementLauncher,
		"it reads through the launcher's MCP provider, which connects the named server there (for a stdio server, starts it)"},
	"read_mcp_resource": {PlacementLauncher,
		"it reads through the launcher's MCP provider, which connects the named server there (for a stdio server, starts it)"},
	"mcp_auth": {PlacementLauncher, "it reads the auth status of the launcher's MCP servers"},

	"task_create":     {PlacementLauncher, "it mutates the launcher's in-memory task registry"},
	"task_get":        {PlacementLauncher, "it reads the launcher's in-memory task registry"},
	"task_list":       {PlacementLauncher, "it reads the launcher's in-memory task registry"},
	"task_output":     {PlacementLauncher, "it reads the launcher's in-memory task registry"},
	"task_stop":       {PlacementLauncher, "it mutates the launcher's in-memory task registry"},
	"task_update":     {PlacementLauncher, "it mutates the launcher's in-memory task registry"},
	"run_task_packet": {PlacementLauncher, "it mutates the launcher's in-memory task registry"},

	"team_create": {PlacementLauncher, "it mutates the launcher's in-memory team registry"},
	"team_get":    {PlacementLauncher, "it reads the launcher's in-memory team registry"},
	"team_list":   {PlacementLauncher, "it reads the launcher's in-memory team registry"},
	"team_delete": {PlacementLauncher, "it mutates the launcher's in-memory team registry"},

	"cron_create": {PlacementLauncher, "it mutates the launcher's in-memory cron registry"},
	"cron_get":    {PlacementLauncher, "it reads the launcher's in-memory cron registry"},
	"cron_list":   {PlacementLauncher, "it reads the launcher's in-memory cron registry"},
	"cron_delete": {PlacementLauncher, "it mutates the launcher's in-memory cron registry"},

	// Persisted launcher-side, at paths derived from launcher state and never
	// from a model argument; the container is torn down at the end of the node.
	"todo_write":      {PlacementLauncher, "the session todo list lives outside the workspace, keyed on the launcher's own directory"},
	"enter_plan_mode": {PlacementLauncher, "plan-mode state is the launcher's, under the run's store directory"},
	"exit_plan_mode":  {PlacementLauncher, "plan-mode state is the launcher's, under the run's store directory"},
	"privacy_filter":  {PlacementLauncher, "the PII vault is the launcher's, under the run's store directory"},
	"privacy_unfilter": {PlacementLauncher,
		"the PII vault is the launcher's, and rehydrating cleartext inside the container would widen its exposure"},

	"config":      {PlacementLauncher, "it reads the configuration map the launcher supplied; the container has none"},
	"tool_search": {PlacementLauncher, "it searches the launcher's live tool catalog, which is the complete one"},
	"web_search": {PlacementLauncher,
		"it queries fixed search endpoints the model cannot choose, with credentials the launcher resolved"},

	// -----------------------------------------------------------------
	// Refused — neither side can serve it for a sandboxed node: the launcher
	// half runs on the host, and the container has no seam onto that half.
	// -----------------------------------------------------------------
	"lsp": {PlacementRefused,
		"the language servers are child processes of the launcher"},
	"screenshot": {PlacementRefused,
		"it drives the launcher host's display through xdotool / import"},
	"computer_use": {PlacementRefused,
		"it drives the launcher host's display through xdotool / import"},

	"worker_create":             {PlacementRefused, workerRefusal},
	"worker_get":                {PlacementRefused, workerRefusal},
	"worker_observe":            {PlacementRefused, workerRefusal},
	"worker_observe_completion": {PlacementRefused, workerRefusal},
	"worker_resolve_trust":      {PlacementRefused, workerRefusal},
	"worker_await_ready":        {PlacementRefused, workerRefusal},
	"worker_send_prompt":        {PlacementRefused, workerRefusal},
	"worker_restart":            {PlacementRefused, workerRefusal},
	"worker_terminate":          {PlacementRefused, workerRefusal},
}

// workerRefusal is shared by the nine worker_* tools: they address one
// registry whose entries are keyed on a model-supplied working directory that
// the launcher reads config from and writes state into.
const workerRefusal = "a worker is a launcher-side agent keyed on a working directory the model supplies, " +
	"which the launcher reads its configuration from and writes its state into"
