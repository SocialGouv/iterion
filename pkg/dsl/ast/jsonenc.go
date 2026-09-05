// MarshalFile / UnmarshalFile provide JSON serialization and deserialization for File
// types, converting Go iota-based enums to human-readable string representations.
package ast

import (
	"encoding/json"
	"fmt"
)

// ---------------------------------------------------------------------------
// Enum string mappings
// ---------------------------------------------------------------------------

var fieldTypeToStr = map[FieldType]string{
	FieldTypeString:      "string",
	FieldTypeBool:        "bool",
	FieldTypeInt:         "int",
	FieldTypeFloat:       "float",
	FieldTypeJSON:        "json",
	FieldTypeStringArray: "string[]",
	FieldTypeFile:        "file",
}

var strToFieldType = reverseMap(fieldTypeToStr)

var sessionModeToStr = map[SessionMode]string{
	SessionFresh:              "fresh",
	SessionInherit:            "inherit",
	SessionInheritIfAvailable: "inherit_if_available",
	SessionArtifactsOnly:      "artifacts_only",
	SessionFork:               "fork",
	SessionPersist:            "persist",
}

var strToSessionMode = reverseMap(sessionModeToStr)

var mcpTransportToStr = map[MCPTransport]string{
	MCPTransportUnknown: "unknown",
	MCPTransportStdio:   "stdio",
	MCPTransportHTTP:    "http",
	MCPTransportSSE:     "sse",
}

var strToMCPTransport = func() map[string]MCPTransport {
	m := reverseMap(mcpTransportToStr)
	m[""] = MCPTransportUnknown
	return m
}()

var routerModeToStr = map[RouterMode]string{
	RouterFanOutAll:  "fan_out_all",
	RouterFanOutEach: "fan_out_each",
	RouterCondition:  "condition",
	RouterRoundRobin: "round_robin",
	RouterLLM:        "llm",
}

var strToRouterMode = reverseMap(routerModeToStr)

var awaitModeToStr = map[AwaitMode]string{
	AwaitWaitAll:    "wait_all",
	AwaitBestEffort: "best_effort",
}

var strToAwaitMode = func() map[string]AwaitMode {
	m := reverseMap(awaitModeToStr)
	m["none"] = AwaitNone // accept "none" on input, but never emit it
	return m
}()

var interactionModeToStr = map[InteractionMode]string{
	InteractionNone:       "none",
	InteractionHuman:      "human",
	InteractionLLM:        "llm",
	InteractionLLMOrHuman: "llm_or_human",
	InteractionReview:     "review",
	InteractionAsync:      "async",
}

var strToInteractionMode = reverseMap(interactionModeToStr)

var typeExprToStr = map[TypeExpr]string{
	TypeString:      "string",
	TypeBool:        "bool",
	TypeInt:         "int",
	TypeFloat:       "float",
	TypeJSON:        "json",
	TypeStringArray: "string[]",
}

var strToTypeExpr = reverseMap(typeExprToStr)

var literalKindToStr = map[LiteralKind]string{
	LitString: "string",
	LitInt:    "int",
	LitFloat:  "float",
	LitBool:   "bool",
}

var strToLiteralKind = reverseMap(literalKindToStr)

func reverseMap[K comparable, V comparable](m map[K]V) map[V]K {
	r := make(map[V]K, len(m))
	for k, v := range m {
		r[v] = k
	}
	return r
}

// ---------------------------------------------------------------------------
// JSON mirror structs
// ---------------------------------------------------------------------------

type jsonFile struct {
	Vars         *jsonVarsBlock          `json:"vars,omitempty"`
	Presets      *jsonPresetsBlock       `json:"presets,omitempty"`
	Attachments  *jsonAttachmentsBlock   `json:"attachments,omitempty"`
	Secrets      *jsonSecretsBlock       `json:"secrets,omitempty"`
	MCPServers   []*jsonMCPServerDecl    `json:"mcp_servers,omitempty"`
	Prompts      []*jsonPromptDecl       `json:"prompts,omitempty"`
	Schemas      []*jsonSchemaDecl       `json:"schemas,omitempty"`
	Cursors      []*jsonCursorDecl       `json:"cursors,omitempty"`
	Supervisors  []*jsonSupervisorDecl   `json:"supervisors,omitempty"`
	Agents       []*jsonAgentDecl        `json:"agents,omitempty"`
	Judges       []*jsonJudgeDecl        `json:"judges,omitempty"`
	Routers      []*jsonRouterDecl       `json:"routers,omitempty"`
	Humans       []*jsonHumanDecl        `json:"humans,omitempty"`
	Tools        []*jsonToolNodeDecl     `json:"tools,omitempty"`
	Computes     []*jsonComputeDecl      `json:"computes,omitempty"`
	Subbots      []*jsonSubbotDecl       `json:"subbots,omitempty"`
	Emits        []*jsonEmitDecl         `json:"emits,omitempty"`
	Waits        []*jsonWaitDecl         `json:"waits,omitempty"`
	AwaitAnswers []*jsonAwaitAnswersDecl `json:"await_answers,omitempty"`
	Fails        []*jsonFailDecl         `json:"fails,omitempty"`
	Workflows    []*jsonWorkflowDecl     `json:"workflows,omitempty"`
	Comments     []*jsonComment          `json:"comments,omitempty"`
}

type jsonComment struct {
	Text string `json:"text,omitempty"`
}

type jsonVarsBlock struct {
	Fields []*jsonVarField `json:"fields,omitempty"`
}

type jsonVarField struct {
	Name    string       `json:"name,omitempty"`
	Type    string       `json:"type,omitempty"`
	Enum    []string     `json:"enum,omitempty"`
	Default *jsonLiteral `json:"default,omitempty"`
}

type jsonSecretsBlock struct {
	Fields []*jsonSecretField `json:"fields,omitempty"`
}

type jsonSecretField struct {
	Name        string   `json:"name,omitempty"`
	Value       string   `json:"value,omitempty"`
	As          string   `json:"as,omitempty"`
	MountPath   string   `json:"mount_path,omitempty"`
	Env         string   `json:"env,omitempty"`
	Optional    bool     `json:"optional,omitempty"`
	Hosts       []string `json:"hosts,omitempty"`
	Description string   `json:"description,omitempty"`
}

type jsonPresetsBlock struct {
	Entries []*jsonPreset `json:"entries,omitempty"`
}

type jsonPreset struct {
	Name   string             `json:"name,omitempty"`
	Values []*jsonPresetValue `json:"values,omitempty"`
}

type jsonPresetValue struct {
	Key   string       `json:"key,omitempty"`
	Value *jsonLiteral `json:"value,omitempty"`
}

type jsonAttachmentsBlock struct {
	Fields []*jsonAttachmentField `json:"fields,omitempty"`
}

type jsonAttachmentField struct {
	Name        string   `json:"name,omitempty"`
	Type        string   `json:"type,omitempty"` // "file" | "image"
	Required    *bool    `json:"required,omitempty"`
	AcceptMIME  []string `json:"accept_mime,omitempty"`
	Description string   `json:"description,omitempty"`
}

type jsonLiteral struct {
	Kind     string  `json:"kind,omitempty"`
	Raw      string  `json:"raw,omitempty"`
	StrVal   string  `json:"str_val,omitempty"`
	IntVal   int64   `json:"int_val,omitempty"`
	FloatVal float64 `json:"float_val,omitempty"`
	BoolVal  bool    `json:"bool_val,omitempty"`
}

type jsonMCPServerDecl struct {
	Name      string           `json:"name,omitempty"`
	Transport string           `json:"transport,omitempty"`
	Command   string           `json:"command,omitempty"`
	Args      []string         `json:"args,omitempty"`
	URL       string           `json:"url,omitempty"`
	Auth      *jsonMCPAuthDecl `json:"auth,omitempty"`
}

type jsonMCPAuthDecl struct {
	Type      string   `json:"type,omitempty"`
	AuthURL   string   `json:"auth_url,omitempty"`
	TokenURL  string   `json:"token_url,omitempty"`
	RevokeURL string   `json:"revoke_url,omitempty"`
	ClientID  string   `json:"client_id,omitempty"`
	Scopes    []string `json:"scopes,omitempty"`
}

type jsonMCPConfigDecl struct {
	AutoloadProject *bool    `json:"autoload_project,omitempty"`
	Inherit         *bool    `json:"inherit,omitempty"`
	Servers         []string `json:"servers,omitempty"`
	Disable         []string `json:"disable,omitempty"`
}

type jsonCompactionBlock struct {
	Threshold      *float64 `json:"threshold,omitempty"`
	PreserveRecent *int     `json:"preserve_recent,omitempty"`
}

type jsonMemoryBlock struct {
	Enabled          *bool    `json:"enabled,omitempty"`
	Scope            *string  `json:"scope,omitempty"`
	Autoload         []string `json:"autoload,omitempty"`
	Read             *bool    `json:"read,omitempty"`
	Write            *bool    `json:"write,omitempty"`
	PreCompactInject *bool    `json:"pre_compact_inject,omitempty"`
	ProjectRoot      *bool    `json:"project_root,omitempty"`
	Visibility       *string  `json:"visibility,omitempty"`
}

