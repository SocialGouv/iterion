// Package natsconfig resolves the supported static NATS source bundle with
// the upstream broker parser. Results contain credentials and must remain
// inside the privileged authority adapter; never log or persist Config.
package natsconfig

import (
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strings"
)

const (
	MaxSourceBytes  = 512 * 1024
	MaxSourceFiles  = 32
	MaxMessageBytes = 4 * 1024 * 1024
)

type Sources struct {
	Entry string            `json:"entry"`
	Files map[string]string `json:"files"`
}

// This is a source-boundary restriction, not a NATS grammar implementation.
// The upstream parser still interprets the untouched files. Initially includes
// must occupy a whole line: include "relative.conf". Refuse any other use of
// the word (even in a comment/value) before invoking that parser, whose include
// resolver otherwise has unrestricted filesystem access. This conservative
// subset cannot silently read configuration outside the supplied source set.
var includeLine = regexp.MustCompile(`^\s*include[ \t]+"([a-zA-Z0-9_./-]+)"[ \t]*;?[ \t]*(?:(?:#|//).*)?$`)

func sourceName(name string) bool {
	return name != "." && fs.ValidPath(name) && len(name) <= 255 &&
		strings.Trim(name, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_./-") == ""
}

func (s Sources) Validate() error {
	if !sourceName(s.Entry) || len(s.Files) == 0 || len(s.Files) > MaxSourceFiles {
		return fmt.Errorf("NATS authority sources need a relative entry file and at most %d files", MaxSourceFiles)
	}
	if _, ok := s.Files[s.Entry]; !ok {
		return fmt.Errorf("NATS authority entry file is absent from supplied sources")
	}
	size := 0
	edges := make(map[string][]string)
	for name, body := range s.Files {
		if !sourceName(name) {
			return fmt.Errorf("NATS authority source path is outside the supported relative-file profile")
		}
		size += len(body)
		if size > MaxSourceBytes {
			return fmt.Errorf("NATS authority sources exceed %d bytes", MaxSourceBytes)
		}
		for _, line := range strings.Split(body, "\n") {
			if !strings.Contains(strings.ToLower(line), "include") {
				continue
			}
			match := includeLine.FindStringSubmatch(line)
			if match == nil || !sourceName(match[1]) {
				return fmt.Errorf("NATS authority includes require a whole-line relative quoted path; other uses of include are unsupported")
			}
			included := path.Join(path.Dir(name), match[1])
			if _, ok := s.Files[included]; !ok {
				return fmt.Errorf("NATS authority include is not present in supplied sources")
			}
			edges[name] = append(edges[name], included)
		}
	}
	visited, active := make(map[string]bool), make(map[string]bool)
	expandedBytes, expandedFiles := make(map[string]int), make(map[string]int)
	var walk func(string) error
	walk = func(name string) error {
		if active[name] {
			return fmt.Errorf("NATS authority sources contain an include cycle")
		}
		if visited[name] {
			return nil
		}
		active[name] = true
		expandedBytes[name], expandedFiles[name] = len(s.Files[name]), 1
		for _, next := range edges[name] {
			if err := walk(next); err != nil {
				return err
			}
			expandedBytes[name] += expandedBytes[next]
			expandedFiles[name] += expandedFiles[next]
			if expandedBytes[name] > 4*MaxSourceBytes || expandedFiles[name] > 256 {
				return fmt.Errorf("NATS authority include expansion exceeds the supported source profile")
			}
		}
		active[name], visited[name] = false, true
		return nil
	}
	for name := range s.Files {
		if err := walk(name); err != nil {
			return err
		}
	}
	return nil
}
