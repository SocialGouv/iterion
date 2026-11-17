package cloudpublisher

import (
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/credpool"
	"github.com/SocialGouv/iterion/pkg/reviewtopology"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// TestWithPinnedWants pins the widening the launch depends on: a mono pin
// makes its family's slack a WANT, appended after the route wants and never
// duplicated. Without it, a bot whose routes spend claude and whose
// integration pins mono_family=gpt is refused at launch by
// reviewtopology's unservable-family gate as soon as a tier before the
// platform one grants (the platform skip removes the only gpt source).
//
// Mutants: dropping withPinnedWants from either acquisition site leaves the
// pin unasked and reds the gpt case here; appending before the route wants
// reds the order assertion; skipping the duplicate check reds the claude
// case.
func TestWithPinnedWants(t *testing.T) {
	anthropic := credpool.Credential{Source: credpool.SourceAPIKey, Ref: string(secrets.ProviderAnthropic)}
	codex := credpool.Credential{Source: credpool.SourceOAuth, Ref: string(secrets.OAuthKindCodex)}
	openai := credpool.Credential{Source: credpool.SourceAPIKey, Ref: string(secrets.ProviderOpenAI)}
	routes := []credpool.Credential{anthropic}

	t.Run("no pin leaves the walk byte-identical", func(t *testing.T) {
		// DUAL is the only mode that suppresses a pin (mono_family is
		// deleted at injection there); AUTO demands, like mono.
		for _, vars := range []map[string]string{
			nil,
			{reviewtopology.VarReviewMode: reviewtopology.ModeDual, reviewtopology.VarMonoFamily: "gpt"},
		} {
			if got := withPinnedWants(routes, vars, ""); !reflect.DeepEqual(got, routes) {
				t.Fatalf("withPinnedWants(%v, %v) = %v, want the route wants untouched", routes, vars, got)
			}
		}
		if got := withPinnedWants(routes, map[string]string{reviewtopology.VarMonoFamily: "gpt"}, reviewtopology.ModeDual); !reflect.DeepEqual(got, routes) {
			t.Fatalf("an explicit dual field must suppress the pin, got %v", got)
		}
	})

	t.Run("a gpt pin asks for the gpt family after the routes", func(t *testing.T) {
		got := withPinnedWants(routes, map[string]string{
			reviewtopology.VarReviewMode: reviewtopology.ModeMono,
			reviewtopology.VarMonoFamily: "gpt",
		}, "")
		want := []credpool.Credential{anthropic, codex, openai}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("withPinnedWants = %v, want %v", got, want)
		}
	})

	t.Run("a claude pin never duplicates a want the routes already carry", func(t *testing.T) {
		got := withPinnedWants(routes, map[string]string{
			reviewtopology.VarReviewMode: reviewtopology.ModeMono,
			reviewtopology.VarMonoFamily: "claude",
		}, "")
		seen := map[credpool.Credential]int{}
		for _, w := range got {
			seen[w]++
		}
		for w, n := range seen {
			if n > 1 {
				t.Fatalf("want %v asked %d times — the pin must not duplicate", w, n)
			}
		}
		if got[0] != anthropic {
			t.Fatalf("the route want moved: %v", got)
		}
	})
}
