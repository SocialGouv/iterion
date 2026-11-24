package runtime

import (
	"context"
	"errors"
	"strconv"

	"github.com/SocialGouv/iterion/pkg/numeric"
)

// exactNativeInt admits an integer without truncating or rounding a decimal.
// Text tolerance is explicit and preserves the historical decimal-int grammar.
func exactNativeInt(ctx context.Context, value any, allowText bool) (int, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	if text, ok := value.(string); ok && allowText {
		n, err := strconv.Atoi(text)
		if err != nil {
			return 0, false, nil
		}
		if err := ctx.Err(); err != nil {
			return 0, false, err
		}
		return n, true, nil
	}
	n, err := numeric.Read(ctx, value)
	if errors.Is(err, numeric.ErrNotNumber) || errors.Is(err, numeric.ErrInvalidNumber) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	i, ok, _ := n.Int64()
	if !ok || int64(int(i)) != i {
		return 0, false, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	return int(i), true, nil
}
