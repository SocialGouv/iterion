package author

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/spec"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
	"github.com/SocialGouv/iterion/pkg/dsl/workflowfile"
)

// Write renders f as an author document: `dsl:` first, the catalog the
// file's frontmatter carries, its imports, then every declaration under
// its key — mappings keyed by name for the declarations the .bot names,
// sequences for the ones whose order is meaning (groups, uses, nodes,
// fallback routes, edges) — each body's keys in the registry's order, and
// every value in YAML's native form. Comments other than the frontmatter
// are not carried: the document is a draft, the .bot the truth.
//
// An AST the document cannot say — two workflows, a with map or a params
// block naming a key twice, a prompt whose name YAML cannot write plain —
// is refused by name rather than written otherwise.
func Write(f *ast.File) ([]byte, error) {
	w := &writer{inline: inlineBodies(f.Prompts)}
	root := w.file(f)
	if w.err != nil {
		return nil, w.err
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return nil, fmt.Errorf("author: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("author: %w", err)
	}
	return buf.Bytes(), nil
}

type writer struct {
	inline map[string]string
	err    error
}

func (w *writer) fail(err error) {
	if w.err == nil {
		w.err = err
	}
}

// inlineBodies indexes the Inline prompts of a file by name: a property
// referring to one is written as its text.
func inlineBodies(prompts []*ast.PromptDecl) map[string]string {
	out := map[string]string{}
	for _, p := range prompts {
		if p.Inline {
			out[p.Name] = p.Body
		}
	}
	return out
}

// ---- node builders ----

// keyNode is a mapping key, tagged `!!str`: the tag makes the encoder quote
// a key whose plain spelling would read as another type (`123`, `true`,
// `null`, `2026-01-01` — author-chosen names reach here through a JSON
// object's keys), so the reader gets the string back, never a non-string
// key to refuse. A scanner break needs the double quotes said explicitly
// (see str).
func keyNode(k string) *yaml.Node {
	if strings.ContainsAny(k, scannerBreaks) {
		return quoted(k)
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}
}

// scannerBreaks are the characters yaml.v3's scanner reads as a line break
// besides the newline: CR, NEL, LS, PS.
var scannerBreaks = string([]rune{13, 0x85, 0x2028, 0x2029})

// str is a string value: plain when YAML reads the plain form back as the
// same string (the encoder quotes the others), a literal block when it
// holds a newline — and double-quoted when it holds a scanner break, the
// one spelling the document reads back as the character (escaped). The
// emitter escapes a CR or a NEL on its own, neither being printable to
// it; an LS or a PS is printable to it and would be written raw, where the
// scanner ends a line and the reader settles a newline.
func str(v string) *yaml.Node {
	if strings.ContainsAny(v, scannerBreaks) {
		return quoted(v)
	}
	n := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
	if strings.Contains(v, "\n") {
		n.Style = yaml.LiteralStyle
	}
	return n
}

// quoted is a string value written in double quotes whatever it holds.
func quoted(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v, Style: yaml.DoubleQuotedStyle}
}

func intNode(i int64) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.FormatInt(i, 10)}
}

// floatNode is a float value whose text YAML reads back as a float: a
// fraction is always present.
func floatNode(text string) *yaml.Node {
	if !strings.ContainsAny(text, ".eE") {
		text += ".0"
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: text}
}

func boolNode(b bool) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(b)}
}

func nullNode() *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}
}

func flowSeq(items []*yaml.Node) *yaml.Node {
	return &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle, Content: items}
}

func blockSeq(items []*yaml.Node) *yaml.Node {
	return &yaml.Node{Kind: yaml.SequenceNode, Content: items}
}

// strList is a flow list of strings, nil when the list is empty.
func strList(vals []string) *yaml.Node {
	if len(vals) == 0 {
		return nil
	}
	items := make([]*yaml.Node, 0, len(vals))
	for _, v := range vals {
		items = append(items, str(v))
	}
	return flowSeq(items)
}

// declaredList is strList for the one list whose EMPTY form is a declared
// value — an agent's or judge's `tools:` (toolcatalog.ToolsDeclared): nil is
// absent and writes nothing, an empty slice is the author saying "this node
// has no tools" and writes `[]`, as the .bot writes it back. Dropping the
// line would turn a closed node into an open one.
func declaredList(vals []string) *yaml.Node {
	if vals == nil {
		return nil
	}
	items := make([]*yaml.Node, 0, len(vals))
	for _, v := range vals {
		items = append(items, str(v))
	}
	return flowSeq(items)
}

// identOrList is a `needs:` value: the one name bare, several as a list.
func identOrList(vals []string) *yaml.Node {
	if len(vals) == 1 {
		return str(vals[0])
	}
	return strList(vals)
}

// mapNode builds a mapping in insertion order; a nil value adds nothing.
// A mapping keyed by author-chosen names (w.named) refuses a name used
// twice: YAML names a key once, and the document the writer produced
// would be refused whole by the reader — the one chokepoint every such
// mapping goes through, so no site can write the duplicate in silence.
type mapNode struct {
	n    *yaml.Node
	w    *writer
	what string
	seen map[string]bool
}

func newMap() *mapNode { return &mapNode{n: &yaml.Node{Kind: yaml.MappingNode}} }

// named is a mapping keyed by the author's names — a var, a port, a
// prompt, a parameter — that refuses a name written twice.
func (w *writer) named(what string) *mapNode {
	return &mapNode{n: &yaml.Node{Kind: yaml.MappingNode}, w: w, what: what, seen: map[string]bool{}}
}

func (m *mapNode) set(key string, v *yaml.Node) *mapNode {
	if v == nil {
		return m
	}
	if m.w != nil {
		if m.seen[key] {
			m.w.fail(fmt.Errorf("author: %s names %q twice; a YAML mapping names a key once", m.what, key))
			return m
		}
		m.seen[key] = true
	}
	m.n.Content = append(m.n.Content, keyNode(key), v)
	return m
}