type jsonPromptDecl struct {
	Name string `json:"name,omitempty"`
	Body string `json:"body,omitempty"`
}

type jsonCursorDecl struct {
	Name        string                 `json:"name,omitempty"`
	Description string                 `json:"description,omitempty"`
	Values      []*jsonCursorEnumValue `json:"values,omitempty"`
	Bands       []*jsonCursorBand      `json:"bands,omitempty"`
}

type jsonCursorEnumValue struct {
	Name   string `json:"name,omitempty"`
	Prompt string `json:"prompt,omitempty"`
}

type jsonCursorBand struct {
	Range  string `json:"range,omitempty"`
	Prompt string `json:"prompt,omitempty"`
}

// jsonSupervisorDecl is the wire form of a top-level `supervisor <name>:`
// declaration. The AST JSON codec is the cloud queue's wire format: a
// declaration missing here compiles fine locally and silently vanishes on
// every runner pod (the supervisor never spawns, no skip logged).
type jsonSupervisorDecl struct {
	Name     string   `json:"name,omitempty"`
	Watches  []string `json:"watches,omitempty"`
	Model    string   `json:"model,omitempty"`
	System   string   `json:"system,omitempty"`
	Cooldown string   `json:"cooldown,omitempty"`
	MaxEvals int      `json:"max_evals,omitempty"`
	Monitors []string `json:"monitors,omitempty"`
}

// jsonFallbackDecl is the wire form of one `fallbacks:` route.
// It MUST round-trip in both directions: UnmarshalFile is a plain typed
// json.Unmarshal, so a key missing here is silently discarded — and the
// studio saves every edit through parse → unparse, which would delete
// the block from the .bot on any unrelated change.
type jsonFallbackDecl struct {
	Name     string   `json:"name,omitempty"`
	Backend  string   `json:"backend,omitempty"`
	Model    string   `json:"model,omitempty"`
	Provider string   `json:"provider,omitempty"`
	On       []string `json:"on,omitempty"`
	Metered  bool     `json:"metered,omitempty"`
	Action   string   `json:"action,omitempty"`
	When     string   `json:"when,omitempty"`
}

type jsonCursorBlock struct {
	Enabled  bool                 `json:"enabled"`
	Settings []*jsonCursorSetting `json:"settings,omitempty"`
}

type jsonCursorSetting struct {
	Key   string `json:"key,omitempty"`
	Value string `json:"value,omitempty"`
}

type jsonSchemaDecl struct {
	Name   string             `json:"name,omitempty"`
	Fields []*jsonSchemaField `json:"fields,omitempty"`
}

type jsonSchemaField struct {
	Name       string   `json:"name,omitempty"`
	Type       string   `json:"type,omitempty"`
	EnumValues []string `json:"enum_values,omitempty"`
}

type jsonAgentDecl struct {
	Name              string               `json:"name,omitempty"`
	Description       string               `json:"description,omitempty"`
	Model             string               `json:"model,omitempty"`
	Backend           string               `json:"backend,omitempty"`
	Provider          string               `json:"provider,omitempty"`
	Command           string               `json:"command,omitempty"`
	MCP               *jsonMCPConfigDecl   `json:"mcp,omitempty"`
	Input             string               `json:"input,omitempty"`
	Output            string               `json:"output,omitempty"`
	Publish           string               `json:"publish,omitempty"`
	ArtifactLabels    []string             `json:"artifact_labels,omitempty"`
	System            string               `json:"system,omitempty"`
	User              string               `json:"user,omitempty"`
	Session           string               `json:"session,omitempty"`
	Tools             []string             `json:"tools,omitempty"`
	ToolPolicy        []string             `json:"tool_policy,omitempty"`
	Capabilities      []string             `json:"capabilities,omitempty"`
	Skills            []string             `json:"skills,omitempty"`
	ToolMaxSteps      int                  `json:"tool_max_steps,omitempty"`
	MaxTokens         int                  `json:"max_tokens,omitempty"`
	ReasoningEffort   string               `json:"reasoning_effort,omitempty"`
	Timeout           string               `json:"timeout,omitempty"`
	Readonly          bool                 `json:"readonly,omitempty"`
	FullAccess        bool                 `json:"full_access,omitempty"`
	Images            []string             `json:"images,omitempty"`
	Interaction       string               `json:"interaction,omitempty"`
	InteractionPrompt string               `json:"interaction_prompt,omitempty"`
	InteractionModel  string               `json:"interaction_model,omitempty"`
	Await             string               `json:"await,omitempty"`
	Compaction        *jsonCompactionBlock `json:"compaction,omitempty"`
	Memory            *jsonMemoryBlock     `json:"memory,omitempty"`
	Sandbox           *jsonSandboxBlock    `json:"sandbox,omitempty"`
	Cursors           *jsonCursorBlock     `json:"cursors,omitempty"`
	Fallbacks         []*jsonFallbackDecl  `json:"fallbacks,omitempty"`
	Compress          string               `json:"compress,omitempty"`
	AutoMemory        string               `json:"auto_memory,omitempty"`
	Permission        string               `json:"permission,omitempty"`
	Needs             []string             `json:"needs,omitempty"`
}

type jsonJudgeDecl struct {
	Name              string               `json:"name,omitempty"`
	Description       string               `json:"description,omitempty"`
	Model             string               `json:"model,omitempty"`
	Backend           string               `json:"backend,omitempty"`
	Provider          string               `json:"provider,omitempty"`
	Command           string               `json:"command,omitempty"`
	MCP               *jsonMCPConfigDecl   `json:"mcp,omitempty"`
	Input             string               `json:"input,omitempty"`
	Output            string               `json:"output,omitempty"`
	Publish           string               `json:"publish,omitempty"`
	ArtifactLabels    []string             `json:"artifact_labels,omitempty"`
	System            string               `json:"system,omitempty"`
	User              string               `json:"user,omitempty"`
	Session           string               `json:"session,omitempty"`
	Tools             []string             `json:"tools,omitempty"`
	ToolPolicy        []string             `json:"tool_policy,omitempty"`
	Capabilities      []string             `json:"capabilities,omitempty"`
	Skills            []string             `json:"skills,omitempty"`
	ToolMaxSteps      int                  `json:"tool_max_steps,omitempty"`
	MaxTokens         int                  `json:"max_tokens,omitempty"`
	ReasoningEffort   string               `json:"reasoning_effort,omitempty"`
	Timeout           string               `json:"timeout,omitempty"`
	Readonly          bool                 `json:"readonly,omitempty"`
	FullAccess        bool                 `json:"full_access,omitempty"`
	Images            []string             `json:"images,omitempty"`
	Interaction       string               `json:"interaction,omitempty"`
	InteractionPrompt string               `json:"interaction_prompt,omitempty"`
	InteractionModel  string               `json:"interaction_model,omitempty"`
	Await             string               `json:"await,omitempty"`
	Compaction        *jsonCompactionBlock `json:"compaction,omitempty"`
	Memory            *jsonMemoryBlock     `json:"memory,omitempty"`
	Sandbox           *jsonSandboxBlock    `json:"sandbox,omitempty"`
	Cursors           *jsonCursorBlock     `json:"cursors,omitempty"`
	Fallbacks         []*jsonFallbackDecl  `json:"fallbacks,omitempty"`
	Compress          string               `json:"compress,omitempty"`
	AutoMemory        string               `json:"auto_memory,omitempty"`
	Permission        string               `json:"permission,omitempty"`
	Needs             []string             `json:"needs,omitempty"`
}

type jsonRouterDecl struct {
	Name            string   `json:"name,omitempty"`
	Description     string   `json:"description,omitempty"`
	Mode            string   `json:"mode,omitempty"`
	Model           string   `json:"model,omitempty"`
	Backend         string   `json:"backend,omitempty"`
	Provider        string   `json:"provider,omitempty"`
	System          string   `json:"system,omitempty"`
	User            string   `json:"user,omitempty"`
	Multi           bool     `json:"multi,omitempty"`
	ReasoningEffort string   `json:"reasoning_effort,omitempty"`
	Over            string   `json:"over,omitempty"`
	As              string   `json:"as,omitempty"`
	Key             string   `json:"key,omitempty"`
	DependsOn       string   `json:"depends_on,omitempty"`
	Needs           []string `json:"needs,omitempty"`
}

type jsonHumanDecl struct {
	Name              string   `json:"name,omitempty"`
	Description       string   `json:"description,omitempty"`
	Input             string   `json:"input,omitempty"`
	Output            string   `json:"output,omitempty"`
	Publish           string   `json:"publish,omitempty"`
	ArtifactLabels    []string `json:"artifact_labels,omitempty"`
	Instructions      string   `json:"instructions,omitempty"`
	Interaction       string   `json:"interaction,omitempty"`
	InteractionPrompt string   `json:"interaction_prompt,omitempty"`
	InteractionModel  string   `json:"interaction_model,omitempty"`
	MinAnswers        int      `json:"min_answers,omitempty"`
	Model             string   `json:"model,omitempty"`
	System            string   `json:"system,omitempty"`
	Await             string   `json:"await,omitempty"`
	ReviewURL         string   `json:"review_url,omitempty"`
	Posture           string   `json:"posture,omitempty"`
	MergeStrategy     string   `json:"merge_strategy,omitempty"`
	MergeInto         string   `json:"merge_into,omitempty"`
	MaxTurns          int      `json:"max_turns,omitempty"`
}

