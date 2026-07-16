package main

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"time"

	codexsdk "github.com/ethpandaops/codex-agent-sdk-go"
)

func main() {
	paths, cleanup, err := inputPaths(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "prepare input paths: %v\n", err)
		os.Exit(1)
	}

	if cleanup != nil {
		defer cleanup()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	runQuery(ctx, "Path mentions", buildPathContent(paths))

	if imageContent, ok, err := buildImageContent(paths); err != nil {
		fmt.Fprintf(os.Stderr, "build image content: %v\n", err)
		os.Exit(1)
	} else if ok {
		runQuery(ctx, "Inline image block", imageContent)
	}
}

func buildPathContent(paths []string) codexsdk.UserMessageContent {
	blocks := make([]codexsdk.ContentBlock, 0, len(paths)+1)
	blocks = append(blocks, codexsdk.TextInput("Summarize these local inputs briefly."))

	for _, path := range paths {
		blocks = append(blocks, codexsdk.PathInput(path))
	}

	return codexsdk.Blocks(blocks...)
}

func buildImageContent(paths []string) (codexsdk.UserMessageContent, bool, error) {
	blocks := []codexsdk.ContentBlock{
		codexsdk.TextInput("Describe the attached image in one sentence."),
	}

	for _, path := range paths {
		switch strings.ToLower(filepath.Ext(path)) {
		case ".png", ".jpg", ".jpeg", ".gif", ".webp":
			block, err := codexsdk.ImageFileInput(path)
			if err != nil {
				return codexsdk.UserMessageContent{}, false, err
			}

			blocks = append(blocks, block)

			return codexsdk.Blocks(blocks...), true, nil
		}
	}

	return codexsdk.UserMessageContent{}, false, nil
}

func inputPaths(args []string) ([]string, func(), error) {
	if len(args) > 0 {
		return args, nil, nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return nil, nil, err
	}

	dir, err := os.MkdirTemp(cwd, ".codexsdk-multimodal-example-*")
	if err != nil {
		return nil, nil, err
	}

	cleanup := func() {
		_ = os.RemoveAll(dir)
	}

	textPath := filepath.Join(dir, "notes.txt")

	writeErr := os.WriteFile(textPath, []byte("These notes describe a tiny red square image."), 0o600)
	if writeErr != nil {
		cleanup()

		return nil, nil, writeErr
	}

	imagePath := filepath.Join(dir, "red-dot.png")

	imageFile, err := os.Create(imagePath)
	if err != nil {
		cleanup()

		return nil, nil, err
	}

	img := image.NewRGBA(image.Rect(0, 0, 8, 8))

	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}

	if err := png.Encode(imageFile, img); err != nil {
		_ = imageFile.Close()

		cleanup()

		return nil, nil, err
	}

	if err := imageFile.Close(); err != nil {
		cleanup()

		return nil, nil, err
	}

	return []string{textPath, imagePath}, cleanup, nil
}

func runQuery(ctx context.Context, title string, content codexsdk.UserMessageContent) {
	fmt.Printf("== %s ==\n", title)

	for msg, err := range codexsdk.Query(ctx, content, codexsdk.WithPermissionMode("bypassPermissions")) {
		if err != nil {
			fmt.Fprintf(os.Stderr, "query error: %v\n", err)

			return
		}

		assistant, ok := msg.(*codexsdk.AssistantMessage)
		if !ok {
			continue
		}

		for _, block := range assistant.Content {
			if text, ok := block.(*codexsdk.TextBlock); ok {
				fmt.Println(text.Text)
			}
		}
	}

	fmt.Println()
}
