package tools

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"strings"

	"github.com/SocialGouv/claw-code-go/internal/api"
)

const maxGeneratedImageBytes = 32 * 1024 * 1024
const maxGeneratedImageDimension = 4096

// ImageGenTool is offered only when the active provider implements image
// generation. The tool saves one PNG and returns its absolute path.
func ImageGenTool() api.Tool {
	return api.Tool{
		Name: "image_gen",
		Description: "Generate a new PNG image from a prompt and save it to a private local file. " +
			"Returns the absolute file path. Use this for user requests to create an image.",
		InputSchema: api.InputSchema{
			Type: "object",
			Properties: map[string]api.Property{
				"prompt": {Type: "string", Description: "Description of the image to create."},
			},
			Required: []string{"prompt"},
		},
	}
}

// ExecuteImageGen validates the returned PNG before writing it with an
// exclusive, unpredictable filename. The provider never chooses a path.
func ExecuteImageGen(ctx context.Context, input map[string]any, generator api.ImageGenerator, outputDir string) (string, error) {
	prompt, _ := input["prompt"].(string)
	if strings.TrimSpace(prompt) == "" {
		return "", fmt.Errorf("image_gen: prompt is required")
	}
	if generator == nil {
		return "", fmt.Errorf("image_gen: active provider does not support image generation")
	}
	generated, err := generator.GenerateImage(ctx, prompt)
	if err != nil {
		return "", err
	}
	if len(generated.Data) == 0 || len(generated.Data) > maxGeneratedImageBytes {
		return "", fmt.Errorf("image_gen: invalid image size")
	}
	info, err := png.DecodeConfig(bytes.NewReader(generated.Data))
	if err != nil || info.Width < 1 || info.Height < 1 ||
		info.Width > maxGeneratedImageDimension || info.Height > maxGeneratedImageDimension {
		return "", fmt.Errorf("image_gen: provider returned an invalid or oversized PNG")
	}
	if _, err := png.Decode(bytes.NewReader(generated.Data)); err != nil {
		return "", fmt.Errorf("image_gen: provider returned a corrupt PNG")
	}
	if outputDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("image_gen: cannot locate image output directory")
		}
		outputDir = filepath.Join(home, ".claw-code", "generated_images")
	}
	outputDir, err = filepath.Abs(outputDir)
	if err != nil {
		return "", fmt.Errorf("image_gen: invalid image output directory")
	}
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		return "", fmt.Errorf("image_gen: create image output directory: %w", err)
	}
	var nameBytes [16]byte
	if _, err := rand.Read(nameBytes[:]); err != nil {
		return "", fmt.Errorf("image_gen: create filename: %w", err)
	}
	path := filepath.Join(outputDir, "image-"+hex.EncodeToString(nameBytes[:])+".png")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("image_gen: create image file: %w", err)
	}
	if _, err := f.Write(generated.Data); err != nil {
		f.Close()
		os.Remove(path)
		return "", fmt.Errorf("image_gen: write image file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("image_gen: close image file: %w", err)
	}
	return fmt.Sprintf("Generated PNG saved to %s (%d x %d)", path, info.Width, info.Height), nil
}