type jsonToolNodeDecl struct {
	Name           string             `json:"name,omitempty"`
	Description    string             `json:"description,omitempty"`
	Command        string             `json:"command,omitempty"`
	Script         string             `json:"script,omitempty"`
	Language       string             `json:"language,omitempty"`
	Input          string             `json:"input,omitempty"`
	Output         string             `json:"output,omitempty"`
	Publish        string             `json:"publish,omitempty"`
	ArtifactLabels []string           `json:"artifact_labels,omitempty"`
	Await          string             `json:"await,omitempty"`
	Sandbox        *jsonSandboxBlock  `json:"sandbox,omitempty"`
	Compress       string             `json:"compress,omitempty"`
	Permission     string             `json:"permission,omitempty"`
	Goal           string             `json:"goal,omitempty"`
	Postcondition  string             `json:"postcondition,omitempty"`
	Policy         string             `json:"policy,omitempty"`
	Recovery       *jsonRecoveryBlock `json:"recovery,omitempty"`
	Needs          []string           `json:"needs,omitempty"`
	ParallelSafe   bool               `json:"parallel_safe,omitempty"`
}

// jsonRecoveryBlock is the JSON form of an ast.RecoveryBlock (ADR-044).
type jsonRecoveryBlock struct {
	MaxRepairAttempts int      `json:"max_repair_attempts,omitempty"`
	MaxAgentAttempts  int      `json:"max_agent_attempts,omitempty"`
	Model             string   `json:"model,omitempty"`
	AgentTools        []string `json:"agent_tools,omitempty"`
}

// jsonSandboxBlock is the JSON form of an ast.SandboxBlock. The
// studio consumes this shape.
type jsonSandboxBlock struct {
	Mode            string                   `json:"mode,omitempty"`
	Image           string                   `json:"image,omitempty"`
	Build           *jsonSandboxBuildBlock   `json:"build,omitempty"`
	User            string                   `json:"user,omitempty"`
	WorkspaceFolder string                   `json:"workspace_folder,omitempty"`
	HostState       string                   `json:"host_state,omitempty"`
	PostCreate      string                   `json:"post_create,omitempty"`
	Env             map[string]string        `json:"env,omitempty"`
	Mounts          []string                 `json:"mounts,omitempty"`
	Network         *jsonSandboxNetworkBlock `json:"network,omitempty"`
}

// jsonSandboxBuildBlock is the JSON form of an ast.SandboxBuildBlock.
type jsonSandboxBuildBlock struct {
	Dockerfile string            `json:"dockerfile,omitempty"`
	Context    string            `json:"context,omitempty"`
	Args       map[string]string `json:"args,omitempty"`
}

// jsonSandboxNetworkBlock is the JSON form of an ast.SandboxNetworkBlock.
type jsonSandboxNetworkBlock struct {
	Mode    string   `json:"mode,omitempty"`
	Preset  string   `json:"preset,omitempty"`
	Rules   []string `json:"rules,omitempty"`
	Inherit string   `json:"inherit,omitempty"`
}

func recoveryBlockToJSON(r *RecoveryBlock) *jsonRecoveryBlock {
	if r == nil {
		return nil
	}
	return &jsonRecoveryBlock{
		MaxRepairAttempts: r.MaxRepairAttempts,
		MaxAgentAttempts:  r.MaxAgentAttempts,
		Model:             r.Model,
		AgentTools:        r.AgentTools,
	}
}

func recoveryBlockFromJSON(j *jsonRecoveryBlock) *RecoveryBlock {
	if j == nil {
		return nil
	}
	return &RecoveryBlock{
		MaxRepairAttempts: j.MaxRepairAttempts,
		MaxAgentAttempts:  j.MaxAgentAttempts,
		Model:             j.Model,
		AgentTools:        j.AgentTools,
	}
}

func sandboxBlockToJSON(s *SandboxBlock) *jsonSandboxBlock {
	if s == nil {
		return nil
	}
	return &jsonSandboxBlock{
		Mode:            s.Mode,
		Image:           s.Image,
		Build:           sandboxBuildBlockToJSON(s.Build),
		User:            s.User,
		WorkspaceFolder: s.WorkspaceFolder,
		HostState:       s.HostState,
		PostCreate:      s.PostCreate,
		Env:             s.Env,
		Mounts:          s.Mounts,
		Network:         sandboxNetworkBlockToJSON(s.Network),
	}
}

func sandboxBuildBlockToJSON(b *SandboxBuildBlock) *jsonSandboxBuildBlock {
	if b == nil {
		return nil
	}
	return &jsonSandboxBuildBlock{
		Dockerfile: b.Dockerfile,
		Context:    b.Context,
		Args:       b.Args,
	}
}

func sandboxNetworkBlockToJSON(n *SandboxNetworkBlock) *jsonSandboxNetworkBlock {
	if n == nil {
		return nil
	}
	return &jsonSandboxNetworkBlock{
		Mode:    n.Mode,
		Preset:  n.Preset,
		Rules:   n.Rules,
		Inherit: n.Inherit,
	}
}

func sandboxBlockFromJSON(j *jsonSandboxBlock) *SandboxBlock {
	if j == nil {
		return nil
	}
	return &SandboxBlock{
		Mode:            j.Mode,
		Image:           j.Image,
		Build:           sandboxBuildBlockFromJSON(j.Build),
		User:            j.User,
		WorkspaceFolder: j.WorkspaceFolder,
		HostState:       j.HostState,
		PostCreate:      j.PostCreate,
		Env:             j.Env,
		Mounts:          j.Mounts,
		Network:         sandboxNetworkBlockFromJSON(j.Network),
	}
}

func sandboxBuildBlockFromJSON(j *jsonSandboxBuildBlock) *SandboxBuildBlock {
	if j == nil {
		return nil
	}
	return &SandboxBuildBlock{
		Dockerfile: j.Dockerfile,
		Context:    j.Context,
		Args:       j.Args,
	}
}

func sandboxNetworkBlockFromJSON(j *jsonSandboxNetworkBlock) *SandboxNetworkBlock {
	if j == nil {
		return nil
	}
	return &SandboxNetworkBlock{
		Mode:    j.Mode,
		Preset:  j.Preset,
		Rules:   j.Rules,
		Inherit: j.Inherit,
	}
}

type jsonComputeDecl struct {
	Name           string             `json:"name,omitempty"`
	Description    string             `json:"description,omitempty"`
	Input          string             `json:"input,omitempty"`
	Output         string             `json:"output,omitempty"`
	Publish        string             `json:"publish,omitempty"`
	ArtifactLabels []string           `json:"artifact_labels,omitempty"`
	Expr           []*jsonComputeExpr `json:"expr,omitempty"`
	Await          string             `json:"await,omitempty"`
}

type jsonComputeExpr struct {
	Key  string `json:"key,omitempty"`
	Expr string `json:"expr,omitempty"`
}

type jsonSubbotDecl struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Source      string           `json:"source,omitempty"`
	With        []*jsonWithEntry `json:"with,omitempty"`
	Output      string           `json:"output,omitempty"`
	Needs       []string         `json:"needs,omitempty"`
	Isolated    bool             `json:"isolated,omitempty"`
}

type jsonEmitDecl struct {
	Name        string           `json:"name,omitempty"`
	Description string           `json:"description,omitempty"`
	Event       string           `json:"event,omitempty"`
	With        []*jsonWithEntry `json:"with,omitempty"`
}

type jsonWaitDecl struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Event       string `json:"event,omitempty"`
	Timeout     string `json:"timeout,omitempty"`
	Output      string `json:"output,omitempty"`
}

type jsonAwaitAnswersDecl struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	From        string `json:"from,omitempty"`
	Timeout     string `json:"timeout,omitempty"`
}

type jsonFailDecl struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Code        string `json:"code,omitempty"`
	Message     string `json:"message,omitempty"`
	Resumable   bool   `json:"resumable,omitempty"`
}

type jsonWorkflowDecl struct {
	Name            string                `json:"name,omitempty"`
	Vars            *jsonVarsBlock        `json:"vars,omitempty"`
	Attachments     *jsonAttachmentsBlock `json:"attachments,omitempty"`
	Entry           string                `json:"entry,omitempty"`
	DefaultBackend  string                `json:"default_backend,omitempty"`
	ToolPolicy      []string              `json:"tool_policy,omitempty"`
	Capabilities    []string              `json:"capabilities,omitempty"`
	Skills          []string              `json:"skills,omitempty"`
	MCP             *jsonMCPConfigDecl    `json:"mcp,omitempty"`
	Budget          *jsonBudgetBlock      `json:"budget,omitempty"`
	Resources       map[string]int        `json:"resources,omitempty"`
	Compaction      *jsonCompactionBlock  `json:"compaction,omitempty"`
	Interaction     string                `json:"interaction,omitempty"`
	Worktree        string                `json:"worktree,omitempty"`
	Compress        string                `json:"compress,omitempty"`
	AutoMemory      string                `json:"auto_memory,omitempty"`
	LoopBudgetGuard string                `json:"loop_budget_guard,omitempty"`
	RepoDevbox      string                `json:"repo_devbox,omitempty"`
	Permission      string                `json:"permission,omitempty"`
	Allow           []string              `json:"allow,omitempty"`
	Ask             []string              `json:"ask,omitempty"`
	Deny            []string              `json:"deny,omitempty"`
	Sandbox         *jsonSandboxBlock     `json:"sandbox,omitempty"`
	Edges           []*jsonEdge           `json:"edges,omitempty"`
}

