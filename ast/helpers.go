package ast

// AllNodeNames returns the names of all declared nodes across all types.
// Useful for validation (duplicate detection, edge target resolution).
func (f *File) AllNodeNames() []string {
	var names []string
	for _, n := range f.Agents {
		names = append(names, n.Name)
	}
	for _, n := range f.Judges {
		names = append(names, n.Name)
	}
	for _, n := range f.Routers {
		names = append(names, n.Name)
	}
	for _, n := range f.Humans {
		names = append(names, n.Name)
	}
	for _, n := range f.Tools {
		names = append(names, n.Name)
	}
	return names
}

// AllSchemaNames returns the names of all declared schemas.
func (f *File) AllSchemaNames() []string {
	names := make([]string, len(f.Schemas))
	for i, s := range f.Schemas {
		names[i] = s.Name
	}
	return names
}

// AllPromptNames returns the names of all declared prompts.
func (f *File) AllPromptNames() []string {
	names := make([]string, len(f.Prompts))
	for i, p := range f.Prompts {
		names[i] = p.Name
	}
	return names
}

// AllMCPServerNames returns the names of all top-level MCP server declarations.
func (f *File) AllMCPServerNames() []string {
	names := make([]string, len(f.MCPServers))
	for i, s := range f.MCPServers {
		names[i] = s.Name
	}
	return names
}

// ReservedTargets are node names that cannot be declared — they are
// implicit terminal nodes used as edge targets.
var ReservedTargets = map[string]bool{
	"done": true,
	"fail": true,
}
