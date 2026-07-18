package github

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/SocialGouv/iterion/pkg/forge"
)

// AdminClient satisfies forge.FileClient (GitHub Contents API).
var _ forge.FileClient = (*AdminClient)(nil)

// escapeContentsPath escapes each path segment but preserves the slashes, so a
// nested contents path (dir/file.json) keeps its shape — url.PathEscape alone
// would percent-encode the separators.
func escapeContentsPath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// GetFile reads a single file via GET /repos/{repo}/contents/{path}. A missing
// path returns forge.ErrFileNotFound; a directory (or symlink/submodule) is an
// error, not a silent empty read.
func (c *AdminClient) GetFile(ctx context.Context, repo, path, ref string) (forge.FileRef, error) {
	p := "/repos/" + repo + "/contents/" + escapeContentsPath(path)
	if ref != "" {
		p += "?ref=" + url.QueryEscape(ref)
	}
	var resp struct {
		Content string `json:"content"`
		SHA     string `json:"sha"`
		Type    string `json:"type"`
	}
	code, err := c.do(ctx, http.MethodGet, p, nil, &resp)
	if err != nil {
		return forge.FileRef{}, fmt.Errorf("github: get contents %s: %w", path, err)
	}
	if code == http.StatusNotFound {
		return forge.FileRef{}, forge.ErrFileNotFound
	}
	if code/100 != 2 {
		return forge.FileRef{}, fmt.Errorf("github: get contents %s: status %d", path, code)
	}
	if resp.Type != "file" {
		return forge.FileRef{}, fmt.Errorf("github: %s is not a file (type %q)", path, resp.Type)
	}
	// GitHub base64 payloads are newline-wrapped.
	content, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(resp.Content, "\n", ""))
	if err != nil {
		return forge.FileRef{}, fmt.Errorf("github: decode contents %s: %w", path, err)
	}
	return forge.FileRef{Path: path, Content: content, SHA: resp.SHA, Ref: ref}, nil
}

// PutFile writes a single file via PUT /repos/{repo}/contents/{path}. Updating
// an existing file requires in.PrevSHA; a stale value yields HTTP 409, mapped
// to forge.ErrFileConflict so the caller re-reads instead of clobbering.
func (c *AdminClient) PutFile(ctx context.Context, repo string, in forge.PutFile) (forge.FileRef, error) {
	body := map[string]any{
		"message": in.Message,
		"content": base64.StdEncoding.EncodeToString(in.Content),
	}
	if in.Branch != "" {
		body["branch"] = in.Branch
	}
	if in.PrevSHA != "" {
		body["sha"] = in.PrevSHA
	}
	if in.AuthorName != "" {
		who := map[string]string{"name": in.AuthorName, "email": in.AuthorEmail}
		body["author"] = who
		body["committer"] = who
	}
	var resp struct {
		Content struct {
			SHA string `json:"sha"`
		} `json:"content"`
	}
	code, err := c.do(ctx, http.MethodPut, "/repos/"+repo+"/contents/"+escapeContentsPath(in.Path), body, &resp)
	if err != nil {
		return forge.FileRef{}, fmt.Errorf("github: put contents %s: %w", in.Path, err)
	}
	if code == http.StatusConflict {
		return forge.FileRef{}, forge.ErrFileConflict
	}
	if code/100 != 2 {
		return forge.FileRef{}, fmt.Errorf("github: put contents %s: status %d", in.Path, code)
	}
	return forge.FileRef{Path: in.Path, Content: in.Content, SHA: resp.Content.SHA, Ref: in.Branch}, nil
}