type jsonBudgetBlock struct {
	MaxParallelBranches int     `json:"max_parallel_branches,omitempty"`
	MaxDuration         string  `json:"max_duration,omitempty"`
	MaxCostUSD          float64 `json:"max_cost_usd,omitempty"`
	MaxTokens           int     `json:"max_tokens,omitempty"`
	WarnTokens          int     `json:"warn_tokens,omitempty"`
	MaxIterations       int     `json:"max_iterations,omitempty"`
}

type jsonEdge struct {
	From   string           `json:"from,omitempty"`
	To     string           `json:"to,omitempty"`
	When   *jsonWhenClause  `json:"when,omitempty"`
	IsElse bool             `json:"is_else,omitempty"`
	Loop   *jsonLoopClause  `json:"loop,omitempty"`
	With   []*jsonWithEntry `json:"with,omitempty"`
}

type jsonWhenClause struct {
	Condition string `json:"condition,omitempty"`
	Negated   bool   `json:"negated,omitempty"`
	Expr      string `json:"expr,omitempty"`
}

type jsonLoopClause struct {
	Name              string `json:"name,omitempty"`
	MaxIterations     int    `json:"max_iterations,omitempty"`
	MaxIterationsExpr string `json:"max_iterations_expr,omitempty"`
	Unbounded         bool   `json:"unbounded,omitempty"`
	FuelCap           int    `json:"fuel_cap,omitempty"`
}

type jsonWithEntry struct {
	Key   string `json:"key,omitempty"`
	Value string `json:"value,omitempty"`
}

// ---------------------------------------------------------------------------
// Marshal
// ---------------------------------------------------------------------------

// Marshal converts an File to JSON with human-readable string enums.
// Span fields are omitted from the output.
func MarshalFile(f *File) ([]byte, error) {
	jf := toJSON(f)
	return json.MarshalIndent(jf, "", "  ")
}

func toJSON(f *File) *jsonFile {
	if f == nil {
		return nil
	}
	jf := &jsonFile{}

	if f.Vars != nil {
		jf.Vars = varsBlockToJSON(f.Vars)
	}
	if f.Presets != nil {
		jf.Presets = presetsBlockToJSON(f.Presets)
	}
	if f.Secrets != nil {
		jf.Secrets = secretsBlockToJSON(f.Secrets)
	}
	if f.Attachments != nil {
		jf.Attachments = attachmentsBlockToJSON(f.Attachments)
	}

	for _, s := range f.MCPServers {
		jf.MCPServers = append(jf.MCPServers, mcpServerToJSON(s))
	}
	for _, p := range f.Prompts {
		jf.Prompts = append(jf.Prompts, &jsonPromptDecl{Name: p.Name, Body: p.Body})
	}
	for _, s := range f.Schemas {
		jf.Schemas = append(jf.Schemas, schemaToJSON(s))
	}
	for _, c := range f.Cursors {
		jf.Cursors = append(jf.Cursors, cursorDeclToJSON(c))
	}
	for _, s := range f.Supervisors {
		jf.Supervisors = append(jf.Supervisors, &jsonSupervisorDecl{
			Name:     s.Name,
			Watches:  s.Watches,
			Model:    s.Model,
			System:   s.System,
			Cooldown: s.Cooldown,
			MaxEvals: s.MaxEvals,
			Monitors: s.Monitors,
		})
	}
	for _, a := range f.Agents {
		jf.Agents = append(jf.Agents, agentToJSON(a))
	}
	for _, j := range f.Judges {
		jf.Judges = append(jf.Judges, judgeToJSON(j))
	}
	for _, r := range f.Routers {
		jf.Routers = append(jf.Routers, &jsonRouterDecl{
			Name:            r.Name,
			Description:     r.Description,
			Mode:            routerModeToStr[r.Mode],
			Model:           r.Model,
			Backend:         r.Backend,
			Provider:        r.Provider,
			System:          r.System,
			User:            r.User,
			Multi:           r.Multi,
			ReasoningEffort: r.ReasoningEffort,
			Over:            r.Over,
			As:              r.As,
			Key:             r.Key,
			DependsOn:       r.DependsOn,
			Needs:           r.Needs,
		})
	}
	for _, h := range f.Humans {
		jf.Humans = append(jf.Humans, humanToJSON(h))
	}
	for _, t := range f.Tools {
		jf.Tools = append(jf.Tools, &jsonToolNodeDecl{
			Name:           t.Name,
			Description:    t.Description,
			Command:        t.Command,
			Script:         t.Script,
			Language:       t.Language,
			Input:          t.Input,
			Output:         t.Output,
			Publish:        t.Publish,
			ArtifactLabels: t.ArtifactLabels,
			Await:          awaitModeToStr[t.Await],
			Sandbox:        sandboxBlockToJSON(t.Sandbox),
			Compress:       t.Compress,
			Permission:     t.Permission,
			Goal:           t.Goal,
			Postcondition:  t.Postcondition,
			Policy:         t.Policy,
			Recovery:       recoveryBlockToJSON(t.Recovery),
			Needs:          t.Needs,
			ParallelSafe:   t.ParallelSafe,
		})
	}
	for _, c := range f.Computes {
		jc := &jsonComputeDecl{
			Name:           c.Name,
			Description:    c.Description,
			Input:          c.Input,
			Output:         c.Output,
			Publish:        c.Publish,
			ArtifactLabels: c.ArtifactLabels,
			Await:          awaitModeToStr[c.Await],
		}
		for _, e := range c.Expr {
			jc.Expr = append(jc.Expr, &jsonComputeExpr{Key: e.Key, Expr: e.Expr})
		}
		jf.Computes = append(jf.Computes, jc)
	}
	for _, s := range f.Subbots {
		js := &jsonSubbotDecl{
			Name:        s.Name,
			Description: s.Description,
			Source:      s.Source,
			Output:      s.Output,
			Needs:       s.Needs,
			Isolated:    s.Isolated,
		}
		for _, w := range s.With {
			js.With = append(js.With, &jsonWithEntry{Key: w.Key, Value: w.Value})
		}
		jf.Subbots = append(jf.Subbots, js)
	}
	for _, em := range f.Emits {
		je := &jsonEmitDecl{Name: em.Name, Description: em.Description, Event: em.Event}
		for _, w := range em.With {
			je.With = append(je.With, &jsonWithEntry{Key: w.Key, Value: w.Value})
		}
		jf.Emits = append(jf.Emits, je)
	}
	for _, wt := range f.Waits {
		jf.Waits = append(jf.Waits, &jsonWaitDecl{
			Name:        wt.Name,
			Description: wt.Description,
			Event:       wt.Event,
			Timeout:     wt.Timeout,
			Output:      wt.Output,
		})
	}
	for _, aa := range f.AwaitAnswers {
		jf.AwaitAnswers = append(jf.AwaitAnswers, &jsonAwaitAnswersDecl{
			Name:        aa.Name,
			Description: aa.Description,
			From:        aa.From,
			Timeout:     aa.Timeout,
		})
	}
	for _, fd := range f.Fails {
		jf.Fails = append(jf.Fails, &jsonFailDecl{
			Name:        fd.Name,
			Description: fd.Description,
			Code:        fd.Code,
			Message:     fd.Message,
			Resumable:   fd.Resumable,
		})
	}
	for _, w := range f.Workflows {
		jf.Workflows = append(jf.Workflows, workflowToJSON(w))
	}
	for _, c := range f.Comments {
		jf.Comments = append(jf.Comments, &jsonComment{Text: c.Text})
	}

	return jf
}

func mcpServerToJSON(s *MCPServerDecl) *jsonMCPServerDecl {
	js := &jsonMCPServerDecl{
		Name:      s.Name,
		Transport: mcpTransportToStr[s.Transport],
		Command:   s.Command,
		Args:      s.Args,
		URL:       s.URL,
	}
	if s.Auth != nil {
		js.Auth = &jsonMCPAuthDecl{
			Type:      s.Auth.Type,
			AuthURL:   s.Auth.AuthURL,
			TokenURL:  s.Auth.TokenURL,
			RevokeURL: s.Auth.RevokeURL,
			ClientID:  s.Auth.ClientID,
			Scopes:    s.Auth.Scopes,
		}
	}
	return js
}

func mcpConfigToJSON(c *MCPConfigDecl) *jsonMCPConfigDecl {
	if c == nil {
		return nil
	}
	return &jsonMCPConfigDecl{
		AutoloadProject: c.AutoloadProject,
		Inherit:         c.Inherit,
		Servers:         c.Servers,
		Disable:         c.Disable,
	}
}

