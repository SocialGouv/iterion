package runtime

import (
	"reflect"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/SocialGouv/iterion/pkg/store"
)

const (
	humanReviewPointsKey      = "ai_review_points"
	maxHumanReviewBriefPoints = 3
	maxHumanReviewPointChars  = 240
	maxHumanReviewBriefChars  = 600
)

var (
	humanReviewURLPattern  = regexp.MustCompile(`(?i)(?:[a-z][a-z0-9+.-]{1,15}://|www\.)`)
	humanReviewUUIDPattern = regexp.MustCompile(
		`(?i)(?:^|[^0-9a-f])[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}(?:$|[^0-9a-f])`,
	)
)

// extractHumanReviewBrief consumes the reserved ai_review_points transport
// field and returns a runtime-stamped brief only when every supplied point is
// safe and within the public contract. Invalid model output is discarded as a
// whole: a partial checklist could materially change the requested review.
//
// The reserved field is removed whether validation succeeds or fails so it can
// never leak into a human answer form, event questions, or a checkpoint.
func extractHumanReviewBrief(questions map[string]any) *store.HumanReviewBrief {
	if questions == nil {
		return nil
	}
	raw, present := questions[humanReviewPointsKey]
	if !present {
		return nil
	}
	delete(questions, humanReviewPointsKey)

	values := reflect.ValueOf(raw)
	if !values.IsValid() || (values.Kind() != reflect.Slice && values.Kind() != reflect.Array) {
		return nil
	}
	if values.Len() < 1 || values.Len() > maxHumanReviewBriefPoints {
		return nil
	}

	points := make([]string, 0, values.Len())
	totalChars := 0
	for i := 0; i < values.Len(); i++ {
		value := values.Index(i)
		if value.Kind() == reflect.Interface {
			if value.IsNil() {
				return nil
			}
			value = value.Elem()
		}
		if value.Kind() != reflect.String {
			return nil
		}

		point := strings.Join(strings.Fields(value.String()), " ")
		pointChars := utf8.RuneCountInString(point)
		if pointChars == 0 || pointChars > maxHumanReviewPointChars || hasUnsafeHumanReviewReference(point) {
			return nil
		}
		totalChars += pointChars
		if totalChars > maxHumanReviewBriefChars {
			return nil
		}
		points = append(points, point)
	}

	return &store.HumanReviewBrief{
		Version: store.HumanReviewBriefVersion,
		Source:  store.HumanReviewBriefSourceAI,
		Points:  points,
	}
}

func hasUnsafeHumanReviewReference(point string) bool {
	if humanReviewURLPattern.MatchString(point) ||
		humanReviewUUIDPattern.MatchString(point) ||
		strings.ContainsAny(point, `/\`) {
		return true
	}
	for _, field := range strings.Fields(point) {
		token := strings.Trim(field, ".,;:!?()[]{}<>\"'`")
		if token == "" {
			continue
		}
		if looksLikeHumanReviewHash(token) || looksLikeHumanReviewFile(token) {
			return true
		}
	}
	return false
}

func looksLikeHumanReviewHash(token string) bool {
	if len(token) < 7 || len(token) > 64 {
		return false
	}
	hasHexLetter := false
	for _, r := range token {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
			hasHexLetter = true
		case r >= 'A' && r <= 'F':
			hasHexLetter = true
		default:
			return false
		}
	}
	// Pure decimal dates/counts are not hashes. Hex strings containing a-f
	// (including all-letter hashes such as deadbeef) are rejected.
	return hasHexLetter
}

func looksLikeHumanReviewFile(token string) bool {
	dot := strings.LastIndexByte(token, '.')
	if dot < 1 || dot == len(token)-1 {
		return false
	}
	extension := token[dot+1:]
	if len(extension) < 2 || len(extension) > 12 {
		return false
	}
	hasLetter := false
	for _, r := range extension {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			hasLetter = true
		case r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return hasLetter
}
