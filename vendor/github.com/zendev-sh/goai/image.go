package goai

import (
	"context"

	"github.com/zendev-sh/goai/provider"
)

// GenerateImage generates images from a text prompt.
func GenerateImage(ctx context.Context, model provider.ImageModel, opts ...ImageOption) (*ImageResult, error) {
	o := imageOptions{
		n: 1,
	}
	for _, opt := range opts {
		opt(&o)
	}

	params := provider.ImageParams{
		Prompt:          o.prompt,
		N:               o.n,
		Size:            o.size,
		AspectRatio:     o.aspectRatio,
		ProviderOptions: o.providerOptions,
	}

	result, err := model.DoGenerate(ctx, params)
	if err != nil {
		return nil, err
	}

	return &ImageResult{
		Images: result.Images,
	}, nil
}

// ImageResult contains the generated images.
type ImageResult struct {
	Images []provider.ImageData
}

// ImageOption configures image generation.
type ImageOption func(*imageOptions)

type imageOptions struct {
	prompt          string
	n               int
	size            string
	aspectRatio     string
	providerOptions map[string]any
}

// WithImagePrompt sets the text prompt for image generation.
func WithImagePrompt(prompt string) ImageOption {
	return func(o *imageOptions) {
		o.prompt = prompt
	}
}

// WithImageCount sets the number of images to generate.
func WithImageCount(n int) ImageOption {
	return func(o *imageOptions) {
		o.n = n
	}
}

// WithImageSize sets the image size (e.g. "1024x1024", "512x512").
func WithImageSize(size string) ImageOption {
	return func(o *imageOptions) {
		o.size = size
	}
}

// WithAspectRatio sets the aspect ratio (e.g. "16:9", "1:1").
func WithAspectRatio(ratio string) ImageOption {
	return func(o *imageOptions) {
		o.aspectRatio = ratio
	}
}

// WithImageProviderOptions sets provider-specific options.
func WithImageProviderOptions(opts map[string]any) ImageOption {
	return func(o *imageOptions) {
		o.providerOptions = opts
	}
}