func compactionToJSON(c *CompactionBlock) *jsonCompactionBlock {
	if c == nil {
		return nil
	}
	return &jsonCompactionBlock{Threshold: c.Threshold, PreserveRecent: c.PreserveRecent}
}

func cursorDeclToJSON(c *CursorDecl) *jsonCursorDecl {
	jc := &jsonCursorDecl{Name: c.Name, Description: c.Description}
	for _, v := range c.Values {
		jc.Values = append(jc.Values, &jsonCursorEnumValue{Name: v.Name, Prompt: v.Prompt})
	}
	for _, b := range c.Bands {
		jc.Bands = append(jc.Bands, &jsonCursorBand{Range: b.Range, Prompt: b.Prompt})
	}
	return jc
}

func cursorDeclFromJSON(jc *jsonCursorDecl) *CursorDecl {
	if jc == nil {
		return nil
	}
	c := &CursorDecl{Name: jc.Name, Description: jc.Description}
	for _, v := range jc.Values {
		c.Values = append(c.Values, &CursorEnumValue{Name: v.Name, Prompt: v.Prompt})
	}
	for _, b := range jc.Bands {
		c.Bands = append(c.Bands, &CursorBand{Range: b.Range, Prompt: b.Prompt})
	}
	return c
}

func cursorBlockToJSON(b *CursorBlock) *jsonCursorBlock {
	if b == nil {
		return nil
	}
	jb := &jsonCursorBlock{Enabled: b.Enabled}
	for _, s := range b.Settings {
		jb.Settings = append(jb.Settings, &jsonCursorSetting{Key: s.Key, Value: s.Value})
	}
	return jb
}

func cursorBlockFromJSON(jb *jsonCursorBlock) *CursorBlock {
	if jb == nil {
		return nil
	}
	b := &CursorBlock{Enabled: jb.Enabled}
	for _, js := range jb.Settings {
		b.Settings = append(b.Settings, &CursorSetting{Key: js.Key, Value: js.Value})
	}
	return b
}

func fallbacksToJSON(fbs []*FallbackDecl) []*jsonFallbackDecl {
	if len(fbs) == 0 {
		return nil
	}
	out := make([]*jsonFallbackDecl, 0, len(fbs))
	for _, f := range fbs {
		if f == nil {
			continue
		}
		out = append(out, &jsonFallbackDecl{
			Name: f.Name, Backend: f.Backend, Model: f.Model,
			Provider: f.Provider, On: f.On, Metered: f.Metered,
			Action: f.Action, When: f.When,
		})
	}
	return out
}

func fallbacksFromJSON(jfbs []*jsonFallbackDecl) []*FallbackDecl {
	if len(jfbs) == 0 {
		return nil
	}
	out := make([]*FallbackDecl, 0, len(jfbs))
	for _, jf := range jfbs {
		if jf == nil {
			continue
		}
		out = append(out, &FallbackDecl{
			Name: jf.Name, Backend: jf.Backend, Model: jf.Model,
			Provider: jf.Provider, On: jf.On, Metered: jf.Metered,
			Action: jf.Action, When: jf.When,
		})
	}
	return out
}

func memoryToJSON(m *MemoryBlock) *jsonMemoryBlock {
	if m == nil {
		return nil
	}
	return &jsonMemoryBlock{
		Enabled:          m.Enabled,
		Scope:            m.Scope,
		Autoload:         m.Autoload,
		Read:             m.Read,
		Write:            m.Write,
		PreCompactInject: m.PreCompactInject,
		ProjectRoot:      m.ProjectRoot,
		Visibility:       m.Visibility,
	}
}

func attachmentsBlockToJSON(a *AttachmentsBlock) *jsonAttachmentsBlock {
	if a == nil {
		return nil
	}
	jb := &jsonAttachmentsBlock{}
	for _, f := range a.Fields {
		jf := &jsonAttachmentField{
			Name:        f.Name,
			Type:        f.Type.String(),
			AcceptMIME:  f.AcceptMIME,
			Description: f.Description,
		}
		if f.Required != nil {
			jf.Required = f.Required
		}
		jb.Fields = append(jb.Fields, jf)
	}
	return jb
}

func attachmentsBlockFromJSON(jb *jsonAttachmentsBlock) (*AttachmentsBlock, error) {
	if jb == nil {
		return nil, nil
	}
	a := &AttachmentsBlock{}
	for _, jf := range jb.Fields {
		var t AttachmentTypeExpr
		switch jf.Type {
		case "file":
			t = AttachmentTypeFile
		case "image":
			t = AttachmentTypeImage
		default:
			return nil, fmt.Errorf("astjson: unknown attachment type %q", jf.Type)
		}
		af := &AttachmentField{
			Name:        jf.Name,
			Type:        t,
			AcceptMIME:  jf.AcceptMIME,
			Description: jf.Description,
		}
		if jf.Required != nil {
			v := *jf.Required
			af.Required = &v
		}
		a.Fields = append(a.Fields, af)
	}
	return a, nil
}

func presetsBlockToJSON(p *PresetsBlock) *jsonPresetsBlock {
	jp := &jsonPresetsBlock{}
	for _, e := range p.Entries {
		je := &jsonPreset{Name: e.Name}
		for _, v := range e.Values {
			jv := &jsonPresetValue{Key: v.Key}
			if v.Value != nil {
				jv.Value = literalToJSON(v.Value)
			}
			je.Values = append(je.Values, jv)
		}
		jp.Entries = append(jp.Entries, je)
	}
	return jp
}

func presetsBlockFromJSON(jp *jsonPresetsBlock) (*PresetsBlock, error) {
	p := &PresetsBlock{}
	for _, je := range jp.Entries {
		e := &Preset{Name: je.Name}
		for _, jv := range je.Values {
			pv := &PresetValue{Key: jv.Key}
			if jv.Value != nil {
				lit, err := literalFromJSON(jv.Value)
				if err != nil {
					return nil, err
				}
				pv.Value = lit
			}
			e.Values = append(e.Values, pv)
		}
		p.Entries = append(p.Entries, e)
	}
	return p, nil
}

func varsBlockToJSON(v *VarsBlock) *jsonVarsBlock {
	jv := &jsonVarsBlock{}
	for _, f := range v.Fields {
		jf := &jsonVarField{
			Name: f.Name,
			Type: typeExprToStr[f.Type],
			Enum: f.EnumValues,
		}
		if f.Default != nil {
			jf.Default = literalToJSON(f.Default)
		}
		jv.Fields = append(jv.Fields, jf)
	}
	return jv
}

func secretsBlockToJSON(s *SecretsBlock) *jsonSecretsBlock {
	js := &jsonSecretsBlock{}
	for _, f := range s.Fields {
		js.Fields = append(js.Fields, &jsonSecretField{
			Name:        f.Name,
			Value:       f.Value,
			As:          f.As,
			MountPath:   f.MountPath,
			Env:         f.Env,
			Optional:    f.Optional,
			Hosts:       f.Hosts,
			Description: f.Description,
		})
	}
	return js
}

func literalToJSON(l *Literal) *jsonLiteral {
	return &jsonLiteral{
		Kind:     literalKindToStr[l.Kind],
		Raw:      l.Raw,
		StrVal:   l.StrVal,
		IntVal:   l.IntVal,
		FloatVal: l.FloatVal,
		BoolVal:  l.BoolVal,
	}
}

func schemaToJSON(s *SchemaDecl) *jsonSchemaDecl {
	js := &jsonSchemaDecl{Name: s.Name}
	for _, f := range s.Fields {
		js.Fields = append(js.Fields, &jsonSchemaField{
			Name:       f.Name,
			Type:       fieldTypeToStr[f.Type],
			EnumValues: f.EnumValues,
		})
	}
	return js
}

func agentToJSON(a *AgentDecl) *jsonAgentDecl {
	return &jsonAgentDecl{
		Name:              a.Name,
		Description:       a.Description,
		Model:             a.Model,
		Backend:           a.Backend,
		Provider:          a.Provider,
		Command:           a.Command,
		MCP:               mcpConfigToJSON(a.MCP),
		Input:             a.Input,
		Output:            a.Output,
		Publish:           a.Publish,
		ArtifactLabels:    a.ArtifactLabels,
		System:            a.System,
		User:              a.User,
		Session:           sessionModeToStr[a.Session],
		Tools:             a.Tools,
		ToolPolicy:        a.ToolPolicy,
		Capabilities:      a.Capabilities,
		Skills:            a.Skills,
		ToolMaxSteps:      a.ToolMaxSteps,
		MaxTokens:         a.MaxTokens,
		ReasoningEffort:   a.ReasoningEffort,
		Timeout:           a.Timeout,
		Readonly:          a.Readonly,
		FullAccess:        a.FullAccess,
		Images:            a.Images,
		Interaction:       interactionModeToStr[a.Interaction],
		InteractionPrompt: a.InteractionPrompt,
		InteractionModel:  a.InteractionModel,
		Await:             awaitModeToStr[a.Await],
		Compaction:        compactionToJSON(a.Compaction),
		Memory:            memoryToJSON(a.Memory),
		Sandbox:           sandboxBlockToJSON(a.Sandbox),
		Cursors:           cursorBlockToJSON(a.Cursors),
		Fallbacks:         fallbacksToJSON(a.Fallbacks),
		Compress:          a.Compress,
		AutoMemory:        a.AutoMemory,
		Permission:        a.Permission,
		Needs:             a.Needs,
	}
}

