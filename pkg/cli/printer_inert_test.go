package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/forge"
	"github.com/SocialGouv/iterion/pkg/runtime"
)

// hostileText carries what a terminal would act on: an OSC title and a
// clipboard write, a screen clear, a C1 CSI, DEL, bidi overrides and
// isolates, a line separator, a carriage return.
const hostileText = "x\x1b]0;owned\x07\x1b]52;c;cGF5bG9hZA==\x07\x1b[2J\u009b2J\u007f\u202edcba\u2066 \r"

// TestPrinter_humanOutputIsInert: in human mode every way out of the
// Printer — a header, a line, the watch health's lines, the JSON human
// rendering, JSON itself — shows what a server relayed as text, escaped,
// and never sends it raw.
func TestPrinter_humanOutputIsInert(t *testing.T) {
	body, err := json.Marshal(map[string]any{"data": map[string]any{"summary": hostileText}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		print func(p *Printer)
	}{
		{"a header", func(p *Printer) {
			printBoardBinding(p, forge.BoardBinding{TenantID: "team", ProjectTitle: "Roadmap" + hostileText})
		}},
		{"a line", func(p *Printer) { p.Line("status: %s", hostileText) }},
		{"the watch health's lines", func(p *Printer) {
			printRemoteWatchHealth(p, remoteWatchHealth{RunID: "r", Watches: []remoteWatchHealthWatch{{
				WatchID: "w1" + hostileText, AssistantStatus: "running" + hostileText, Attention: []string{hostileText},
				Episodes: []remoteWatchHealthEpisode{{EpisodeID: "e1", State: "pending" + hostileText, LastErrorReason: hostileText}},
			}}}, false)
		}},
		{"the JSON human rendering", func(p *Printer) { PrintRemoteJSON(p, body) }},
		{"JSON", func(p *Printer) { p.JSON(map[string]string{"summary": hostileText}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			tc.print(&Printer{W: &buf, Format: OutputHuman})
			if r, raw := rawControl(buf.String()); raw {
				t.Fatalf("sends %U raw to the terminal: %q", r, buf.String())
			}
			if !strings.Contains(buf.String(), `\u202e`) {
				t.Fatalf("does not show the relayed text escaped: %q", buf.String())
			}
		})
	}
}

// TestPrinter_humanOutputKeepsItsLayout: the line feeds and tabs a Printer
// writes in human mode are its layout, written as they are.
func TestPrinter_humanOutputKeepsItsLayout(t *testing.T) {
	var buf bytes.Buffer
	(&Printer{W: &buf, Format: OutputHuman}).Line("a\tb\nc")
	if buf.String() != "a\tb\nc\n" {
		t.Fatalf("the layout is not written as it is: %q", buf.String())
	}
}

// TestPrinter_jsonModeWritesTheEncodingAsIs: in JSON mode the output is for
// a machine — the server's body passes byte for byte, and stays JSON.
func TestPrinter_jsonModeWritesTheEncodingAsIs(t *testing.T) {
	body, err := json.Marshal(map[string]any{"summary": "ok\u009b\u007f\u202e"})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	PrintRemoteJSON(&Printer{W: &buf, Format: OutputJSON}, body)
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil || got["summary"] != "ok\u009b\u007f\u202e" {
		t.Fatalf("JSON mode does not pass the body as is: %v, %q", err, buf.String())
	}
}

// TestRemoteRunsRaw_writesWhatTheServerSent: a run's log is shown as its
// processes wrote it — colors included — in human mode too.
func TestRemoteRunsRaw_writesWhatTheServerSent(t *testing.T) {
	const log = "\x1b[32mok\x1b[0m step one\n\tindented\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(log))
	}))
	defer srv.Close()
	var buf bytes.Buffer
	c := &RemoteClient{cfg: RemoteConfig{BaseURL: srv.URL, Token: "tok"}, http: srv.Client()}
	if err := RemoteRunsRaw(context.Background(), c, &Printer{W: &buf, Format: OutputHuman}, "r1", "/log"); err != nil {
		t.Fatal(err)
	}
	if buf.String() != log {
		t.Fatalf("the log is not shown as the server sent it: %q", buf.String())
	}
}

// TestPrintError_isInert: an error carries what a server or a run's
// processes wrote; the CLI shows it escaped.
func TestPrintError_isInert(t *testing.T) {
	for _, err := range []error{
		&runtime.RuntimeError{Code: "X", Message: "boom" + hostileText, NodeID: "n" + hostileText, Hint: "retry" + hostileText},
		errors.New("plain" + hostileText),
	} {
		var buf bytes.Buffer
		PrintError(&buf, err)
		if r, raw := rawControl(buf.String()); raw {
			t.Fatalf("PrintError sends %U raw to the terminal: %q", r, buf.String())
		}
		if !strings.Contains(buf.String(), `\x1b`) {
			t.Fatalf("PrintError does not show the error escaped: %q", buf.String())
		}
	}
}

// TestPrinter_hasOneWayOut: no file of the package but output.go reaches a
// Printer's writer — every human write passes through its choke point.
func TestPrinter_hasOneWayOut(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "output.go" {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "W" {
				t.Errorf("%s writes to a Printer's writer, around its choke point", fset.Position(sel.Pos()))
			}
			return true
		})
	}
}