func (m *mapNode) flow() *mapNode {
	m.n.Style = yaml.FlowStyle
	return m
}

func (m *mapNode) node() *yaml.Node { return m.n }

// props collects a kind's property values by name and writes them in the
// registry's order — the document's canonical order, the one the
// reference documents.
type props struct {
	kind  spec.Kind
	vals  map[string]*yaml.Node
	lead  *mapNode
	trail *mapNode
}

func (w *writer) props(kind string) *props {
	k, ok := spec.Lookup(kind)
	if !ok {
		w.fail(fmt.Errorf("author: the registry names no kind %q", kind))
	}
	return &props{kind: k, vals: map[string]*yaml.Node{}, lead: newMap(), trail: w.named("the " + kind + " block's entries")}
}

// set records a property's value; a name the kind does not have is a
// programming error the tests catch.
func (p *props) set(name string, v *yaml.Node) {
	if v == nil {
		return
	}
	if !p.kind.Has(name) {
		panic("author: " + p.kind.Name + " has no property " + name)
	}
	p.vals[name] = v
}

func (p *props) str(name, v string) {
	if v == "" {
		return
	}
	// An enum|env property takes its words bare or quoted and anything else
	// quoted (a run-time substitution, kept as written): the document says
	// the same with a quoted scalar, which the reader takes as any string.
	for _, pr := range p.kind.Properties {
		if pr.Name == name && pr.Form == spec.EnumOrEnv && !isOneOf(v, pr.Values) {
			p.set(name, quoted(v))
			return
		}
	}
	p.set(name, str(v))
}

func isOneOf(v string, words []string) bool {
	for _, w := range words {
		if v == w {
			return true
		}
	}
	return false
}

func (p *props) int(name string, v int) {
	if v != 0 {
		p.set(name, intNode(int64(v)))
	}
}

func (p *props) bool(name string, v bool) {
	if v {
		p.set(name, boolNode(true))
	}
}

func (p *props) node() *yaml.Node {
	m := newMap()
	m.n.Content = append(m.n.Content, p.lead.n.Content...)
	for _, prop := range p.kind.Properties {
		if v := p.vals[prop.Name]; v != nil {
			m.set(prop.Name, v)
		}
	}
	m.n.Content = append(m.n.Content, p.trail.n.Content...)
	return m.n
}

// ---- the file ----

func (w *writer) file(f *ast.File) *yaml.Node {
	root := newMap()
	root.set("dsl", intNode(int64(f.EffectiveProfile())))
	if cat := frontmatterOf(f.Comments); cat != nil {
		root.set("catalog", cat)
	}
	if len(f.Imports) > 0 {
		var paths []string
		for _, im := range f.Imports {
			paths = append(paths, im.Path)
		}
		root.set("imports", strList(paths))
	}
	if f.Vars != nil {
		root.set("vars", w.vars(f.Vars))
	}
	if f.Presets != nil {
		root.set("presets", w.presets(f.Presets))
	}
	if f.Attachments != nil {
		root.set("attachments", w.attachments(f.Attachments))
	}
	if f.Secrets != nil {
		root.set("secrets", w.secrets(f.Secrets))
	}
	if len(f.Schemas) > 0 {
		m := w.named("schemas")
		for _, sd := range f.Schemas {
			m.set(sd.Name, w.schema(sd))
		}
		root.set("schemas", m.node())
	}
	if m := w.prompts(f.Prompts); m != nil {
		root.set("prompts", m)
	}
	if len(f.Cursors) > 0 {
		m := w.named("cursors")
		for _, cd := range f.Cursors {
			m.set(cd.Name, w.cursorDecl(cd))
		}
		root.set("cursors", m.node())
	}
	if len(f.Supervisors) > 0 {
		m := w.named("supervisors")
		for _, sd := range f.Supervisors {
			m.set(sd.Name, w.supervisor(sd))
		}
		root.set("supervisors", m.node())
	}
	if len(f.MCPServers) > 0 {
		m := w.named("mcp_servers")
		for _, md := range f.MCPServers {
			m.set(md.Name, w.mcpServer(md))
		}
		root.set("mcp_servers", m.node())
	}
	if len(f.Contracts) > 0 {
		m := w.named("contracts")
		for _, cd := range f.Contracts {
			m.set(cd.Name, w.contract(cd))
		}
		root.set("contracts", m.node())
	}
	if len(f.Groups) > 0 {
		var items []*yaml.Node
		for _, gd := range f.Groups {
			items = append(items, w.group(gd))
		}
		root.set("groups", blockSeq(items))
	}
	if len(f.Uses) > 0 {
		var items []*yaml.Node
		for _, ud := range f.Uses {
			items = append(items, w.use(ud))
		}
		root.set("uses", blockSeq(items))
	}
	if nodes := w.nodes(fileNodes(f)); nodes != nil {
		root.set("nodes", nodes)
	}
	switch len(f.Workflows) {
	case 0:
	case 1:
		root.set("workflow", w.workflow(f.Workflows[0]))
	default:
		w.fail(fmt.Errorf("author: the document holds one workflow, the file declares %d (%s and %s)", len(f.Workflows), f.Workflows[0].Name, f.Workflows[1].Name))
	}
	return root.node()
}

