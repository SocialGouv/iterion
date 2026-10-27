package tokenizer

// Package tokenizer provides BPE tokenization for iterion's thinking-token
// metric.
//
// This directory is a trimmed first-party fork of
// github.com/tiktoken-go/tokenizer v0.8.1 (recorded in README.md): only the
// o200k_base encoding is built — the other encodings, their vocab tables,
// ForModel and the model tables are deleted. Resync only to take upstream
// engine fixes, then re-apply the trim.
//
// Usage Example
//
//	enc, err := tokenizer.Get(tokenizer.O200kBase)
//	if err != nil {
//		panic("oh oh")
//	}
//	ids, _, _ := enc.Encode("supercalifragilistic")
//	_ = ids

import (
	"errors"

	"github.com/tiktoken-go/tokenizer/codec"
)

var ErrEncodingNotSupported = errors.New("encoding not supported")

type Codec interface {
	GetName() string
	Count(string) (int, error)
	Encode(string) ([]uint, []string, error)
	Decode([]uint) (string, error)
}

type Encoding string

const O200kBase Encoding = "o200k_base"

// Get returns a new instance of a Codec implementation based on the specified
// encoding format. The returned Codec instance can be used to encode (tokenize)
// and decode (reassemble) text. If the specified encoding is not supported,
// an error is returned.
func Get(encoding Encoding) (Codec, error) {
	switch encoding {
	case O200kBase:
		return codec.NewO200kBase(), nil
	default:
		return nil, ErrEncodingNotSupported
	}
}
