// Package histograms holds the opinionated default aggregation used across
// the ethPandaOps agent SDKs. We default to base-2 exponential histograms so
// that latency and magnitude histograms have good resolution without manual
// bucket picking.
//
// MaxScale=3 caps the growth factor at 2^(1/8) ≈ 1.0905 per bucket — roughly
// equivalent to the Prometheus native-histogram default of 1.1. MaxSize=160
// caps per-series memory; the SDK downscales if the range would exceed that.
package histograms

import sdkmetric "go.opentelemetry.io/otel/sdk/metric"

const (
	DefaultMaxSize  = 160
	DefaultMaxScale = 3
)

// ExponentialHistogram returns the opinionated default aggregation for
// latency- and magnitude-style histograms.
func ExponentialHistogram() sdkmetric.Aggregation {
	return sdkmetric.AggregationBase2ExponentialHistogram{
		MaxSize:  DefaultMaxSize,
		MaxScale: DefaultMaxScale,
	}
}

// AggregationSelector returns ExponentialHistogram for Histogram instruments
// and falls back to the OTel defaults for other instrument kinds. Plug into
// the Prometheus exporter via otelprom.WithAggregationSelector.
func AggregationSelector(kind sdkmetric.InstrumentKind) sdkmetric.Aggregation {
	switch kind {
	case sdkmetric.InstrumentKindHistogram:
		return ExponentialHistogram()
	default:
		return sdkmetric.DefaultAggregationSelector(kind)
	}
}

// NewAggregation returns a base-2 exponential histogram aggregation with the
// given MaxSize and MaxScale. Use when the package defaults (MaxSize=160,
// MaxScale=3) are a poor fit for a specific deployment; prefer
// ExponentialHistogram otherwise.
func NewAggregation(maxSize, maxScale int32) sdkmetric.Aggregation {
	return sdkmetric.AggregationBase2ExponentialHistogram{
		MaxSize:  maxSize,
		MaxScale: maxScale,
	}
}

// NewSelector returns an AggregationSelector that applies the given aggregation
// to histogram instruments and falls back to OTel defaults for other kinds.
// Pair with NewAggregation to plug custom tuning into promexporter via
// WithAggregationSelector.
func NewSelector(agg sdkmetric.Aggregation) sdkmetric.AggregationSelector {
	return func(kind sdkmetric.InstrumentKind) sdkmetric.Aggregation {
		switch kind {
		case sdkmetric.InstrumentKindHistogram:
			return agg
		default:
			return sdkmetric.DefaultAggregationSelector(kind)
		}
	}
}