func judgeToJSON(j *JudgeDecl) *jsonJudgeDecl {
	return &jsonJudgeDecl{
		Name:              j.Name,
		Description:       j.Description,
		Model:             j.Model,
		Backend:           j.Backend,
		Provider:          j.Provider,
		Command:           j.Command,
		MCP:               mcpConfigToJSON(j.MCP),
		Input:             j.Input,
		Output:            j.Output,
		Publish:           j.Publish,
		ArtifactLabels:    j.ArtifactLabels,
		System:            j.System,
		User:              j.User,
		Session:           sessionModeToStr[j.Session],
		Tools:             j.Tools,
		ToolPolicy:        j.ToolPolicy,
		Capabilities:      j.Capabilities,
		Skills:            j.Skills,
		ToolMaxSteps:      j.ToolMaxSteps,
		MaxTokens:         j.MaxTokens,
		ReasoningEffort:   j.ReasoningEffort,
		Timeout:           j.Timeout,
		Readonly:          j.Readonly,
		FullAccess:        j.FullAccess,
		Images:            j.Images,
		Interaction:       interactionModeToStr[j.Interaction],
		InteractionPrompt: j.InteractionPrompt,
		InteractionModel:  j.InteractionModel,
		Await:             awaitModeToStr[j.Await],
		Compaction:        compactionToJSON(j.Compaction),
		Memory:            memoryToJSON(j.Memory),
		Sandbox:           sandboxBlockToJSON(j.Sandbox),
		Cursors:           cursorBlockToJSON(j.Cursors),
		Fallbacks:         fallbacksToJSON(j.Fallbacks),
		Compress:          j.Compress,
		AutoMemory:        j.AutoMemory,
		Permission:        j.Permission,
		Needs:             j.Needs,
	}
}

func humanToJSON(h *HumanDecl) *jsonHumanDecl {
	return &jsonHumanDecl{
		Name:              h.Name,
		Description:       h.Description,
		Input:             h.Input,
		Output:            h.Output,
		Publish:           h.Publish,
		ArtifactLabels:    h.ArtifactLabels,
		Instructions:      h.Instructions,
		Interaction:       interactionModeToStr[h.Interaction],
		InteractionPrompt: h.InteractionPrompt,
		InteractionModel:  h.InteractionModel,
		MinAnswers:        h.MinAnswers,
		Model:             h.Model,
		System:            h.System,
		Await:             awaitModeToStr[h.Await],
		ReviewURL:         h.ReviewURL,
		Posture:           h.Posture,
		MergeStrategy:     h.MergeStrategy,
		MergeInto:         h.MergeInto,
		MaxTurns:          h.MaxTurns,
	}
}

func workflowToJSON(w *WorkflowDecl) *jsonWorkflowDecl {
	jw := &jsonWorkflowDecl{
		Name:            w.Name,
		Entry:           w.Entry,
		DefaultBackend:  w.DefaultBackend,
		ToolPolicy:      w.ToolPolicy,
		Capabilities:    w.Capabilities,
		Skills:          w.Skills,
		MCP:             mcpConfigToJSON(w.MCP),
		Compaction:      compactionToJSON(w.Compaction),
		Worktree:        w.Worktree,
		Compress:        w.Compress,
		AutoMemory:      w.AutoMemory,
		LoopBudgetGuard: w.LoopBudgetGuard,
		RepoDevbox:      w.RepoDevbox,
		Permission:      w.Permission,
		Allow:           w.Allow,
		Ask:             w.Ask,
		Deny:            w.Deny,
		Sandbox:         sandboxBlockToJSON(w.Sandbox),
	}
	if w.Vars != nil {
		jw.Vars = varsBlockToJSON(w.Vars)
	}
	if w.Attachments != nil {
		jw.Attachments = attachmentsBlockToJSON(w.Attachments)
	}
	if w.Interaction != nil {
		jw.Interaction = interactionModeToStr[*w.Interaction]
	}
	if w.Budget != nil {
		jw.Budget = &jsonBudgetBlock{
			MaxParallelBranches: w.Budget.MaxParallelBranches,
			MaxDuration:         w.Budget.MaxDuration,
			MaxCostUSD:          w.Budget.MaxCostUSD,
			MaxTokens:           w.Budget.MaxTokens,
			WarnTokens:          w.Budget.WarnTokens,
			MaxIterations:       w.Budget.MaxIterations,
		}
	}
	if w.Resources != nil && len(w.Resources.Capacities) > 0 {
		jw.Resources = w.Resources.Capacities
	}
	for _, e := range w.Edges {
		jw.Edges = append(jw.Edges, edgeToJSON(e))
	}
	return jw
}

func edgeToJSON(e *Edge) *jsonEdge {
	je := &jsonEdge{
		From:   e.From,
		To:     e.To,
		IsElse: e.IsElse,
	}
	if e.When != nil {
		je.When = &jsonWhenClause{
			Condition: e.When.Condition,
			Negated:   e.When.Negated,
			Expr:      e.When.Expr,
		}
	}
	if e.Loop != nil {
		je.Loop = &jsonLoopClause{
			Name:              e.Loop.Name,
			MaxIterations:     e.Loop.MaxIterations,
			MaxIterationsExpr: e.Loop.MaxIterationsExpr,
			Unbounded:         e.Loop.Unbounded,
			FuelCap:           e.Loop.FuelCap,
		}
	}
	for _, w := range e.With {
		je.With = append(je.With, &jsonWithEntry{
			Key:   w.Key,
			Value: w.Value,
		})
	}
	return je
}

// ---------------------------------------------------------------------------
// Unmarshal
// ---------------------------------------------------------------------------

// Unmarshal converts JSON (produced by Marshal) back to an File.
func UnmarshalFile(data []byte) (*File, error) {
	var jf jsonFile
	if err := json.Unmarshal(data, &jf); err != nil {
		return nil, fmt.Errorf("astjson: %w", err)
	}
	return fromJSON(&jf)
}