// frontmatterOf reads the catalog identity off the file's head comments —
// the `---` fence through the closing one — as the bundle's frontmatter
// reader reads it (bundle.ParseFrontmatter: the same YAML, the same four
// keys), and returns it as the `catalog:` mapping; nil when the file
// carries none.
func frontmatterOf(comments []*ast.Comment) *yaml.Node {
	if len(comments) == 0 || strings.TrimSpace(comments[0].Text) != workflowfile.FrontmatterFence {
		return nil
	}
	var inner []string
	closed := false
	for _, c := range comments[1:] {
		if strings.TrimSpace(c.Text) == workflowfile.FrontmatterFence {
			closed = true
			break
		}
		inner = append(inner, c.Text)
	}
	if !closed {
		return nil
	}
	var fm struct {
		Name         string   `yaml:"name"`
		Description  string   `yaml:"description"`
		Triggers     []string `yaml:"triggers"`
		Capabilities []string `yaml:"capabilities"`
	}
	if err := yaml.Unmarshal([]byte(strings.Join(inner, "\n")), &fm); err != nil {
		return nil
	}
	m := newMap()
	if fm.Name != "" {
		m.set("name", str(fm.Name))
	}
	if fm.Description != "" {
		m.set("description", str(fm.Description))
	}
	m.set("triggers", strList(fm.Triggers))
	m.set("capabilities", strList(fm.Capabilities))
	if len(m.n.Content) == 0 {
		return nil
	}
	return m.node()
}

// ---- top-level blocks ----

func (w *writer) vars(vb *ast.VarsBlock) *yaml.Node {
	m := w.named("vars")
	for _, v := range vb.Fields {
		if len(v.EnumValues) == 0 && v.Matching == "" && v.Default == nil {
			m.set(v.Name, str(v.Type.String()))
			continue
		}
		e := newMap().set("type", str(v.Type.String()))
		e.set("enum", strList(v.EnumValues))
		if v.Matching != "" {
			e.set("matching", str(v.Matching))
		}
		if v.Default != nil {
			e.set("default", literalNode(v.Default))
		}
		m.set(v.Name, e.node())
	}
	return m.node()
}

func literalNode(l *ast.Literal) *yaml.Node {
	switch l.Kind {
	case ast.LitInt:
		return intNode(l.IntVal)
	case ast.LitFloat:
		if numberRe.MatchString(l.Raw) {
			return floatNode(l.Raw)
		}
		return floatNode(strconv.FormatFloat(l.FloatVal, 'f', -1, 64))
	case ast.LitBool:
		return boolNode(l.BoolVal)
	}
	return str(l.StrVal)
}

func (w *writer) presets(pb *ast.PresetsBlock) *yaml.Node {
	m := w.named("presets")
	for _, p := range pb.Entries {
		vals := w.named("the preset " + p.Name)
		for _, pv := range p.Values {
			if pv.Value != nil {
				vals.set(pv.Key, literalNode(pv.Value))
			}
		}
		m.set(p.Name, vals.node())
	}
	return m.node()
}

func (w *writer) attachments(ab *ast.AttachmentsBlock) *yaml.Node {
	m := w.named("attachments")
	for _, a := range ab.Fields {
		if a.Required == nil && len(a.AcceptMIME) == 0 && a.Description == "" {
			m.set(a.Name, str(a.Type.String()))
			continue
		}
		p := w.props("attachment")
		p.lead.set("type", str(a.Type.String()))
		p.str("description", a.Description)
		p.set("accept_mime", strList(a.AcceptMIME))
		if a.Required != nil {
			p.set("required", boolNode(*a.Required))
		}
		m.set(a.Name, p.node())
	}
	return m.node()
}

func (w *writer) secrets(sb *ast.SecretsBlock) *yaml.Node {
	m := w.named("secrets")
	for _, s := range sb.Fields {
		bare := s.As == "" && s.MountPath == "" && s.Env == "" && !s.Optional && len(s.Hosts) == 0 && s.Description == ""
		switch {
		case bare && s.Value == "":
			m.set(s.Name, nullNode())
		case bare:
			m.set(s.Name, str(s.Value))
		default:
			p := w.props("secret")
			p.str("value", s.Value)
			p.str("as", s.As)
			p.str("mount_path", s.MountPath)
			p.str("env", s.Env)
			p.bool("optional", s.Optional)
			p.set("hosts", strList(s.Hosts))
			p.str("description", s.Description)
			m.set(s.Name, p.node())
		}
	}
	return m.node()
}

func (w *writer) schema(sd *ast.SchemaDecl) *yaml.Node {
	m := w.named("the schema " + sd.Name)
	for _, f := range sd.Fields {
		if len(f.EnumValues) == 0 {
			m.set(f.Name, str(f.Type.String()))
			continue
		}
		m.set(f.Name, newMap().set("type", str(f.Type.String())).set("enum", strList(f.EnumValues)).node())
	}
	return m.node()
}

// prompts writes the declared prompts; an Inline one is written on the
// property that refers to it.
func (w *writer) prompts(prompts []*ast.PromptDecl) *yaml.Node {
	m := w.named("prompts")
	for _, p := range prompts {
		if p.Inline {
			continue
		}
		// A declared body is written under its header, its indentation
		// read off its first line: a body with no such form (the save
		// guard refuses it as well) is refused here, not written otherwise.
		if err := parser.CheckPromptBody(p.Body); err != nil {
			w.fail(fmt.Errorf("author: the prompt %q has no written form: %v", p.Name, err))
			continue
		}
		m.set(p.Name, str(p.Body))
	}
	if len(m.n.Content) == 0 {
		return nil
	}
	return m.node()
}

// promptRef writes a prompt-referencing property: the text of an Inline
// prompt — quoted when YAML would read the text as a bare name — or the
// declared prompt's name, plain.
func (w *writer) promptRef(name string) *yaml.Node {
	if name == "" {
		return nil
	}
	if body, ok := w.inline[name]; ok {
		if isIdent(body) || !strings.Contains(body, "\n") && needsQuote(body) {
			return quoted(body)
		}
		return str(body)
	}
	if needsQuote(name) {
		w.fail(fmt.Errorf("author: the prompt name %q has no plain YAML spelling (it reads as another type); rename the prompt", name))
	}
	return str(name)
}

// needsQuote reports whether YAML would not write s as a plain scalar
// reading back as a string — the encoder quotes it, and a quoted name is
// read as an inline prompt's text.
func needsQuote(s string) bool {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	if err := enc.Encode(str(s)); err != nil {
		return true
	}
	_ = enc.Close()
	out := strings.TrimSpace(buf.String())
	return out != s
}

