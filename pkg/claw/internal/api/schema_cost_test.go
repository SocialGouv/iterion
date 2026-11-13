package api

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

func nestedSchema(depth int) []byte {
	var b strings.Builder
	b.WriteString(`{"type":"object","properties":{"a":`)
	for i := 0; i < depth; i++ {
		b.WriteString(`{"type":"object","description":"level","properties":{"a":`)
	}
	b.WriteString(`{"type":"string"}`)
	for i := 0; i < depth; i++ {
		b.WriteString(`}}`)
	}
	b.WriteString(`}}`)
	return []byte(b.String())
}

func TestR2ProbeMarshalCost(t *testing.T) {
	for _, depth := range []int{100, 1000, 3000} {
		raw := nestedSchema(depth)
		var s InputSchema
		runtime.GC()
		var m0, m1, m2 runtime.MemStats
		runtime.ReadMemStats(&m0)
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatal(err)
		}
		runtime.GC()
		runtime.ReadMemStats(&m1)
		retained := int64(m1.HeapAlloc) - int64(m0.HeapAlloc)
		start := time.Now()
		body, err := json.Marshal(Tool{Name: "deep", InputSchema: s})
		el := time.Since(start)
		runtime.ReadMemStats(&m2)
		t.Logf("PROBE marshal-cost depth=%d in=%dB retainedAfterDecode=%dB marshal=%v allocDuringMarshal=%dB out=%dB err=%v", depth, len(raw), retained, el, m2.TotalAlloc-m1.TotalAlloc, len(body), err)
		runtime.KeepAlive(s)
	}
}

func TestR2ProbeDecodeCost(t *testing.T) {
	for _, depth := range []int{1000, 3000} {
		raw := nestedSchema(depth)
		var m0, m1 runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m0)
		start := time.Now()
		var s InputSchema
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatal(err)
		}
		el := time.Since(start)
		runtime.ReadMemStats(&m1)
		t.Logf("PROBE decode-cost depth=%d in=%dB decode=%v allocDuringDecode=%dB", depth, len(raw), el, m1.TotalAlloc-m0.TotalAlloc)
		runtime.KeepAlive(s)
	}
}

// Assert allocated bytes, not elapsed time: machine scheduling must not make
// the linearity witness flaky. Tripling depth used to multiply bytes by ~9.
func TestSchemaDecodeAllocationGrowth(t *testing.T) {
	allocated := func(depth int) uint64 {
		raw := nestedSchema(depth)
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		var s InputSchema
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatal(err)
		}
		runtime.ReadMemStats(&after)
		runtime.KeepAlive(s)
		return after.TotalAlloc - before.TotalAlloc
	}
	small, large := allocated(300), allocated(900)
	if large > 5*small {
		t.Fatalf("decode allocations grow superlinearly: %d -> %d bytes", small, large)
	}
}

func r4benchSchema(n int, changed bool) []byte {
	var b strings.Builder
	b.WriteString(`{"type":"object","properties":{`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"field_%d":{"type":"string","description":"Field number %d with a reasonably long human description.","enum":["alpha","beta","gamma","delta","epsilon","zeta","eta","theta","iota","kappa"],"minLength":1}`, i, i)
	}
	b.WriteString(`},"required":["field_0"],"additionalProperties":false`)
	if changed {
		b.WriteString(`,"$schema":"http://json-schema.org/draft-07/schema#"`)
	}
	b.WriteString(`}`)
	return []byte(b.String())
}

func BenchmarkR4DecodeMarshal(b *testing.B) {
	for _, n := range []int{20, 5000} {
		for _, changed := range []bool{false, true} {
			raw := r4benchSchema(n, changed)
			b.Run(fmt.Sprintf("props=%d/bytes=%d/changed=%v", n, len(raw), changed), func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(raw)))
				for i := 0; i < b.N; i++ {
					var s InputSchema
					if err := json.Unmarshal(raw, &s); err != nil {
						b.Fatal(err)
					}
					if _, err := s.MarshalJSON(); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// The unchanged 4 KiB tool needs fewer than 1000 allocations with Decode.
// Leave headroom while catching eager token decoding (~3300 allocations).
func TestSchemaUnchangedDecodeAllocations(t *testing.T) {
	raw := r4benchSchema(20, false)
	allocs := testing.AllocsPerRun(20, func() {
		var s InputSchema
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatal(err)
		}
		if _, err := s.MarshalJSON(); err != nil {
			t.Fatal(err)
		}
	})
	if allocs > 1500 {
		t.Fatalf("unchanged schema allocation budget exceeded: %.0f > 1500", allocs)
	}
}
