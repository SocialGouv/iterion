package sandbox

import (
	"errors"
	"fmt"
	"math/big"
	"os"
	"strconv"
	"strings"
)

// ReadBackCapBytes is the default figure one declared read-back dir's set
// is metered against at the drain (ADR-143: the cap is named and metered,
// never "whatever the container wrote"). The meter is PER DECLARED DIR —
// each entry of the drain manifest carries its own figure
// ([ReadBackDir.CapBytes], resolved by [ReadBackCapFor]) — and it lives on
// the host, in the drain: the member that takes a dir's set past its cap
// is dropped and reported, what landed before it stays, and the pod and
// the rest of the run go on. The set is what the host holds — every
// attempt of a retried drain counted together, since tar never deletes
// and a drain no attempt settles keeps them all — never one archive
// alone. The pod side carries NO sizeLimit for it: a
// kubelet evicts the WHOLE pod when an emptyDir's usage passes its
// sizeLimit, which would end the run mid-flight — no drain, no workspace
// export — where the contract promises a dropped member. Same class as
// the scratch bank's cap (ADR-106): how big one run's by-product tree may
// be. The escape hatches: the serving mount's own `size=` and
// [ReadBackCapEnv].
const ReadBackCapBytes int64 = 256 << 20

// ReadBackCapEnv is the deployment-wide override of [ReadBackCapBytes]: a
// byte quantity in the Kubernetes grammar (`1Gi`, `500M`, `1073741824`).
// Read by [ReadBackCapFor] for every declared dir whose serving mount names
// no size of its own. An unreadable value is reported and the default kept
// — the cap is not the place to fail a run on a knob.
const ReadBackCapEnv = "ITERION_SANDBOX_READBACK_CAP"

// ReadBackCapFor resolves the figure one declared read-back dir is metered
// against, from the mount that serves it: the mount's own `size=` wins (a
// bot that serves the path with its own `type=emptydir,...,size=2Gi` chose
// the figure explicitly, and the meter never cuts an explicit choice down
// to the default), then the [ReadBackCapEnv] override, then
// [ReadBackCapBytes]. A value that is no byte quantity is reported through
// warn (nil is accepted) and the next rung applies.
func ReadBackCapFor(servingMount string, warn func(format string, args ...any)) int64 {
	if warn == nil {
		warn = func(string, ...any) {}
	}
	if size := MountSizeOf(servingMount); size != "" {
		n, err := ParseByteQuantity(size)
		if err == nil {
			return n
		}
		warn("sandbox: the mount serving a read-back path names size=%q, which is no byte quantity (%v) — the drain meters it on %s or the %s default instead", size, err, ReadBackCapEnv, ReadBackCapText(ReadBackCapBytes))
	}
	if raw := strings.TrimSpace(os.Getenv(ReadBackCapEnv)); raw != "" {
		n, err := ParseByteQuantity(raw)
		if err == nil {
			return n
		}
		warn("sandbox: %s=%q is no byte quantity (%v) — keeping the %s default", ReadBackCapEnv, raw, err, ReadBackCapText(ReadBackCapBytes))
	}
	return ReadBackCapBytes
}

// ReadBackCapText renders a cap for a message: whole MiB when it is one,
// bytes otherwise, so a 1 KiB witness cap does not read as "0 MiB".
func ReadBackCapText(capBytes int64) string {
	if capBytes > 0 && capBytes%(1<<20) == 0 {
		return fmt.Sprintf("%d MiB", capBytes>>20)
	}
	return fmt.Sprintf("%d bytes", capBytes)
}

// MountSizeOf extracts a mount string's `size=` field — the emptyDir
// sizeLimit the kubernetes driver renders — "" when it names none: the
// last such field, the one the driver renders ([MountField]), so the
// figure the drain meters is the volume's own.
func MountSizeOf(m string) string {
	if v, bare, ok := MountField(m, "size"); ok && !bare {
		return v
	}
	return ""
}

