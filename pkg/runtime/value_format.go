package runtime

import "github.com/SocialGouv/iterion/pkg/store"

// A format refusal is not a failed execution of the row now at this ID.
// Callers stop without attributing a failure or cleanup to its successor.
func isRunValueFormatRefusal(err error) bool { return store.IsValueFormatRefusal(err) }
