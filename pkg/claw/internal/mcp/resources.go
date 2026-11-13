package mcp

import "context"

// McpResourceInfo describes an MCP resource.
type McpResourceInfo struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// McpResourceContent holds the full content of a read MCP resource, including
// metadata fields matching Rust's response shape.
type McpResourceContent struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mime_type,omitempty"`
	Content     string `json:"content"`
}

// ListResources queries the MCP server for available resources.
func (c *Client) ListResources(ctx context.Context) ([]McpResourceInfo, error) {
	var result struct {
		Resources []McpResourceInfo `json:"resources"`
	}
	err := c.call(ctx, "resources/list", nil, &result)
	if err != nil {
		return nil, err
	}
	return result.Resources, nil
}

// ReadResource reads a specific resource by URI from the MCP server.
// Returns an McpResourceContent with metadata and content text.
func (c *Client) ReadResource(ctx context.Context, uri string) (McpResourceContent, error) {
	params := map[string]any{
		"uri": uri,
	}
	var result struct {
		Contents []struct {
			URI         string `json:"uri"`
			Name        string `json:"name,omitempty"`
			Description string `json:"description,omitempty"`
			MimeType    string `json:"mimeType,omitempty"`
			Text        string `json:"text,omitempty"`
		} `json:"contents"`
	}
	err := c.call(ctx, "resources/read", params, &result)
	if err != nil {
		return McpResourceContent{}, err
	}
	if len(result.Contents) == 0 {
		return McpResourceContent{URI: uri}, nil
	}
	c0 := result.Contents[0]
	return McpResourceContent{
		URI:         c0.URI,
		Name:        c0.Name,
		Description: c0.Description,
		MimeType:    c0.MimeType,
		Content:     c0.Text,
	}, nil
}