func (w *writer) cursorDecl(cd *ast.CursorDecl) *yaml.Node {
	p := w.props("cursor")
	p.str("description", cd.Description)
	if len(cd.Values) > 0 {
		m := w.named("the cursor " + cd.Name + "'s values")
		for _, v := range cd.Values {
			m.set(v.Name, str(v.Prompt))
		}
		p.set("values", m.node())
	}
	if len(cd.Bands) > 0 {
		m := w.named("the cursor " + cd.Name + "'s bands")
		for _, b := range cd.Bands {
			m.set(b.Range, str(b.Prompt))
		}
		p.set("bands", m.node())
	}
	return p.node()
}

func (w *writer) supervisor(sd *ast.SupervisorDecl) *yaml.Node {
	p := w.props("supervisor")
	p.set("watches", strList(sd.Watches))
	p.str("model", sd.Model)
	p.set("system", w.promptRef(sd.System))
	p.str("cooldown", sd.Cooldown)
	p.int("max_evals", sd.MaxEvals)
	p.set("monitors", strList(sd.Monitors))
	return p.node()
}

func (w *writer) mcpServer(md *ast.MCPServerDecl) *yaml.Node {
	p := w.props("mcp_server")
	if md.Transport != ast.MCPTransportUnknown {
		p.set("transport", str(md.Transport.String()))
	}
	p.str("command", md.Command)
	p.set("args", strList(md.Args))
	p.str("url", md.URL)
	if md.Auth != nil {
		a := w.props("auth")
		a.str("type", md.Auth.Type)
		a.str("auth_url", md.Auth.AuthURL)
		a.str("token_url", md.Auth.TokenURL)
		a.str("revoke_url", md.Auth.RevokeURL)
		a.str("client_id", md.Auth.ClientID)
		a.set("scopes", strList(md.Auth.Scopes))
		p.set("auth", a.node())
	}
	return p.node()
}

// ---- contracts ----

func (w *writer) contract(cd *ast.ContractDecl) *yaml.Node {
	p := w.props("contract")
	p.str("display_name", cd.DisplayName)
	p.str("responsibility", cd.Responsibility)
	if cd.Version != nil {
		p.set("version", intNode(int64(*cd.Version)))
	}
	if len(cd.Inputs) > 0 {
		p.set("inputs", w.ports(cd.Inputs))
	}
	if len(cd.Outputs) > 0 {
		p.set("outputs", w.ports(cd.Outputs))
	}
	if len(cd.Criteria) > 0 {
		m := w.named("the contract " + cd.Name + "'s criteria")
		for _, c := range cd.Criteria {
			if c.Kind == "" && c.Port == "" && len(c.Params) == 0 {
				m.set(c.Name, nullNode())
				continue
			}
			cp := w.props("contract.criterion")
			cp.str("kind", c.Kind)
			cp.str("port", c.Port)
			if len(c.Params) > 0 {
				cp.set("params", w.jsonNode(c.Params))
			}
			m.set(c.Name, cp.node())
		}
		p.set("criteria", m.node())
	}
	if len(cd.Effects) > 0 {
		m := w.named("the contract " + cd.Name + "'s effects")
		for _, e := range cd.Effects {
			if e.Description == "" && !e.Paid {
				m.set(e.Name, nullNode())
				continue
			}
			ep := w.props("contract.effect")
			ep.str("description", e.Description)
			ep.bool("paid", e.Paid)
			m.set(e.Name, ep.node())
		}
		p.set("effects", m.node())
	}
	return p.node()
}

func (w *writer) ports(ports []*ast.PortDecl) *yaml.Node {
	m := w.named("a contract's ports")
	for _, port := range ports {
		if port.Description == "" && port.Required == nil && !port.Nullable && len(port.Default) == 0 && port.MinItems == nil && port.MaxItems == nil && port.From == "" && port.FileSpec == nil {
			m.set(port.Name, str(port.Type))
			continue
		}
		p := w.props("contract.port")
		p.lead.set("type", str(port.Type))
		p.str("description", port.Description)
		if port.Required != nil {
			p.set("required", boolNode(*port.Required))
		}
		p.bool("nullable", port.Nullable)
		if len(port.Default) > 0 {
			p.set("default", w.jsonNode(port.Default))
		}
		if port.MinItems != nil {
			p.set("min_items", intNode(int64(*port.MinItems)))
		}
		if port.MaxItems != nil {
			p.set("max_items", intNode(int64(*port.MaxItems)))
		}
		p.str("from", port.From)
		if port.FileSpec != nil {
			fp := w.props("contract.file")
			fp.str("media_type", port.FileSpec.MediaType)
			if port.FileSpec.MinBytes != 0 {
				fp.set("min_bytes", intNode(port.FileSpec.MinBytes))
			}
			fp.str("schema", port.FileSpec.Schema)
			p.set("file", fp.node())
		}
		m.set(port.Name, p.node())
	}
	return m.node()
}

// jsonNode writes a JSON value (a port's default, a criterion's params) as
// the YAML value it is.
func (w *writer) jsonNode(raw json.RawMessage) *yaml.Node {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		w.fail(fmt.Errorf("author: a JSON value of the program does not decode: %w", err))
		return nullNode()
	}
	return w.jsonValueNode(v)
}

func (w *writer) jsonValueNode(v any) *yaml.Node {
	switch t := v.(type) {
	case nil:
		return nullNode()
	case string:
		return str(t)
	case bool:
		return boolNode(t)
	case json.Number:
		s := t.String()
		if strings.ContainsAny(s, ".eE") {
			return floatNode(s)
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: s}
	case []any:
		items := make([]*yaml.Node, 0, len(t))
		for _, e := range t {
			items = append(items, w.jsonValueNode(e))
		}
		return flowSeq(items)
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		m := w.named("a JSON object").flow()
		for _, k := range keys {
			m.set(k, w.jsonValueNode(t[k]))
		}
		return m.node()
	}
	return str(fmt.Sprint(v))
}