func fromJSON(jf *jsonFile) (*File, error) {
	f := &File{}

	if jf.Attachments != nil {
		a, err := attachmentsBlockFromJSON(jf.Attachments)
		if err != nil {
			return nil, err
		}
		f.Attachments = a
	}
	if jf.Vars != nil {
		v, err := varsBlockFromJSON(jf.Vars)
		if err != nil {
			return nil, err
		}
		f.Vars = v
	}
	if jf.Secrets != nil {
		f.Secrets = secretsBlockFromJSON(jf.Secrets)
	}
	if jf.Presets != nil {
		p, err := presetsBlockFromJSON(jf.Presets)
		if err != nil {
			return nil, err
		}
		f.Presets = p
	}

	for _, js := range jf.MCPServers {
		s, err := mcpServerFromJSON(js)
		if err != nil {
			return nil, err
		}
		f.MCPServers = append(f.MCPServers, s)
	}

	for _, jp := range jf.Prompts {
		f.Prompts = append(f.Prompts, &PromptDecl{Name: jp.Name, Body: jp.Body})
	}

	for _, js := range jf.Schemas {
		s, err := schemaFromJSON(js)
		if err != nil {
			return nil, err
		}
		f.Schemas = append(f.Schemas, s)
	}

	for _, jc := range jf.Cursors {
		if c := cursorDeclFromJSON(jc); c != nil {
			f.Cursors = append(f.Cursors, c)
		}
	}

	for _, js := range jf.Supervisors {
		f.Supervisors = append(f.Supervisors, &SupervisorDecl{
			Name:     js.Name,
			Watches:  js.Watches,
			Model:    js.Model,
			System:   js.System,
			Cooldown: js.Cooldown,
			MaxEvals: js.MaxEvals,
			Monitors: js.Monitors,
		})
	}

	for _, ja := range jf.Agents {
		a, err := agentFromJSON(ja)
		if err != nil {
			return nil, err
		}
		f.Agents = append(f.Agents, a)
	}

	for _, jj := range jf.Judges {
		j, err := judgeFromJSON(jj)
		if err != nil {
			return nil, err
		}
		f.Judges = append(f.Judges, j)
	}

	for _, jr := range jf.Routers {
		mode, ok := strToRouterMode[jr.Mode]
		if !ok {
			return nil, fmt.Errorf("astjson: unknown router mode %q", jr.Mode)
		}
		f.Routers = append(f.Routers, &RouterDecl{
			Name:            jr.Name,
			Description:     jr.Description,
			Mode:            mode,
			Model:           jr.Model,
			Backend:         jr.Backend,
			Provider:        jr.Provider,
			System:          jr.System,
			User:            jr.User,
			Multi:           jr.Multi,
			ReasoningEffort: jr.ReasoningEffort,
			Over:            jr.Over,
			As:              jr.As,
			Key:             jr.Key,
			DependsOn:       jr.DependsOn,
			Needs:           jr.Needs,
		})
	}

	for _, jh := range jf.Humans {
		h, err := humanFromJSON(jh)
		if err != nil {
			return nil, err
		}
		f.Humans = append(f.Humans, h)
	}

	for _, jt := range jf.Tools {
		aw, ok := strToAwaitMode[jt.Await]
		if jt.Await != "" && !ok {
			return nil, fmt.Errorf("astjson: unknown await mode %q", jt.Await)
		}
		f.Tools = append(f.Tools, &ToolNodeDecl{
			Name:           jt.Name,
			Description:    jt.Description,
			Command:        jt.Command,
			Script:         jt.Script,
			Language:       jt.Language,
			Input:          jt.Input,
			Output:         jt.Output,
			Publish:        jt.Publish,
			ArtifactLabels: jt.ArtifactLabels,
			Await:          aw,
			Sandbox:        sandboxBlockFromJSON(jt.Sandbox),
			Compress:       jt.Compress,
			Permission:     jt.Permission,
			Goal:           jt.Goal,
			Postcondition:  jt.Postcondition,
			Policy:         jt.Policy,
			Recovery:       recoveryBlockFromJSON(jt.Recovery),
			Needs:          jt.Needs,
			ParallelSafe:   jt.ParallelSafe,
		})
	}

	for _, jc := range jf.Computes {
		aw, ok := strToAwaitMode[jc.Await]
		if jc.Await != "" && !ok {
			return nil, fmt.Errorf("astjson: unknown await mode %q", jc.Await)
		}
		cd := &ComputeDecl{
			Name:           jc.Name,
			Description:    jc.Description,
			Input:          jc.Input,
			Output:         jc.Output,
			Publish:        jc.Publish,
			ArtifactLabels: jc.ArtifactLabels,
			Await:          aw,
		}
		for _, je := range jc.Expr {
			cd.Expr = append(cd.Expr, &ComputeExpr{Key: je.Key, Expr: je.Expr})
		}
		f.Computes = append(f.Computes, cd)
	}

	for _, js := range jf.Subbots {
		sd := &SubbotDecl{
			Name:        js.Name,
			Description: js.Description,
			Source:      js.Source,
			Output:      js.Output,
			Needs:       js.Needs,
			Isolated:    js.Isolated,
		}
		for _, w := range js.With {
			sd.With = append(sd.With, &WithEntry{Key: w.Key, Value: w.Value})
		}
		f.Subbots = append(f.Subbots, sd)
	}

	for _, je := range jf.Emits {
		ed := &EmitDecl{Name: je.Name, Description: je.Description, Event: je.Event}
		for _, w := range je.With {
			ed.With = append(ed.With, &WithEntry{Key: w.Key, Value: w.Value})
		}
		f.Emits = append(f.Emits, ed)
	}

	for _, jw := range jf.Waits {
		f.Waits = append(f.Waits, &WaitDecl{
			Name:        jw.Name,
			Description: jw.Description,
			Event:       jw.Event,
			Timeout:     jw.Timeout,
			Output:      jw.Output,
		})
	}

	for _, ja := range jf.AwaitAnswers {
		f.AwaitAnswers = append(f.AwaitAnswers, &AwaitAnswersDecl{
			Name:        ja.Name,
			Description: ja.Description,
			From:        ja.From,
			Timeout:     ja.Timeout,
		})
	}

	for _, jfd := range jf.Fails {
		f.Fails = append(f.Fails, &FailDecl{
			Name:        jfd.Name,
			Description: jfd.Description,
			Code:        jfd.Code,
			Message:     jfd.Message,
			Resumable:   jfd.Resumable,
		})
	}

	for _, jw := range jf.Workflows {
		w, err := workflowFromJSON(jw)
		if err != nil {
			return nil, err
		}
		f.Workflows = append(f.Workflows, w)
	}

	for _, jc := range jf.Comments {
		f.Comments = append(f.Comments, &Comment{Text: jc.Text})
	}

	return f, nil
}

func mcpServerFromJSON(js *jsonMCPServerDecl) (*MCPServerDecl, error) {
	transport, ok := strToMCPTransport[js.Transport]
	if js.Transport != "" && !ok {
		return nil, fmt.Errorf("astjson: unknown mcp transport %q", js.Transport)
	}
	s := &MCPServerDecl{
		Name:      js.Name,
		Transport: transport,
		Command:   js.Command,
		Args:      js.Args,
		URL:       js.URL,
	}
	if js.Auth != nil {
		s.Auth = &MCPAuthDecl{
			Type:      js.Auth.Type,
			AuthURL:   js.Auth.AuthURL,
			TokenURL:  js.Auth.TokenURL,
			RevokeURL: js.Auth.RevokeURL,
			ClientID:  js.Auth.ClientID,
			Scopes:    js.Auth.Scopes,
		}
	}
	return s, nil
}

func mcpConfigFromJSON(jc *jsonMCPConfigDecl) *MCPConfigDecl {
	if jc == nil {
		return nil
	}
	return &MCPConfigDecl{
		AutoloadProject: jc.AutoloadProject,
		Inherit:         jc.Inherit,
		Servers:         jc.Servers,
		Disable:         jc.Disable,
	}
}

func compactionFromJSON(jc *jsonCompactionBlock) *CompactionBlock {
	if jc == nil {
		return nil
	}
	return &CompactionBlock{Threshold: jc.Threshold, PreserveRecent: jc.PreserveRecent}
}

func memoryFromJSON(jm *jsonMemoryBlock) *MemoryBlock {
	if jm == nil {
		return nil
	}
	return &MemoryBlock{
		Enabled:          jm.Enabled,
		Scope:            jm.Scope,
		Autoload:         jm.Autoload,
		Read:             jm.Read,
		Write:            jm.Write,
		PreCompactInject: jm.PreCompactInject,
		ProjectRoot:      jm.ProjectRoot,
		Visibility:       jm.Visibility,
	}
}

func secretsBlockFromJSON(js *jsonSecretsBlock) *SecretsBlock {
	s := &SecretsBlock{}
	for _, jf := range js.Fields {
		s.Fields = append(s.Fields, &SecretField{
			Name:        jf.Name,
			Value:       jf.Value,
			As:          jf.As,
			MountPath:   jf.MountPath,
			Env:         jf.Env,
			Optional:    jf.Optional,
			Hosts:       jf.Hosts,
			Description: jf.Description,
		})
	}
	return s
}

func varsBlockFromJSON(jv *jsonVarsBlock) (*VarsBlock, error) {
	v := &VarsBlock{}
	for _, jf := range jv.Fields {
		te, ok := strToTypeExpr[jf.Type]
		if !ok {
			return nil, fmt.Errorf("astjson: unknown type %q", jf.Type)
		}
		vf := &VarField{Name: jf.Name, Type: te, EnumValues: jf.Enum}
		if jf.Default != nil {
			l, err := literalFromJSON(jf.Default)
			if err != nil {
				return nil, err
			}
			vf.Default = l
		}
		v.Fields = append(v.Fields, vf)
	}
	return v, nil
}

func literalFromJSON(jl *jsonLiteral) (*Literal, error) {
	kind, ok := strToLiteralKind[jl.Kind]
	if !ok {
		return nil, fmt.Errorf("astjson: unknown literal kind %q", jl.Kind)
	}
	return &Literal{
		Kind:     kind,
		Raw:      jl.Raw,
		StrVal:   jl.StrVal,
		IntVal:   jl.IntVal,
		FloatVal: jl.FloatVal,
		BoolVal:  jl.BoolVal,
	}, nil
}

func schemaFromJSON(js *jsonSchemaDecl) (*SchemaDecl, error) {
	s := &SchemaDecl{Name: js.Name}
	for _, jf := range js.Fields {
		ft, ok := strToFieldType[jf.Type]
		if !ok {
			return nil, fmt.Errorf("astjson: unknown field type %q", jf.Type)
		}
		s.Fields = append(s.Fields, &SchemaField{
			Name:       jf.Name,
			Type:       ft,
			EnumValues: jf.EnumValues,
		})
	}
	return s, nil
}

