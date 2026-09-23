package bundle

import (
	"errors"
	"fmt"
)

// ErrAuthorDocument is the refusal every launcher gives an author document
// (`.bot.yaml`, pkg/dsl/author): a draft of a `.bot`, never a workflow file.
// It is one sentinel so that every surface — the CLI, the HTTP API, the
// MCP tools, the runner's compile floor — refuses the same way and a
// caller tells the refusal apart with errors.Is, before any run, upload or
// store write exists.
var ErrAuthorDocument = errors.New("an author document (.bot.yaml) is a draft of a .bot, not a workflow: `iterion fmt --to bot <file>` writes the .bot it stands for, then launch the .bot")

// AuthorDocumentError is ErrAuthorDocument naming the path it refuses.
func AuthorDocumentError(path string) error {
	return fmt.Errorf("%s: %w", path, ErrAuthorDocument)
}