// ---- nodes ----

// nodeAt is a node declaration with its place in the text, for the order
// the document writes them in.
type nodeAt struct {
	kind string
	pos  ast.Pos
	decl any
}

// fileNodes lists the file's nodes in the order the .bot declared them
// (by position) when every node has one, else kind by kind.
func fileNodes(f *ast.File) []nodeAt {
	var out []nodeAt
	for _, d := range f.Agents {
		out = append(out, nodeAt{"agent", d.Span.Start, d})
	}
	for _, d := range f.Judges {
		out = append(out, nodeAt{"judge", d.Span.Start, d})
	}
	for _, d := range f.Routers {
		out = append(out, nodeAt{"router", d.Span.Start, d})
	}
	for _, d := range f.Humans {
		out = append(out, nodeAt{"human", d.Span.Start, d})
	}
	for _, d := range f.Tools {
		out = append(out, nodeAt{"tool", d.Span.Start, d})
	}
	for _, d := range f.Computes {
		out = append(out, nodeAt{"compute", d.Span.Start, d})
	}
	for _, d := range f.Emits {
		out = append(out, nodeAt{"emit", d.Span.Start, d})
	}
	for _, d := range f.Waits {
		out = append(out, nodeAt{"wait", d.Span.Start, d})
	}
	for _, d := range f.AwaitAnswers {
		out = append(out, nodeAt{"await_answers", d.Span.Start, d})
	}
	for _, d := range f.Fails {
		out = append(out, nodeAt{"fail", d.Span.Start, d})
	}
	for _, d := range f.Subbots {
		out = append(out, nodeAt{"subbot", d.Span.Start, d})
	}
	return sortedByPosition(out)
}

func groupNodes(g *ast.GroupDecl) []nodeAt {
	var out []nodeAt
	for _, d := range g.Agents {
		out = append(out, nodeAt{"agent", d.Span.Start, d})
	}
	for _, d := range g.Judges {
		out = append(out, nodeAt{"judge", d.Span.Start, d})
	}
	for _, d := range g.Routers {
		out = append(out, nodeAt{"router", d.Span.Start, d})
	}
	for _, d := range g.Humans {
		out = append(out, nodeAt{"human", d.Span.Start, d})
	}
	for _, d := range g.Tools {
		out = append(out, nodeAt{"tool", d.Span.Start, d})
	}
	for _, d := range g.Computes {
		out = append(out, nodeAt{"compute", d.Span.Start, d})
	}
	return sortedByPosition(out)
}

// sortedByPosition orders the nodes as the text declared them when every
// one carries a position; a document built in memory keeps kind order.
func sortedByPosition(nodes []nodeAt) []nodeAt {
	for _, n := range nodes {
		if n.pos.Line == 0 {
			return nodes
		}
	}
	sort.SliceStable(nodes, func(i, j int) bool {
		a, b := nodes[i].pos, nodes[j].pos
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Column < b.Column
	})
	return nodes
}

func (w *writer) nodes(nodes []nodeAt) *yaml.Node {
	if len(nodes) == 0 {
		return nil
	}
	items := make([]*yaml.Node, 0, len(nodes))
	for _, n := range nodes {
		items = append(items, w.node(n))
	}
	return blockSeq(items)
}

func (w *writer) node(n nodeAt) *yaml.Node {
	switch d := n.decl.(type) {
	case *ast.AgentDecl:
		return w.llm("agent", d.Name, &d.LLMDecl)
	case *ast.JudgeDecl:
		return w.llm("judge", d.Name, &d.LLMDecl)
	case *ast.RouterDecl:
		return w.router(d)
	case *ast.HumanDecl:
		return w.human(d)
	case *ast.ToolNodeDecl:
		return w.tool(d)
	case *ast.ComputeDecl:
		return w.compute(d)
	case *ast.EmitDecl:
		p := w.props("emit")
		p.lead.set("emit", str(d.Name))
		p.str("description", d.Description)
		p.str("event", d.Event)
		p.set("with", w.withMap(d.With))
		return p.node()
	case *ast.WaitDecl:
		p := w.props("wait")
		p.lead.set("wait", str(d.Name))
		p.str("description", d.Description)
		p.str("event", d.Event)
		p.str("timeout", d.Timeout)
		p.str("output", d.Output)
		return p.node()
	case *ast.AwaitAnswersDecl:
		p := w.props("await_answers")
		p.lead.set("await_answers", str(d.Name))
		p.str("description", d.Description)
		p.str("from", d.From)
		p.str("timeout", d.Timeout)
		return p.node()
	case *ast.FailDecl:
		p := w.props("fail")
		p.lead.set("fail", str(d.Name))
		p.str("description", d.Description)
		p.str("code", d.Code)
		p.str("message", d.Message)
		p.bool("resumable", d.Resumable)
		return p.node()
	case *ast.SubbotDecl:
		p := w.props("subbot")
		p.lead.set("subbot", str(d.Name))
		p.str("description", d.Description)
		p.str("source", d.Source)
		p.set("with", w.withMap(d.With))
		p.str("output", d.Output)
		p.set("needs", identOrList(d.Needs))
		p.bool("isolated", d.Isolated)
		return p.node()
	}
	w.fail(fmt.Errorf("author: no writer for a %s node", n.kind))
	return nullNode()
}