func agentFromJSON(ja *jsonAgentDecl) (*AgentDecl, error) {
	sess, ok := strToSessionMode[ja.Session]
	if ja.Session != "" && !ok {
		return nil, fmt.Errorf("astjson: unknown session mode %q", ja.Session)
	}
	aw, ok := strToAwaitMode[ja.Await]
	if ja.Await != "" && !ok {
		return nil, fmt.Errorf("astjson: unknown await mode %q", ja.Await)
	}
	interaction, ok := strToInteractionMode[ja.Interaction]
	if ja.Interaction != "" && !ok {
		return nil, fmt.Errorf("astjson: unknown interaction mode %q", ja.Interaction)
	}
	return &AgentDecl{
		Name: ja.Name,
		LLMDecl: LLMDecl{
			Description:       ja.Description,
			Model:             ja.Model,
			Backend:           ja.Backend,
			Provider:          ja.Provider,
			Command:           ja.Command,
			MCP:               mcpConfigFromJSON(ja.MCP),
			Input:             ja.Input,
			Output:            ja.Output,
			Publish:           ja.Publish,
			ArtifactLabels:    ja.ArtifactLabels,
			System:            ja.System,
			User:              ja.User,
			Session:           sess,
			Tools:             ja.Tools,
			ToolPolicy:        ja.ToolPolicy,
			Capabilities:      ja.Capabilities,
			Skills:            ja.Skills,
			ToolMaxSteps:      ja.ToolMaxSteps,
			MaxTokens:         ja.MaxTokens,
			ReasoningEffort:   ja.ReasoningEffort,
			Timeout:           ja.Timeout,
			Readonly:          ja.Readonly,
			FullAccess:        ja.FullAccess,
			Images:            ja.Images,
			Interaction:       interaction,
			InteractionPrompt: ja.InteractionPrompt,
			InteractionModel:  ja.InteractionModel,
			Await:             aw,
			Compaction:        compactionFromJSON(ja.Compaction),
			Memory:            memoryFromJSON(ja.Memory),
			Sandbox:           sandboxBlockFromJSON(ja.Sandbox),
			Cursors:           cursorBlockFromJSON(ja.Cursors),
			Fallbacks:         fallbacksFromJSON(ja.Fallbacks),
			Compress:          ja.Compress,
			AutoMemory:        ja.AutoMemory,
			Permission:        ja.Permission,
			Needs:             ja.Needs,
		},
	}, nil
}

func judgeFromJSON(jj *jsonJudgeDecl) (*JudgeDecl, error) {
	sess, ok := strToSessionMode[jj.Session]
	if jj.Session != "" && !ok {
		return nil, fmt.Errorf("astjson: unknown session mode %q", jj.Session)
	}
	aw, ok := strToAwaitMode[jj.Await]
	if jj.Await != "" && !ok {
		return nil, fmt.Errorf("astjson: unknown await mode %q", jj.Await)
	}
	interaction, ok := strToInteractionMode[jj.Interaction]
	if jj.Interaction != "" && !ok {
		return nil, fmt.Errorf("astjson: unknown interaction mode %q", jj.Interaction)
	}
	return &JudgeDecl{
		Name: jj.Name,
		LLMDecl: LLMDecl{
			Description:       jj.Description,
			Model:             jj.Model,
			Backend:           jj.Backend,
			Provider:          jj.Provider,
			Command:           jj.Command,
			MCP:               mcpConfigFromJSON(jj.MCP),
			Input:             jj.Input,
			Output:            jj.Output,
			Publish:           jj.Publish,
			ArtifactLabels:    jj.ArtifactLabels,
			System:            jj.System,
			User:              jj.User,
			Session:           sess,
			Tools:             jj.Tools,
			ToolPolicy:        jj.ToolPolicy,
			Capabilities:      jj.Capabilities,
			Skills:            jj.Skills,
			ToolMaxSteps:      jj.ToolMaxSteps,
			MaxTokens:         jj.MaxTokens,
			ReasoningEffort:   jj.ReasoningEffort,
			Timeout:           jj.Timeout,
			Readonly:          jj.Readonly,
			FullAccess:        jj.FullAccess,
			Images:            jj.Images,
			Interaction:       interaction,
			InteractionPrompt: jj.InteractionPrompt,
			InteractionModel:  jj.InteractionModel,
			Await:             aw,
			Compaction:        compactionFromJSON(jj.Compaction),
			Memory:            memoryFromJSON(jj.Memory),
			Sandbox:           sandboxBlockFromJSON(jj.Sandbox),
			Cursors:           cursorBlockFromJSON(jj.Cursors),
			Fallbacks:         fallbacksFromJSON(jj.Fallbacks),
			Compress:          jj.Compress,
			AutoMemory:        jj.AutoMemory,
			Permission:        jj.Permission,
			Needs:             jj.Needs,
		},
	}, nil
}

func humanFromJSON(jh *jsonHumanDecl) (*HumanDecl, error) {
	interactionStr := jh.Interaction
	interaction, ok := strToInteractionMode[interactionStr]
	if interactionStr != "" && !ok {
		return nil, fmt.Errorf("astjson: unknown interaction mode %q", interactionStr)
	}
	if interactionStr == "" {
		interaction = InteractionHuman // default for human nodes
	}
	return humanFromJSONWithInteraction(jh, interaction)
}

func humanFromJSONWithInteraction(jh *jsonHumanDecl, interaction InteractionMode) (*HumanDecl, error) {
	aw, ok := strToAwaitMode[jh.Await]
	if jh.Await != "" && !ok {
		return nil, fmt.Errorf("astjson: unknown await mode %q", jh.Await)
	}
	return &HumanDecl{
		Name:              jh.Name,
		Description:       jh.Description,
		Input:             jh.Input,
		Output:            jh.Output,
		Publish:           jh.Publish,
		ArtifactLabels:    jh.ArtifactLabels,
		Instructions:      jh.Instructions,
		Interaction:       interaction,
		InteractionPrompt: jh.InteractionPrompt,
		InteractionModel:  jh.InteractionModel,
		MinAnswers:        jh.MinAnswers,
		Model:             jh.Model,
		System:            jh.System,
		Await:             aw,
		ReviewURL:         jh.ReviewURL,
		Posture:           jh.Posture,
		MergeStrategy:     jh.MergeStrategy,
		MergeInto:         jh.MergeInto,
		MaxTurns:          jh.MaxTurns,
	}, nil
}

func workflowFromJSON(jw *jsonWorkflowDecl) (*WorkflowDecl, error) {
	w := &WorkflowDecl{
		Name:            jw.Name,
		Entry:           jw.Entry,
		DefaultBackend:  jw.DefaultBackend,
		ToolPolicy:      jw.ToolPolicy,
		Capabilities:    jw.Capabilities,
		Skills:          jw.Skills,
		MCP:             mcpConfigFromJSON(jw.MCP),
		Compaction:      compactionFromJSON(jw.Compaction),
		Worktree:        jw.Worktree,
		Compress:        jw.Compress,
		AutoMemory:      jw.AutoMemory,
		LoopBudgetGuard: jw.LoopBudgetGuard,
		RepoDevbox:      jw.RepoDevbox,
		Permission:      jw.Permission,
		Allow:           jw.Allow,
		Ask:             jw.Ask,
		Deny:            jw.Deny,
		Sandbox:         sandboxBlockFromJSON(jw.Sandbox),
	}
	if jw.Vars != nil {
		v, err := varsBlockFromJSON(jw.Vars)
		if err != nil {
			return nil, err
		}
		w.Vars = v
	}
	if jw.Attachments != nil {
		a, err := attachmentsBlockFromJSON(jw.Attachments)
		if err != nil {
			return nil, err
		}
		w.Attachments = a
	}
	if jw.Interaction != "" {
		interaction, ok := strToInteractionMode[jw.Interaction]
		if !ok {
			return nil, fmt.Errorf("astjson: unknown interaction mode %q", jw.Interaction)
		}
		w.Interaction = &interaction
	}
	if jw.Budget != nil {
		w.Budget = &BudgetBlock{
			MaxParallelBranches: jw.Budget.MaxParallelBranches,
			MaxDuration:         jw.Budget.MaxDuration,
			MaxCostUSD:          jw.Budget.MaxCostUSD,
			MaxTokens:           jw.Budget.MaxTokens,
			WarnTokens:          jw.Budget.WarnTokens,
			MaxIterations:       jw.Budget.MaxIterations,
		}
	}
	if len(jw.Resources) > 0 {
		w.Resources = &ResourcesBlock{Capacities: jw.Resources}
	}
	for _, je := range jw.Edges {
		e, err := edgeFromJSON(je)
		if err != nil {
			return nil, err
		}
		w.Edges = append(w.Edges, e)
	}
	return w, nil
}

func edgeFromJSON(je *jsonEdge) (*Edge, error) {
	e := &Edge{
		From:   je.From,
		To:     je.To,
		IsElse: je.IsElse,
	}
	if je.When != nil {
		// Reject ambiguous shapes where both Condition and Expr are
		// set: ir.Compile silently prefers Expr and drops Condition,
		// so a reviewer auditing the JSON sees one predicate while
		// the runtime executes another (F-DSL-9).
		if je.When.Expr != "" && je.When.Condition != "" {
			return nil, fmt.Errorf("edge %s→%s: when clause has both .expr and .condition — use exactly one", je.From, je.To)
		}
		e.When = &WhenClause{
			Condition: je.When.Condition,
			Negated:   je.When.Negated,
			Expr:      je.When.Expr,
		}
	}
	if je.Loop != nil {
		e.Loop = &LoopClause{
			Name:              je.Loop.Name,
			MaxIterations:     je.Loop.MaxIterations,
			MaxIterationsExpr: je.Loop.MaxIterationsExpr,
			Unbounded:         je.Loop.Unbounded,
			FuelCap:           je.Loop.FuelCap,
		}
	}
	for _, jw := range je.With {
		e.With = append(e.With, &WithEntry{
			Key:   jw.Key,
			Value: jw.Value,
		})
	}
	return e, nil
}
