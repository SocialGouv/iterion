// Package sessionpack packs and unpacks CLI session files for ADR-089 persist.
package sessionpack

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	headerName    = ".iterion-session.json"
	maxFiles      = 64
	maxFileBytes  = 16 << 20
	maxTotalBytes = 32 << 20
	headerBackend = "backend"
	headerSession = "session_id"
)

// Header is stored as the first tar member.
type Header struct {
	Backend   string `json:"backend"`
	SessionID string `json:"session_id"`
}

// File is one session artifact (relative path + bytes).
type File struct {
	Name string
	Body []byte
}

// Pack builds a tar archive from header + files. Rejects unsafe names.
func Pack(h Header, files []File) ([]byte, error) {
	if h.Backend == "" || h.SessionID == "" {
		return nil, fmt.Errorf("sessionpack: header requires backend and session_id")
	}
	if len(files) > maxFiles {
		return nil, fmt.Errorf("sessionpack: too many files (%d > %d)", len(files), maxFiles)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hb, err := json.Marshal(h)
	if err != nil {
		return nil, err
	}
	if err := writeTar(tw, headerName, hb); err != nil {
		return nil, err
	}
	total := int64(len(hb))
	for _, f := range files {
		if err := checkRelPath(f.Name); err != nil {
			return nil, err
		}
		if int64(len(f.Body)) > maxFileBytes {
			return nil, fmt.Errorf("sessionpack: file %s exceeds cap", f.Name)
		}
		total += int64(len(f.Body))
		if total > maxTotalBytes {
			return nil, fmt.Errorf("sessionpack: archive exceeds cap")
		}
		if err := writeTar(tw, f.Name, f.Body); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Unpack extracts a pack into destDir after validating header and paths.
func Unpack(blob []byte, want Header, destDir string) error {
	tr := tar.NewReader(bytes.NewReader(blob))
	var got Header
	haveHeader := false
	n := 0
	var total int64
	tmp, err := os.MkdirTemp(filepath.Dir(destDir), ".sess-unpack-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("sessionpack: tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			return fmt.Errorf("sessionpack: refused entry type %v for %s", hdr.Typeflag, hdr.Name)
		}
		if err := checkRelPath(hdr.Name); err != nil {
			return err
		}
		n++
		if n > maxFiles+1 {
			return fmt.Errorf("sessionpack: too many members")
		}
		if hdr.Size > maxFileBytes {
			return fmt.Errorf("sessionpack: member %s too large", hdr.Name)
		}
		body, err := io.ReadAll(io.LimitReader(tr, hdr.Size+1))
		if err != nil {
			return err
		}
		if int64(len(body)) != hdr.Size {
			return fmt.Errorf("sessionpack: size mismatch for %s", hdr.Name)
		}
		total += int64(len(body))
		if total > maxTotalBytes {
			return fmt.Errorf("sessionpack: archive exceeds cap")
		}
		if hdr.Name == headerName {
			if err := json.Unmarshal(body, &got); err != nil {
				return fmt.Errorf("sessionpack: header: %w", err)
			}
			haveHeader = true
			continue
		}
		out := filepath.Join(tmp, filepath.FromSlash(hdr.Name))
		if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(out, body, 0o600); err != nil {
			return err
		}
	}
	if !haveHeader {
		return fmt.Errorf("sessionpack: missing header")
	}
	if got.Backend != want.Backend || got.SessionID != want.SessionID {
		return fmt.Errorf("sessionpack: header mismatch (got %s/%s want %s/%s)", got.Backend, got.SessionID, want.Backend, want.SessionID)
	}
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return err
	}
	ents, err := os.ReadDir(tmp)
	if err != nil {
		return err
	}
	for _, e := range ents {
		src := filepath.Join(tmp, e.Name())
		dst := filepath.Join(destDir, e.Name())
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
		if err := os.Rename(src, dst); err != nil {
			return copyTree(src, dst)
		}
	}
	return nil
}

func writeTar(tw *tar.Writer, name string, body []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err := tw.Write(body)
	return err
}

func checkRelPath(name string) error {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
		return fmt.Errorf("sessionpack: unsafe path %q", name)
	}
	if name == headerName {
		return nil
	}
	clean := pathClean(name)
	if clean != name || strings.HasPrefix(clean, "../") || clean == ".." {
		return fmt.Errorf("sessionpack: unsafe path %q", name)
	}
	return nil
}

func pathClean(name string) string {
	parts := strings.Split(name, "/")
	var out []string
	for _, p := range parts {
		switch p {
		case "", ".":
		case "..":
			if len(out) == 0 {
				return ".."
			}
			out = out[:len(out)-1]
		default:
			out = append(out, p)
		}
	}
	return strings.Join(out, "/")
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o600)
	})
}

// CollectBySessionID gathers regular files under root whose base name
// (minus extension) equals sessionID, skipping credential filenames.
func CollectBySessionID(root, sessionID string) ([]File, error) {
	skip := map[string]bool{
		".credentials.json": true,
		"auth.json":         true,
		".claude.json":      true,
	}
	var files []File
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		base := info.Name()
		if skip[base] {
			return nil
		}
		if isHardlink(info) {
			return nil
		}
		stem := strings.TrimSuffix(base, filepath.Ext(base))
		if stem != sessionID && base != sessionID+".jsonl" {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files = append(files, File{Name: filepath.ToSlash(rel), Body: body})
		return nil
	})
	return files, err
}

// HasFile reports whether CollectBySessionID would find anything.
func HasFile(root, sessionID string) bool {
	files, err := CollectBySessionID(root, sessionID)
	return err == nil && len(files) > 0
}