func (w *writer) llm(kind, name string, d *ast.LLMDecl) *yaml.Node {
	p := w.props(kind)
	p.lead.set(kind, str(name))
	p.str("description", d.Description)
	p.str("model", d.Model)
	p.str("backend", d.Backend)
	p.str("provider", d.Provider)
	p.str("command", d.Command)
	p.str("input", d.Input)
	p.str("output", d.Output)
	p.str("publish", d.Publish)
	p.set("artifact_labels", strList(d.ArtifactLabels))
	p.set("system", w.promptRef(d.System))
	p.set("user", w.promptRef(d.User))
	if d.Session != ast.SessionFresh {
		p.set("session", str(d.Session.String()))
	}
	p.str("session_slot", d.SessionSlot)
	p.set("tools", declaredList(d.Tools))
	p.set("tool_policy", strList(d.ToolPolicy))
	p.set("capabilities", strList(d.Capabilities))
	p.set("skills", strList(d.Skills))
	p.int("tool_max_steps", d.ToolMaxSteps)
	p.int("max_tokens", d.MaxTokens)
	p.str("reasoning_effort", d.ReasoningEffort)
	p.str("timeout", d.Timeout)
	p.bool("readonly", d.Readonly)
	p.bool("full_access", d.FullAccess)
	p.set("images", strList(d.Images))
	if d.Interaction != ast.InteractionNone {
		p.set("interaction", str(d.Interaction.String()))
	}
	p.str("interaction_prompt", d.InteractionPrompt)
	p.str("interaction_model", d.InteractionModel)
	if d.Await != ast.AwaitNone {
		p.set("await", str(d.Await.String()))
	}
	p.str("compress", d.Compress)
	p.str("auto_memory", d.AutoMemory)
	p.str("permission", d.Permission)
	p.set("allow", strList(d.Allow))
	p.set("ask", strList(d.Ask))
	p.set("deny", strList(d.Deny))
	p.set("needs", identOrList(d.Needs))
	if len(d.Fallbacks) > 0 {
		items := make([]*yaml.Node, 0, len(d.Fallbacks))
		for _, fb := range d.Fallbacks {
			items = append(items, w.fallback(fb))
		}
		p.set("fallbacks", blockSeq(items))
	}
	if d.MCP != nil {
		p.set("mcp", w.mcpConfig(d.MCP))
	}
	if d.Compaction != nil {
		p.set("compaction", w.compaction(d.Compaction))
	}
	if d.Memory != nil {
		p.set("memory", w.memory(d.Memory))
	}
	if d.Sandbox != nil {
		p.set("sandbox", w.sandbox(d.Sandbox))
	}
	if d.Cursors != nil {
		p.set("cursors", w.cursors(d.Cursors))
	}
	return p.node()
}

func (w *writer) fallback(fb *ast.FallbackDecl) *yaml.Node {
	p := w.props("fallback")
	p.lead.set("route", str(fb.Name))
	p.str("backend", fb.Backend)
	p.str("model", fb.Model)
	p.str("provider", fb.Provider)
	p.set("on", strList(fb.On))
	p.bool("metered", fb.Metered)
	p.str("action", fb.Action)
	p.str("when", fb.When)
	return p.node()
}

func (w *writer) mcpConfig(c *ast.MCPConfigDecl) *yaml.Node {
	p := w.props("mcp")
	if c.AutoloadProject != nil {
		p.set("autoload_project", boolNode(*c.AutoloadProject))
	}
	if c.Inherit != nil {
		p.set("inherit", boolNode(*c.Inherit))
	}
	p.set("servers", strList(c.Servers))
	p.set("disable", strList(c.Disable))
	return p.node()
}

func (w *writer) compaction(c *ast.CompactionBlock) *yaml.Node {
	p := w.props("compaction")
	if c.Threshold != nil {
		p.set("threshold", floatNode(strconv.FormatFloat(*c.Threshold, 'f', -1, 64)))
	}
	if c.PreserveRecent != nil {
		p.set("preserve_recent", intNode(int64(*c.PreserveRecent)))
	}
	return p.node()
}

func (w *writer) memory(m *ast.MemoryBlock) *yaml.Node {
	p := w.props("memory")
	if m.Enabled != nil {
		p.set("enabled", boolNode(*m.Enabled))
	}
	if m.Scope != nil {
		p.set("scope", str(*m.Scope))
	}
	p.set("autoload", strList(m.Autoload))
	if m.Read != nil {
		p.set("read", boolNode(*m.Read))
	}
	if m.Write != nil {
		p.set("write", boolNode(*m.Write))
	}
	if m.PreCompactInject != nil {
		p.set("pre_compact_inject", boolNode(*m.PreCompactInject))
	}
	if m.ProjectRoot != nil {
		p.set("project_root", boolNode(*m.ProjectRoot))
	}
	if m.Visibility != nil {
		p.set("visibility", str(*m.Visibility))
	}
	return p.node()
}

// sandbox writes the short form (`auto`, `none`) when the block holds a
// mode and nothing else, the block form otherwise (mode `inline` is what
// a block form reads as, and is not written).
func (w *writer) sandbox(sb *ast.SandboxBlock) *yaml.Node {
	bodyEmpty := sb.Image == "" && sb.Build == nil && sb.User == "" && sb.WorkspaceFolder == "" && sb.HostState == "" && sb.PostCreate == "" && sb.Env == nil && len(sb.Mounts) == 0 && sb.Network == nil
	if bodyEmpty && sb.Mode != "" && sb.Mode != "inline" {
		return str(sb.Mode)
	}
	p := w.props("sandbox")
	if sb.Mode != "" && sb.Mode != "inline" {
		p.str("mode", sb.Mode)
	}
	p.str("image", sb.Image)
	if sb.Build != nil {
		b := w.props("sandbox.build")
		b.str("dockerfile", sb.Build.Dockerfile)
		b.str("context", sb.Build.Context)
		if sb.Build.Args != nil {
			b.set("args", w.stringMap("the build args", sb.Build.Args))
		}
		p.set("build", b.node())
	}
	p.str("user", sb.User)
	p.str("workspace_folder", sb.WorkspaceFolder)
	p.str("host_state", sb.HostState)
	p.str("post_create", sb.PostCreate)
	if sb.Env != nil {
		p.set("env", w.stringMap("the sandbox env", sb.Env))
	}
	p.set("mounts", strList(sb.Mounts))
	if sb.Network != nil {
		n := w.props("sandbox.network")
		n.str("mode", sb.Network.Mode)
		n.str("preset", sb.Network.Preset)
		n.str("inherit", sb.Network.Inherit)
		n.set("rules", strList(sb.Network.Rules))
		p.set("network", n.node())
	}
	return p.node()
}

