// Package goai provides a unified SDK for interacting with AI language models.
//
// GoAI is a Go port of the Vercel AI SDK, providing a consistent API across
// multiple AI providers (OpenAI, Anthropic, Google, and more).
//
// Core functions:
//   - [GenerateText]: non-streaming text generation
//   - [StreamText]: streaming text generation with multiple consumption modes
//   - [GenerateObject]: structured output with auto-generated JSON Schema
//   - [StreamObject]: streaming structured output with partial object emission
//   - [Embed]: single text embedding
//   - [EmbedMany]: batch text embeddings with auto-chunking
//   - [GenerateImage]: image generation from text prompts
//
// Basic usage:
//
//	result, err := goai.GenerateText(ctx, model, goai.WithPrompt("Hello"))
//
//	stream, err := goai.StreamText(ctx, model, goai.WithPrompt("Hello"))
//	for text := range stream.TextStream() {
//	    fmt.Print(text)
//	}
package goai
