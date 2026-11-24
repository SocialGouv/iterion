package compatgw

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/pricefeed"
)

// The sandbox image a host may cross into predates the run table: its
// route reads OPENAI_COMPATIBLE_REASONING_EFFORTS alone, by exact wire id,
// through its own parser. olderImageEfforts is that reader, frozen as
// origin/main shipped it before ADR-166 (compatgw.ReasoningEfforts and
// parseReasoningEfforts, verbatim), so a vocabulary this package gains
// later is judged by what the older image actually accepts — never by
// today's parser.
func olderImageEfforts(modelID string, get func(string) string, warn func(string, ...any)) []string {
	raw := catalogEnv(get, ReasoningEffortsEnv)
	if raw == "" {
		return nil
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &entries); err != nil || entries == nil {
		warn("REASONING_EFFORTS_INVALID: %s must be a JSON object; ignoring effort mapping", ReasoningEffortsEnv)
		return nil
	}
	keys := make([]string, 0, len(entries))
	for id := range entries {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	var result []string
	for _, id := range keys {
		values, err := olderImageParse(entries[id])
		if strings.TrimSpace(id) == "" {
			err = fmt.Errorf("empty model id")
		}
		if err != nil {
			warn("REASONING_EFFORTS_INVALID: %s[%q]: %v; ignoring entry", ReasoningEffortsEnv, id, err)
			continue
		}
		if id == modelID {
			result = values
		}
	}
	return result
}

func olderImageParse(raw json.RawMessage) ([]string, error) {
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil || string(raw) == "null" {
		return nil, fmt.Errorf("expected an array of effort levels or integers 1..100")
	}
	values := make([]string, 0, len(entries))
	for _, entry := range entries {
		var value string
		if err := json.Unmarshal(entry, &value); err != nil {
			value = string(entry)
		}
		value = strings.ToLower(strings.TrimSpace(value))
		switch value {
		case "none", "minimal", "low", "medium", "high", "xhigh", "max":
		default:
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > 100 {
				return nil, fmt.Errorf("unknown effort level (expected none, minimal, low, medium, high, xhigh, max or an integer 1..100)")
			}
			value = strconv.Itoa(n)
		}
		values = append(values, value)
	}
	return values, nil
}

// The ids and lists the randomized tables draw from: spellings of one
// gateway id, other ids, blank ids, and lists the vocabulary accepts and
// refuses — near-miss words ("ultra", 0, 101) among them, so a level the
// vocabulary gains that an older image refuses shows as a disagreement.
var (
	imageIDs    = []string{"scaleway/deepseek-v4.1-flash", "Scaleway/DeepSeek-v4.1-flash", "SCALEWAY/DEEPSEEK-V4.1-FLASH", " scaleway/deepseek-v4.1-flash", "kimi-k2", "glm-5.3", "", " "}
	imageLists  = []string{`["low","high","xhigh","max"]`, `["LOW"," High "]`, `[]`, `null`, `["medium"]`, `[5,"10"]`, `[5.0]`, `["ultra"]`, `"high"`, `[true]`, `["max","max"]`, `[0]`, `[101]`, `["  minimal  "]`}
	imageFeeds  = [][]string{{"low", "high", "xhigh", "max"}, {"high", "max"}, {}, {"ultra"}, {"MEDIUM"}, {"5"}, {"none", "minimal"}, {"100"}}
	imageSilent = func(string, ...any) {}
)

func randomEnvEfforts(r *rand.Rand) string {
	switch r.Intn(10) {
	case 0:
		return ""
	case 1:
		return "not json"
	case 2:
		return "null"
	case 3:
		return "[]"
	}
	parts := make([]string, 0, 4)
	for range r.Intn(4) {
		id, _ := json.Marshal(imageIDs[r.Intn(len(imageIDs))])
		parts = append(parts, string(id)+":"+imageLists[r.Intn(len(imageLists))])
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func randomFeedEfforts(r *rand.Rand) string {
	if r.Intn(3) == 0 {
		return ""
	}
	efforts := map[string]pricefeed.EffortsEntry{}
	for i := range r.Intn(3) {
		id := pricefeed.NormalizeID(imageIDs[r.Intn(len(imageIDs))])
		if id == "" {
			continue
		}
		efforts[id] = pricefeed.EffortsEntry{Feed: "f" + strconv.Itoa(i), Values: imageFeeds[r.Intn(len(imageFeeds))]}
	}
	raw, err := json.Marshal(pricefeed.Table{Entries: map[string]pricefeed.Entry{}, Efforts: efforts})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// With no feed table, the host's ladder answers exactly what origin/main's
// reader answered for every id: a runner with no feed configured maps
// efforts as before ADR-166.
func TestAcceptedEffortsWithNoFeedAnswersAsBefore(t *testing.T) {
	ResetCatalogCaches()
	t.Cleanup(ResetCatalogCaches)
	r := rand.New(rand.NewSource(1))
	mismatches := 0
	for range 20000 {
		env := randomEnvEfforts(r)
		get := mapGet(map[string]string{ReasoningEffortsEnv: env})
		for _, id := range imageIDs {
			before := olderImageEfforts(id, get, imageSilent)
			now, _ := AcceptedEfforts(id, get, imageSilent)
			if !slices.Equal(before, now) && (len(before) > 0 || len(now) > 0) {
				if mismatches++; mismatches <= 5 {
					t.Errorf("env %s, id %q: origin/main answered %v, the ladder %v", env, id, before, now)
				}
			}
		}
	}
	if mismatches > 0 {
		t.Fatalf("%d (env, id) pairs answer differently than before ADR-166", mismatches)
	}
}

// Over random env and feed tables, an older sandbox image reading the
// crossed name with its own frozen parser, and an image of this release
// reading the crossed pair, both answer the list the host answers for
// the node's wire id.
func TestCrossingAgreesWithOlderAndNewerImages(t *testing.T) {
	ResetCatalogCaches()
	t.Cleanup(ResetCatalogCaches)
	r := rand.New(rand.NewSource(2))
	disagreements := 0
	for range 20000 {
		ResetFeedTableCache()
		env, feed := randomEnvEfforts(r), randomFeedEfforts(r)
		host := mapGet(map[string]string{ReasoningEffortsEnv: env, FeedTableEnv: feed})
		for _, wire := range imageIDs {
			if strings.TrimSpace(wire) == "" {
				continue
			}
			want, _ := AcceptedEfforts(wire, host, imageSilent)
			crossed := CrossingReasoningEfforts(wire, host, imageSilent)
			older := olderImageEfforts(wire, mapGet(map[string]string{ReasoningEffortsEnv: crossed}), imageSilent)
			newer, _ := AcceptedEfforts(wire, mapGet(map[string]string{ReasoningEffortsEnv: crossed, FeedTableEnv: feed}), imageSilent)
			for image, got := range map[string][]string{"an older image": older, "a newer image": newer} {
				if !slices.Equal(want, got) && (len(want) > 0 || len(got) > 0) {
					if disagreements++; disagreements <= 5 {
						t.Errorf("%s answers %v for %q, the host %v (env %s, feed %s, crossed %s)", image, got, wire, want, env, feed, crossed)
					}
				}
			}
		}
	}
	if disagreements > 0 {
		t.Fatalf("%d crossings disagree with the host", disagreements)
	}
}