// stringMap writes a map of strings with its keys sorted — the AST holds
// a Go map, which has no order to keep.
func (w *writer) stringMap(what string, m map[string]string) *yaml.Node {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := w.named(what)
	for _, k := range keys {
		out.set(k, str(m[k]))
	}
	if len(keys) == 0 {
		out.flow()
	}
	return out.node()
}

func (w *writer) cursors(cb *ast.CursorBlock) *yaml.Node {
	p := w.props("cursors")
	if !cb.Enabled {
		p.set("enabled", boolNode(false))
	}
	for _, s := range cb.Settings {
		if p.kind.Has(s.Key) {
			w.fail(fmt.Errorf("author: a cursor setting is named %q, the block's own key", s.Key))
			continue
		}
		p.trail.set(s.Key, str(s.Value))
	}
	return p.node()
}

func (w *writer) router(d *ast.RouterDecl) *yaml.Node {
	p := w.props("router")
	p.lead.set("router", str(d.Name))
	p.str("description", d.Description)
	if d.Mode != ast.RouterFanOutAll {
		p.set("mode", str(d.Mode.String()))
	}
	p.str("model", d.Model)
	p.str("backend", d.Backend)
	p.str("provider", d.Provider)
	p.set("system", w.promptRef(d.System))
	p.set("user", w.promptRef(d.User))
	p.bool("multi", d.Multi)
	p.str("reasoning_effort", d.ReasoningEffort)
	p.str("over", d.Over)
	p.str("as", d.As)
	p.str("key", d.Key)
	p.str("depends_on", d.DependsOn)
	p.set("needs", identOrList(d.Needs))
	return p.node()
}

func (w *writer) human(d *ast.HumanDecl) *yaml.Node {
	p := w.props("human")
	p.lead.set("human", str(d.Name))
	p.str("description", d.Description)
	p.str("input", d.Input)
	p.str("output", d.Output)
	p.str("publish", d.Publish)
	p.set("artifact_labels", strList(d.ArtifactLabels))
	p.set("instructions", w.promptRef(d.Instructions))
	p.set("system", w.promptRef(d.System))
	p.str("model", d.Model)
	if d.Interaction != ast.InteractionNone {
		p.set("interaction", str(d.Interaction.String()))
	}
	p.str("interaction_prompt", d.InteractionPrompt)
	p.str("interaction_model", d.InteractionModel)
	p.int("min_answers", d.MinAnswers)
	if d.Await != ast.AwaitNone {
		p.set("await", str(d.Await.String()))
	}
	p.str("review_url", d.ReviewURL)
	p.str("posture", d.Posture)
	p.str("merge_strategy", d.MergeStrategy)
	p.str("merge_into", d.MergeInto)
	p.int("max_turns", d.MaxTurns)
	return p.node()
}

func (w *writer) tool(d *ast.ToolNodeDecl) *yaml.Node {
	p := w.props("tool")
	p.lead.set("tool", str(d.Name))
	p.str("description", d.Description)
	p.str("command", d.Command)
	p.str("script", d.Script)
	p.str("language", d.Language)
	p.str("input", d.Input)
	p.str("output", d.Output)
	p.str("publish", d.Publish)
	p.set("artifact_labels", strList(d.ArtifactLabels))
	if d.Await != ast.AwaitNone {
		p.set("await", str(d.Await.String()))
	}
	if d.Sandbox != nil {
		p.set("sandbox", w.sandbox(d.Sandbox))
	}
	p.str("compress", d.Compress)
	p.str("permission", d.Permission)
	p.set("needs", identOrList(d.Needs))
	p.bool("parallel_safe", d.ParallelSafe)
	p.str("goal", d.Goal)
	p.str("postcondition", d.Postcondition)
	p.str("policy", d.Policy)
	if d.Recovery != nil {
		r := w.props("recovery")
		r.int("max_repair_attempts", d.Recovery.MaxRepairAttempts)
		r.int("max_agent_attempts", d.Recovery.MaxAgentAttempts)
		r.str("model", d.Recovery.Model)
		r.set("agent_tools", strList(d.Recovery.AgentTools))
		p.set("recovery", r.node())
	}
	p.str("action", d.Action)
	p.str("connection", d.Connection)
	if len(d.Params) > 0 {
		m := w.named("the tool " + d.Name + "'s params")
		for _, ap := range d.Params {
			m.set(ap.Key, str(ap.Value))
		}
		p.set("params", m.node())
	}
	p.str("retry", d.Retry)
	p.str("timeout", d.Timeout)
	return p.node()
}

func (w *writer) compute(d *ast.ComputeDecl) *yaml.Node {
	p := w.props("compute")
	p.lead.set("compute", str(d.Name))
	p.str("description", d.Description)
	p.str("input", d.Input)
	p.str("output", d.Output)
	p.str("publish", d.Publish)
	p.set("artifact_labels", strList(d.ArtifactLabels))
	if d.Await != ast.AwaitNone {
		p.set("await", str(d.Await.String()))
	}
	if len(d.Expr) > 0 {
		m := w.named("the compute " + d.Name + "'s expr")
		for _, e := range d.Expr {
			m.set(e.Key, str(e.Expr))
		}
		p.set("expr", m.node())
	}
	return p.node()
}

// withMap writes a with map, flow style; a key named twice is refused.
func (w *writer) withMap(entries []*ast.WithEntry) *yaml.Node {
	if len(entries) == 0 {
		return nil
	}
	m := w.named("a with map").flow()
	for _, e := range entries {
		m.set(e.Key, str(e.Value))
	}
	return m.node()
}

// ---- groups, uses, workflow ----

func (w *writer) group(g *ast.GroupDecl) *yaml.Node {
	m := newMap().set("group", str(g.Name))
	m.set("params", strList(g.Params))
	m.set("nodes", w.nodes(groupNodes(g)))
	m.set("edges", w.edges(g.Edges))
	return m.node()
}

