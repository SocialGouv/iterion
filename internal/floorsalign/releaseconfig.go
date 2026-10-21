package floorsalign

import (
	"fmt"
	"strings"
)

// ReleaseHook returns the commands .release-it.mjs's source wires to a hook
// slot, in the order release-it runs them. The source is read the way
// JavaScript reads it: a `//` or `/* */` comment outside a string runs
// nothing — a hook line left in a comment, wholly or after a live entry, is
// not wired — while the same characters inside a quoted command are just
// text. The slot must be wired exactly once, as a list of quoted commands,
// under a single- or double-quoted key.
func ReleaseHook(src []byte, slot string) ([]string, error) {
	toks, err := jsTokens(string(src))
	if err != nil {
		return nil, err
	}
	var lists [][]string
	for i := 0; i+2 < len(toks); i++ {
		if !toks[i].str || toks[i].text != slot || toks[i+1].text != ":" || toks[i+2].text != "[" {
			continue
		}
		var commands []string
		closed := false
		for j := i + 3; j < len(toks); j++ {
			switch {
			case toks[j].str:
				commands = append(commands, toks[j].text)
			case toks[j].text == "]":
				closed = true
			case toks[j].text == ",":
				continue
			default:
				return nil, fmt.Errorf("the release config's %q list holds %q, not a quoted command", slot, toks[j].text)
			}
			if closed {
				break
			}
		}
		if !closed {
			return nil, fmt.Errorf("the release config's %q list never closes", slot)
		}
		lists = append(lists, commands)
	}
	if len(lists) != 1 {
		return nil, fmt.Errorf("the release config wires %q %d time(s) as a command list, want exactly once", slot, len(lists))
	}
	return lists[0], nil
}

// jsToken is a string literal's value (str) or a single punctuation or word
// token; comments and whitespace produce none.
type jsToken struct {
	text string
	str  bool
}

// jsTokens splits JavaScript source into string literals and the other
// tokens a hook list is made of, dropping comments outside strings. It is not
// a parser: it knows exactly enough — quotes with escapes, line and block
// comments — to tell a live hook list from a commented one.
func jsTokens(src string) ([]jsToken, error) {
	var toks []jsToken
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return nil, fmt.Errorf("the release config has an unterminated /* comment")
			}
			i += end + 4
		case c == '\'' || c == '"' || c == '`':
			var b strings.Builder
			j := i + 1
			for ; j < len(src) && src[j] != c; j++ {
				if src[j] == '\\' && j+1 < len(src) {
					j++
				}
				b.WriteByte(src[j])
			}
			if j >= len(src) {
				return nil, fmt.Errorf("the release config has an unterminated %c string", c)
			}
			toks = append(toks, jsToken{text: b.String(), str: true})
			i = j + 1
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case strings.IndexByte("[]{}(),:;=", c) >= 0:
			toks = append(toks, jsToken{text: string(c)})
			i++
		default:
			j := i
			for j < len(src) && strings.IndexByte(" \t\n\r[]{}(),:;='\"`/", src[j]) < 0 {
				j++
			}
			if j == i {
				j++
			}
			toks = append(toks, jsToken{text: src[i:j]})
			i = j
		}
	}
	return toks, nil
}
