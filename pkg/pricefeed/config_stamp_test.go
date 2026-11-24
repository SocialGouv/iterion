package pricefeed

import (
	"testing"
	"time"
)

// TestNextConfigStamp pins the CAS token's advance on a fixed clock: a
// write in the stored stamp's tick, a clock reading that carries a
// sub-precision remainder, and a clock behind the stored stamp all still
// move the token one tick past it.
func TestNextConfigStamp(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		previous  time.Time
		now       time.Time
		precision time.Duration
		want      time.Time
	}{
		{"same tick ms", base, base, time.Millisecond, base.Add(time.Millisecond)},
		{"same tick ns", base, base, time.Nanosecond, base.Add(time.Nanosecond)},
		{"sub-tick remainder ms", base, base.Add(400 * time.Microsecond), time.Millisecond, base.Add(time.Millisecond)},
		{"clock behind ms", base, base.Add(-time.Hour), time.Millisecond, base.Add(time.Millisecond)},
		{"zero previous ms", time.Time{}, base.Add(300 * time.Microsecond), time.Millisecond, base},
		{"clock ahead ms", base, base.Add(5*time.Millisecond + 7*time.Nanosecond), time.Millisecond, base.Add(5 * time.Millisecond)},
	}
	for _, tc := range cases {
		got := nextConfigStamp(tc.previous, tc.now, tc.precision)
		if !got.Equal(tc.want) {
			t.Errorf("%s: nextConfigStamp(%s, %s, %s) = %s, want %s",
				tc.name, tc.previous.Format(time.RFC3339Nano), tc.now.Format(time.RFC3339Nano), tc.precision,
				got.Format(time.RFC3339Nano), tc.want.Format(time.RFC3339Nano))
		}
		if got.Location() != time.UTC {
			t.Errorf("%s: stamp location = %s, want UTC", tc.name, got.Location())
		}
	}
}
