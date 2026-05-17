package appinfo

import (
	"runtime/debug"
	"strings"
)

const (
	Name    = "iterion"
	RepoURL = "https://github.com/SocialGouv/iterion"
)

// Version is intended to be overridden at build time. The Taskfile
// already wires the correct path; the doc example below mirrors that
// wiring exactly so anyone copying it from the source comment lands a
// real injection. The pkg/ prefix matters: -X flags targeting an
// absent symbol are silently ignored by `go build`, and the binary
// would ship Version = "dev" — breaking release tracking,
// SandboxImageTag (returns "edge"), `/server-info`, MCP clientInfo
// and the `iterion_version` stamped on every run document.
//
// Example:
//
//	go build -ldflags "-X github.com/SocialGouv/iterion/pkg/internal/appinfo.Version=v0.1.0 -X github.com/SocialGouv/iterion/pkg/internal/appinfo.Commit=$(git rev-parse --short HEAD)"
var Version = "dev"

// Commit optionally carries a VCS revision (preferably short SHA).
// It can be set via -ldflags or inferred from Go build settings.
var Commit = ""

func init() {
	if strings.TrimSpace(Commit) != "" {
		return
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" {
			Commit = strings.TrimSpace(s.Value)
			break
		}
	}
}

func FullVersion() string {
	v := strings.TrimSpace(Version)
	if v == "" {
		v = "dev"
	}
	c := strings.TrimSpace(Commit)
	if c == "" {
		return v
	}
	if len(c) > 12 {
		c = c[:12]
	}
	return v + "+" + c
}

// SandboxImageTag returns the tag to use when picking a default
// iterion-sandbox-{slim,full} image. Release builds (Version like
// "v1.2.3") return the version verbatim so a v1.2.3 binary always
// pulls iterion-sandbox-*:v1.2.3. Snapshot/dev builds return "edge"
// to track the rolling main-branch tag published by CI.
func SandboxImageTag() string {
	v := strings.TrimSpace(Version)
	if strings.HasPrefix(v, "v") && len(v) > 1 {
		return v
	}
	return "edge"
}
