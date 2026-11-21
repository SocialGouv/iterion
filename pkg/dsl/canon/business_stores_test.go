package canon

import (
	"strings"
	"testing"
)

func TestBusinessStoreCommentsAllowCanonicalDocument(t *testing.T) {
	const source = `dsl: 2
nodes:
  - tool: inspect
    command: "true"
workflow:
  name: test
  business_stores:
    - id: ledger
      kind: postgres
      namespace: accounting
      # Accounting stays required.
      required: true
    - id: ledger
      kind: postgres
      namespace: audit
      # Audit is supplemental.
      required: false
  entry: inspect
  edges:
    - inspect -> done
`
	for name, text := range map[string]string{
		"same-id":    source,
		"kind-first": strings.ReplaceAll(source, "- id: ledger\n      kind: postgres", "- kind: postgres\n      id: ledger"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Document("comments.bot.yaml", []byte(text)); err != nil {
				t.Fatalf("canonical formatter refuses valid business stores: %v", err)
			}
		})
	}
}
