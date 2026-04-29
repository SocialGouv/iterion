package appinfo

import (
	"runtime/debug"
	"strings"
)

const (
	Name    = "iterion"
	RepoURL = "https://github.com/SocialGouv/iterion"
)

// Version is intended to be overridden at build time.
//
// Example:
//
//	go build -ldflags "-X github.com/SocialGouv/iterion/internal/appinfo.Version=v0.1.0 -X github.com/SocialGouv/iterion/internal/appinfo.Commit=$(git rev-parse --short HEAD)"
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
