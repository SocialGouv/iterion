package openai

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/SocialGouv/claw-code-go/internal/api"
)

const (
	defaultImageModel     = "gpt-image-2"
	maxImageResponseBytes = 48 * 1024 * 1024
	maxImageBytes         = 32 * 1024 * 1024
	imageRequestTimeout   = 5 * time.Minute
)

func newImageHTTPClient() *http.Client {
	client := api.NewStreamingHTTPClient()
	client.Transport.(*http.Transport).ResponseHeaderTimeout = imageRequestTimeout
	return client
}

// SupportsImageGeneration reports whether this client targets an OpenAI image
// endpoint known to be available. Compatible custom API endpoints may only
// implement text APIs, so the conversation does not advertise image_gen there.
func (c *Client) SupportsImageGeneration() bool {
	return c.AuthMode == AuthModeChatGPTOAuth || strings.TrimRight(c.BaseURL, "/") == defaultBaseURL
}

// GenerateImage uses the same authenticated provider as the text conversation.
// Codex's image API is a separate endpoint, not a /responses output item.
func (c *Client) GenerateImage(ctx context.Context, prompt string) (api.GeneratedImage, error) {
	if strings.TrimSpace(prompt) == "" {
		return api.GeneratedImage{}, fmt.Errorf("image_gen: prompt is required")
	}
	endpoint := strings.TrimRight(c.BaseURL, "/") + "/v1/images/generations"
	if c.AuthMode == AuthModeChatGPTOAuth {
		endpoint = strings.TrimRight(c.BaseURL, "/") + "/images/generations"
	}
	body, err := json.Marshal(struct {
		Prompt     string `json:"prompt"`
		Model      string `json:"model"`
		Background string `json:"background"`
		Quality    string `json:"quality"`
		Size       string `json:"size"`
	}{prompt, defaultImageModel, "auto", "auto", "auto"})
	if err != nil {
		return api.GeneratedImage{}, fmt.Errorf("image_gen: encode request: %w", err)
	}
	requestCtx, cancel := context.WithTimeout(ctx, imageRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return api.GeneratedImage{}, fmt.Errorf("image_gen: create request: %w", err)
	}
	if err := c.setAuthHeaders(req); err != nil {
		return api.GeneratedImage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.AuthMode == AuthModeChatGPTOAuth {
		turnID, err := imageTurnID()
		if err != nil {
			return api.GeneratedImage{}, fmt.Errorf("image_gen: create turn ID: %w", err)
		}
		req.Header.Set("x-codex-image-turn-id", turnID)
	}
	c.Identity.Apply(req.Header)
	imageClient := c.ImageHTTPClient
	if imageClient == nil {
		imageClient = c.HTTPClient
	}
	resp, err := imageClient.Do(req)
	if err != nil {
		return api.GeneratedImage{}, fmt.Errorf("image_gen: request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		message := fmt.Sprintf("image_gen: provider rejected request (HTTP %d)", resp.StatusCode)
		if resp.StatusCode == http.StatusUnauthorized && c.AuthMode == AuthModeChatGPTOAuth {
			message += "; open Codex to refresh your login, then retry"
		}
		return api.GeneratedImage{}, &api.APIError{
			Provider: "openai", StatusCode: resp.StatusCode, Message: message,
			Retryable: api.IsRetryableStatus(resp.StatusCode),
		}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageResponseBytes+1))
	if err != nil {
		return api.GeneratedImage{}, fmt.Errorf("image_gen: read response: %w", err)
	}
	if len(data) > maxImageResponseBytes {
		return api.GeneratedImage{}, fmt.Errorf("image_gen: response exceeds size limit")
	}
	var result struct {
		Data []struct {
			Base64        string `json:"b64_json"`
			RevisedPrompt string `json:"revised_prompt"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &result); err != nil || len(result.Data) == 0 || result.Data[0].Base64 == "" {
		return api.GeneratedImage{}, fmt.Errorf("image_gen: malformed image response")
	}
	if base64.StdEncoding.DecodedLen(len(result.Data[0].Base64)) > maxImageBytes {
		return api.GeneratedImage{}, fmt.Errorf("image_gen: generated image exceeds size limit")
	}
	imageData, err := base64.StdEncoding.DecodeString(result.Data[0].Base64)
	if err != nil || len(imageData) == 0 {
		return api.GeneratedImage{}, fmt.Errorf("image_gen: invalid image encoding")
	}
	return api.GeneratedImage{Data: imageData, RevisedPrompt: result.Data[0].RevisedPrompt}, nil
}

func imageTurnID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s", hex.EncodeToString(id[:4]),
		hex.EncodeToString(id[4:6]), hex.EncodeToString(id[6:8]),
		hex.EncodeToString(id[8:10]), hex.EncodeToString(id[10:])), nil
}
