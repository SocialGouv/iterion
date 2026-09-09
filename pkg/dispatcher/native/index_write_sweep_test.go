package native

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The dirty-set merge of Reconcile rests on one invariant: every write to
// s.index goes through setIndexLocked or dropIndexLocked, which mark the
// id while a scan is in flight. A mutator that wrote the map directly
// would be reverted by the next overflow rebuild without a test noticing,
// so the invariant is held by this sweep over the package's source, the
// way bot_resolver_sweep_test.go holds the role-bot constants.
func TestEveryIndexWriteGoesThroughTheChokePoint(t *testing.T) {
	direct := regexp.MustCompile(`s\.index\[[^\]]+\]\s*=|delete\(s\.index,`)
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "store_index.go" {
			continue
		}
		src, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if direct.MatchString(line) {
				offenders = append(offenders, name+":"+itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("index written outside setIndexLocked/dropIndexLocked (the write would escape the rebuild's dirty set):\n  %s", strings.Join(offenders, "\n  "))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