func (w *writer) use(u *ast.UseDecl) *yaml.Node {
	m := newMap().set("use", str(u.Group)).set("as", str(u.Prefix))
	m.set("with", w.withMap(u.With))
	return m.node()
}

func (w *writer) workflow(wf *ast.WorkflowDecl) *yaml.Node {
	p := w.props("workflow")
	p.lead.set("name", str(wf.Name))
	p.str("entry", wf.Entry)
	p.str("contract", wf.Contract)
	if wf.Vars != nil {
		p.set("vars", w.vars(wf.Vars))
	}
	if wf.Attachments != nil {
		p.set("attachments", w.attachments(wf.Attachments))
	}
	if wf.Budget != nil {
		b := w.props("budget")
		b.int("max_parallel_branches", wf.Budget.MaxParallelBranches)
		b.str("max_duration", wf.Budget.MaxDuration)
		if wf.Budget.MaxCostUSD != 0 {
			b.set("max_cost_usd", numberNode(wf.Budget.MaxCostUSD))
		}
		b.int("max_tokens", wf.Budget.MaxTokens)
		b.int("warn_tokens", wf.Budget.WarnTokens)
		b.int("max_iterations", wf.Budget.MaxIterations)
		p.set("budget", b.node())
	}
	if wf.Resources != nil {
		p.set("resources", w.resourcesNode(wf.Resources))
	}
	if wf.MCP != nil {
		p.set("mcp", w.mcpConfig(wf.MCP))
	}
	if wf.Compaction != nil {
		p.set("compaction", w.compaction(wf.Compaction))
	}
	if wf.Sandbox != nil {
		p.set("sandbox", w.sandbox(wf.Sandbox))
	}
	p.str("worktree", wf.Worktree)
	p.str("default_backend", wf.DefaultBackend)
	p.str("compress", wf.Compress)
	p.str("auto_memory", wf.AutoMemory)
	p.str("loop_budget_guard", wf.LoopBudgetGuard)
	p.str("repo_devbox", wf.RepoDevbox)
	p.str("workspace_checkpoint", wf.WorkspaceCheckpoint)
	p.str("permission", wf.Permission)
	p.set("allow", strList(wf.Allow))
	p.set("ask", strList(wf.Ask))
	p.set("deny", strList(wf.Deny))
	p.set("tool_policy", strList(wf.ToolPolicy))
	p.set("capabilities", strList(wf.Capabilities))
	p.set("skills", strList(wf.Skills))
	if wf.Interaction != nil {
		p.set("interaction", str(wf.Interaction.String()))
	}
	p.trail.set("edges", w.edges(wf.Edges))
	return p.node()
}

// numberNode writes a non-negative number as the .bot reads one: an
// integer as digits, a float with its fraction.
func numberNode(f float64) *yaml.Node {
	if f == float64(int64(f)) {
		return intNode(int64(f))
	}
	return floatNode(strconv.FormatFloat(f, 'f', -1, 64))
}

// resourcesNode writes the resources by name, sorted (the AST holds maps):
// a pool as its member list, a semaphore as its count.
func (w *writer) resourcesNode(r *ast.ResourcesBlock) *yaml.Node {
	names := make([]string, 0, len(r.Capacities))
	for n := range r.Capacities {
		names = append(names, n)
	}
	sort.Strings(names)
	m := w.named("resources")
	for _, n := range names {
		if members, ok := r.Members[n]; ok {
			list := strList(members)
			if list == nil {
				list = flowSeq(nil)
			}
			m.set(n, list)
			continue
		}
		m.set(n, intNode(int64(r.Capacities[n])))
	}
	if len(names) == 0 {
		m.flow()
	}
	return m.node()
}

// edges writes edge lines in the .bot's own grammar, one string each.
func (w *writer) edges(edges []*ast.Edge) *yaml.Node {
	if len(edges) == 0 {
		return nil
	}
	items := make([]*yaml.Node, 0, len(edges))
	for _, e := range edges {
		items = append(items, str(edgeText(e)))
	}
	return blockSeq(items)
}

// edgeText is one edge as the .bot writes it — the grammar
// parser.ParseEdgeLine reads back, its strings in the standard escapes.
func edgeText(e *ast.Edge) string {
	var b strings.Builder
	b.WriteString(e.From + " -> " + e.To)
	if e.IsElse {
		b.WriteString(" else")
	}
	if e.When != nil {
		if e.When.Expr != "" {
			b.WriteString(" when " + unparse.QuoteStrict(e.When.Expr))
		} else {
			b.WriteString(" when ")
			if e.When.Negated {
				b.WriteString("not ")
			}
			b.WriteString(e.When.Condition)
		}
	}
	if e.Loop != nil {
		switch {
		case e.Loop.Unbounded:
			if e.Loop.FuelCap > 0 {
				fmt.Fprintf(&b, " as %s(unbounded %d)", e.Loop.Name, e.Loop.FuelCap)
			} else {
				fmt.Fprintf(&b, " as %s(unbounded)", e.Loop.Name)
			}
		case e.Loop.MaxIterationsExpr != "":
			fmt.Fprintf(&b, " as %s(%s)", e.Loop.Name, unparse.QuoteStrict(e.Loop.MaxIterationsExpr))
		default:
			fmt.Fprintf(&b, " as %s(%d)", e.Loop.Name, e.Loop.MaxIterations)
		}
	}
	if e.Foreach != nil {
		fmt.Fprintf(&b, " as foreach %s(%s in %s)", e.Foreach.Name, e.Foreach.Item, unparse.QuoteStrict(e.Foreach.Collection))
	}
	if len(e.With) > 0 {
		var parts []string
		for _, we := range e.With {
			parts = append(parts, we.Key+": "+unparse.QuoteStrict(we.Value))
		}
		b.WriteString(" with { " + strings.Join(parts, ", ") + " }")
	}
	return b.String()
}