// byteQuantityFactors are the suffixes a Kubernetes quantity carries that
// scale to whole bytes: binary (1024^n) and decimal (1000^n). The sub-unit
// suffixes (n, u, m) scale below a byte and are refused as a cap.
var byteQuantityFactors = map[string]*big.Int{
	"":   big.NewInt(1),
	"Ki": big.NewInt(1 << 10),
	"Mi": big.NewInt(1 << 20),
	"Gi": big.NewInt(1 << 30),
	"Ti": big.NewInt(1 << 40),
	"Pi": big.NewInt(1 << 50),
	"Ei": big.NewInt(1 << 60),
	"k":  big.NewInt(1e3),
	"M":  big.NewInt(1e6),
	"G":  big.NewInt(1e9),
	"T":  big.NewInt(1e12),
	"P":  big.NewInt(1e15),
	"E":  big.NewInt(1e18),
}

// maxByteQuantityText bounds the text ParseByteQuantity reads: an int64
// is 19 digits, and the bound leaves room for a fraction, a suffix and an
// exponent while keeping the parse of a value a bot wrote small.
const maxByteQuantityText = 64

// maxByteQuantityExponent bounds a decimal exponent. Within the text
// bound, a figure scaled past it is either far past what an int64 holds
// or far below a byte; and the scale is computed as 10^e, so an exponent
// a bot wrote unbounded (`size=1e999999999`) would hold the run's start
// on a number hundreds of megabytes long.
const maxByteQuantityExponent = 64

// ParseByteQuantity reads a Kubernetes quantity (`256Mi`, `2Gi`, `500M`,
// `1.5Gi`, `1e9`, `1073741824`) as a positive number of whole bytes,
// rounded up — the grammar an emptyDir sizeLimit is written in, so the
// figure a bot wrote on its mount and the figure the drain meters against
// read the same text. Sub-byte suffixes, a sign, a fraction form, a value
// past int64, a text past maxByteQuantityText and an exponent past
// ±maxByteQuantityExponent are refused with the reason.
func ParseByteQuantity(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty")
	}
	if len(s) > maxByteQuantityText {
		return 0, fmt.Errorf("a %d-character value is longer than a byte quantity is written (%d characters at most)", len(s), maxByteQuantityText)
	}
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	num, suffix := s[:i], s[i:]
	if num == "" || num == "." || strings.Count(num, ".") > 1 {
		return 0, fmt.Errorf("%q is not a number followed by a quantity suffix", s)
	}
	if strings.HasPrefix(num, ".") {
		num = "0" + num
	}
	if strings.HasSuffix(num, ".") {
		num += "0"
	}
	factor, known := byteQuantityFactors[suffix]
	exponent := 0
	switch {
	case known:
	case suffix == "n" || suffix == "u" || suffix == "m":
		return 0, fmt.Errorf("%q scales below a byte (suffix %s)", s, suffix)
	case suffix[0] == 'e' || suffix[0] == 'E':
		e, err := strconv.Atoi(suffix[1:])
		if err != nil {
			return 0, fmt.Errorf("%q carries no quantity suffix and no exponent", s)
		}
		if e > maxByteQuantityExponent || e < -maxByteQuantityExponent {
			return 0, fmt.Errorf("%q carries the exponent %d, past the ±%d a byte quantity is written with", s, e, maxByteQuantityExponent)
		}
		factor, exponent = big.NewInt(1), e
	default:
		return 0, fmt.Errorf("%q carries no quantity suffix (Ki..Ei, k..E, or an exponent)", s)
	}
	value, ok := new(big.Rat).SetString(num)
	if !ok {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	value.Mul(value, new(big.Rat).SetInt(factor))
	if exponent != 0 {
		scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(abs(exponent))), nil)
		if exponent > 0 {
			value.Mul(value, new(big.Rat).SetInt(scale))
		} else {
			value.Quo(value, new(big.Rat).SetInt(scale))
		}
	}
	// Whole bytes, rounded up: a cap of 1.5 bytes admits 2.
	whole := new(big.Int).Quo(value.Num(), value.Denom())
	if new(big.Int).Mul(whole, value.Denom()).Cmp(value.Num()) != 0 {
		whole.Add(whole, big.NewInt(1))
	}
	if whole.Sign() <= 0 {
		return 0, fmt.Errorf("%q is not a positive number of bytes", s)
	}
	if !whole.IsInt64() {
		return 0, fmt.Errorf("%q is past the %d bytes a cap may name", s, int64(^uint64(0)>>1))
	}
	return whole.Int64(), nil
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
