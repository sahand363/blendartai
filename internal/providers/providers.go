// Package providers talks to the AI services over plain HTTPS:
// OpenAI, Gemini, Anthropic, and custom OpenAI-compatible APIs.
//
// Each provider can list its models, describe images (vision chat), and,
// except Anthropic, generate an image from text.
package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/sahand363/blendartai/internal/imageprep"
)

const (
	KindOpenAI    = "openai"
	KindGemini    = "gemini"
	KindAnthropic = "anthropic"
	KindCustom    = "custom" // OpenAI-compatible
)

var Kinds = []string{KindOpenAI, KindGemini, KindAnthropic, KindCustom}

// DefaultBaseURL returns the official API root for a kind ("" for custom).
func DefaultBaseURL(kind string) string {
	switch kind {
	case KindOpenAI:
		return "https://api.openai.com/v1"
	case KindGemini:
		return "https://generativelanguage.googleapis.com/v1beta"
	case KindAnthropic:
		return "https://api.anthropic.com/v1"
	}
	return ""
}

// CanMakeImages reports whether a kind has an image generation API.
func CanMakeImages(kind string) bool { return kind != KindAnthropic }

// Config is one saved provider.
type Config struct {
	Kind    string
	BaseURL string // "" means the official one
	APIKey  string
}

// APIError is a non-2xx answer from a provider.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	msg := e.Body
	var parsed struct {
		Error any `json:"error"`
	}
	if json.Unmarshal([]byte(e.Body), &parsed) == nil && parsed.Error != nil {
		switch v := parsed.Error.(type) {
		case string:
			msg = v
		case map[string]any:
			if m, ok := v["message"].(string); ok {
				msg = m
			}
		}
	}
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, strings.TrimSpace(msg))
}

// IsQuota is true for rate limits and exhausted credit (429).
func IsQuota(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status == 429 || strings.Contains(ae.Body, "RESOURCE_EXHAUSTED") || strings.Contains(ae.Body, "insufficient_quota")
	}
	return false
}

// IsRetryable is true for temporary failures (5xx, overloaded, network).
func IsRetryable(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status >= 500 || ae.Status == 408 || ae.Status == 529
	}
	var ne interface{ Timeout() bool }
	return errors.As(err, &ne) && ne.Timeout()
}

// IsAuth is true when the key was rejected.
func IsAuth(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && (ae.Status == 401 || ae.Status == 403)
}

// Client is a provider ready to use.
type Client struct {
	cfg  Config
	base string
	http *http.Client
}

// New makes a client. proxyURL may be "" or http(s):// or socks5://.
func New(cfg Config, proxyURL string) (*Client, error) {
	if !validKind(cfg.Kind) {
		return nil, fmt.Errorf("unknown provider kind %q", cfg.Kind)
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("API key is empty")
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = DefaultBaseURL(cfg.Kind)
	}
	if base == "" {
		return nil, errors.New("custom provider needs a base URL, like https://host/v1")
	}
	if u, err := url.Parse(base); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("bad base URL %q", base)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if proxyURL = strings.TrimSpace(proxyURL); proxyURL != "" {
		pu, err := url.Parse(proxyURL)
		if err != nil || pu.Host == "" {
			return nil, fmt.Errorf("bad proxy URL %q", proxyURL)
		}
		tr.Proxy = http.ProxyURL(pu)
	}
	return &Client{cfg: cfg, base: base, http: &http.Client{Transport: tr, Timeout: 5 * time.Minute}}, nil
}

func validKind(k string) bool {
	for _, v := range Kinds {
		if v == k {
			return true
		}
	}
	return false
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	switch c.cfg.Kind {
	case KindGemini:
		req.Header.Set("x-goog-api-key", c.cfg.APIKey)
	case KindAnthropic:
		req.Header.Set("x-api-key", c.cfg.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	default:
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 100<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return &APIError{Status: resp.StatusCode, Body: string(raw)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("unexpected answer from %s: %w", c.base, err)
	}
	return nil
}

// ListModels checks the key and returns the model IDs, sorted.
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	var ids []string
	switch c.cfg.Kind {
	case KindGemini:
		token := ""
		for page := 0; page < 20; page++ {
			var out struct {
				Models []struct {
					Name    string   `json:"name"`
					Methods []string `json:"supportedGenerationMethods"`
				} `json:"models"`
				Next string `json:"nextPageToken"`
			}
			p := "/models?pageSize=1000"
			if token != "" {
				p += "&pageToken=" + url.QueryEscape(token)
			}
			if err := c.do(ctx, http.MethodGet, p, nil, &out); err != nil {
				return nil, err
			}
			for _, m := range out.Models {
				ok := false
				for _, meth := range m.Methods {
					if meth == "generateContent" || meth == "predict" {
						ok = true
					}
				}
				if ok || len(m.Methods) == 0 {
					ids = append(ids, strings.TrimPrefix(m.Name, "models/"))
				}
			}
			if out.Next == "" {
				break
			}
			token = out.Next
		}
	case KindAnthropic:
		after := ""
		for page := 0; page < 20; page++ {
			var out struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
				HasMore bool   `json:"has_more"`
				LastID  string `json:"last_id"`
			}
			p := "/models?limit=1000"
			if after != "" {
				p += "&after_id=" + url.QueryEscape(after)
			}
			if err := c.do(ctx, http.MethodGet, p, nil, &out); err != nil {
				return nil, err
			}
			for _, m := range out.Data {
				ids = append(ids, m.ID)
			}
			if !out.HasMore || out.LastID == "" {
				break
			}
			after = out.LastID
		}
	default:
		var out struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := c.do(ctx, http.MethodGet, "/models", nil, &out); err != nil {
			return nil, err
		}
		for _, m := range out.Data {
			ids = append(ids, m.ID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// Describe sends a text instruction plus images and returns the reply text.
func (c *Client) Describe(ctx context.Context, model, instruction string, imgs []imageprep.Image) (string, error) {
	var text string
	switch c.cfg.Kind {
	case KindGemini:
		parts := []map[string]any{{"text": instruction}}
		for _, im := range imgs {
			parts = append(parts, map[string]any{"inline_data": map[string]string{
				"mime_type": im.MIMEType, "data": base64.StdEncoding.EncodeToString(im.Data),
			}})
		}
		var out geminiResp
		body := map[string]any{"contents": []map[string]any{{"role": "user", "parts": parts}}}
		if err := c.do(ctx, http.MethodPost, "/models/"+url.PathEscape(model)+":generateContent", body, &out); err != nil {
			return "", err
		}
		for _, cand := range out.Candidates {
			for _, p := range cand.Content.Parts {
				text += p.Text
			}
			if text != "" {
				break
			}
		}
		if text == "" && out.PromptFeedback.BlockReason != "" {
			return "", fmt.Errorf("blocked by Gemini: %s", out.PromptFeedback.BlockReason)
		}
	case KindAnthropic:
		content := []map[string]any{}
		for _, im := range imgs {
			content = append(content, map[string]any{"type": "image", "source": map[string]string{
				"type": "base64", "media_type": im.MIMEType, "data": base64.StdEncoding.EncodeToString(im.Data),
			}})
		}
		content = append(content, map[string]any{"type": "text", "text": instruction})
		var out struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		body := map[string]any{"model": model, "max_tokens": 1500, "messages": []map[string]any{{"role": "user", "content": content}}}
		if err := c.do(ctx, http.MethodPost, "/messages", body, &out); err != nil {
			return "", err
		}
		for _, b := range out.Content {
			if b.Type == "text" {
				text += b.Text
			}
		}
	default:
		content := []map[string]any{{"type": "text", "text": instruction}}
		for _, im := range imgs {
			content = append(content, map[string]any{"type": "image_url", "image_url": map[string]string{
				"url": "data:" + im.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(im.Data),
			}})
		}
		var out struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		body := map[string]any{"model": model, "messages": []map[string]any{{"role": "user", "content": content}}}
		if err := c.do(ctx, http.MethodPost, "/chat/completions", body, &out); err != nil {
			return "", err
		}
		if len(out.Choices) > 0 {
			text = out.Choices[0].Message.Content
		}
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("the model returned no text")
	}
	return text, nil
}

type geminiResp struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text       string `json:"text"`
				InlineData *struct {
					MimeType string `json:"mimeType"`
					Data     string `json:"data"`
				} `json:"inlineData"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	PromptFeedback struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
}

// GenerateImage makes one image from a text prompt. size is "1K", "2K",
// "4K" or a "WxH" value; each provider gets the closest form it accepts.
func (c *Client) GenerateImage(ctx context.Context, model, prompt, size string) ([]byte, string, error) {
	switch c.cfg.Kind {
	case KindAnthropic:
		return nil, "", errors.New("Anthropic has no image models, pick an OpenAI, Gemini or custom model for images")
	case KindGemini:
		if strings.HasPrefix(model, "imagen") {
			var out struct {
				Predictions []struct {
					Bytes string `json:"bytesBase64Encoded"`
					Mime  string `json:"mimeType"`
				} `json:"predictions"`
			}
			body := map[string]any{"instances": []map[string]string{{"prompt": prompt}}, "parameters": map[string]any{"sampleCount": 1}}
			if err := c.do(ctx, http.MethodPost, "/models/"+url.PathEscape(model)+":predict", body, &out); err != nil {
				return nil, "", err
			}
			for _, p := range out.Predictions {
				if p.Bytes != "" {
					data, err := base64.StdEncoding.DecodeString(p.Bytes)
					return data, orDefault(p.Mime, "image/png"), err
				}
			}
			return nil, "", errors.New("no image in the answer (it may have been filtered)")
		}
		gen := map[string]any{"responseModalities": []string{"IMAGE"}}
		if gs := geminiSize(size); gs != "" {
			gen["imageConfig"] = map[string]string{"imageSize": gs}
		}
		body := map[string]any{
			"contents":         []map[string]any{{"role": "user", "parts": []map[string]string{{"text": prompt}}}},
			"generationConfig": gen,
		}
		var out geminiResp
		if err := c.do(ctx, http.MethodPost, "/models/"+url.PathEscape(model)+":generateContent", body, &out); err != nil {
			return nil, "", err
		}
		for _, cand := range out.Candidates {
			for _, p := range cand.Content.Parts {
				if p.InlineData != nil && p.InlineData.Data != "" {
					data, err := base64.StdEncoding.DecodeString(p.InlineData.Data)
					return data, orDefault(p.InlineData.MimeType, "image/png"), err
				}
			}
		}
		if out.PromptFeedback.BlockReason != "" {
			return nil, "", fmt.Errorf("blocked by Gemini: %s", out.PromptFeedback.BlockReason)
		}
		return nil, "", errors.New("no image in the answer (it may have been filtered)")
	default:
		body := map[string]any{"model": model, "prompt": prompt, "n": 1, "size": openAISize(size)}
		if strings.HasPrefix(model, "dall-e") {
			body["response_format"] = "b64_json"
		}
		var out struct {
			Data []struct {
				B64 string `json:"b64_json"`
				URL string `json:"url"`
			} `json:"data"`
		}
		if err := c.do(ctx, http.MethodPost, "/images/generations", body, &out); err != nil {
			return nil, "", err
		}
		if len(out.Data) == 0 {
			return nil, "", errors.New("no image in the answer")
		}
		if out.Data[0].B64 != "" {
			data, err := base64.StdEncoding.DecodeString(out.Data[0].B64)
			if err != nil {
				return nil, "", err
			}
			return data, http.DetectContentType(data), nil
		}
		if out.Data[0].URL != "" {
			return c.download(ctx, out.Data[0].URL)
		}
		return nil, "", errors.New("no image in the answer")
	}
}

func (c *Client) download(ctx context.Context, u string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 100<<20))
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode/100 != 2 {
		return nil, "", &APIError{Status: resp.StatusCode, Body: "image download failed"}
	}
	return data, http.DetectContentType(data), nil
}

func geminiSize(s string) string {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "1K", "2K", "4K":
		return strings.ToUpper(strings.TrimSpace(s))
	}
	return ""
}

func openAISize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if strings.Contains(s, "x") {
		return s
	}
	switch s {
	case "2k":
		return "1536x1024"
	case "4k":
		return "1792x1024"
	}
	return "1024x1024"
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// PingResult is what a model test found.
type PingResult struct {
	// State is "ok" (the model answered), "warn" (it exists but would not
	// chat, which is normal for image-only models) or "fail".
	State     string `json:"state"`
	Message   string `json:"message"`
	LatencyMS int64  `json:"latency_ms"`
}

// LooksImageOnly guesses from the name whether a model only draws.
func LooksImageOnly(model string) bool {
	m := strings.ToLower(model)
	return strings.HasPrefix(m, "imagen") || strings.HasPrefix(m, "dall-e") || strings.HasPrefix(m, "gpt-image") ||
		strings.Contains(m, "flux") || strings.Contains(m, "stable-diffusion") || strings.Contains(m, "sdxl")
}

// Ping checks that a model answers, as cheaply as possible: a tiny text
// request for chat models, and a model lookup for image-only models (so a
// test never pays for a picture).
func (c *Client) Ping(ctx context.Context, model string) PingResult {
	start := time.Now()
	since := func() int64 { return time.Since(start).Milliseconds() }
	if LooksImageOnly(model) && c.cfg.Kind != KindAnthropic {
		if err := c.lookup(ctx, model); err != nil {
			return PingResult{State: "fail", Message: err.Error(), LatencyMS: since()}
		}
		return PingResult{State: "ok", Message: "image model found (use Test draw to make a real picture)", LatencyMS: since()}
	}
	text, err := c.chatPing(ctx, model)
	if err == nil {
		if len(text) > 60 {
			text = text[:60] + "…"
		}
		return PingResult{State: "ok", Message: "answered: " + text, LatencyMS: since()}
	}
	if IsAuth(err) || IsQuota(err) || c.cfg.Kind == KindAnthropic {
		return PingResult{State: "fail", Message: err.Error(), LatencyMS: since()}
	}
	// A chat failure on a model that exists usually means it only draws or
	// embeds; say so instead of calling it broken.
	if lerr := c.lookup(ctx, model); lerr == nil {
		return PingResult{State: "warn", Message: "model exists but won't chat (" + err.Error() + ")", LatencyMS: since()}
	}
	return PingResult{State: "fail", Message: err.Error(), LatencyMS: since()}
}

func (c *Client) chatPing(ctx context.Context, model string) (string, error) {
	const q = "Reply with the single word OK."
	var text string
	switch c.cfg.Kind {
	case KindGemini:
		var out geminiResp
		body := map[string]any{"contents": []map[string]any{{"role": "user", "parts": []map[string]string{{"text": q}}}}}
		if err := c.do(ctx, http.MethodPost, "/models/"+url.PathEscape(model)+":generateContent", body, &out); err != nil {
			return "", err
		}
		for _, cand := range out.Candidates {
			for _, p := range cand.Content.Parts {
				text += p.Text
			}
		}
	case KindAnthropic:
		var out struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		body := map[string]any{"model": model, "max_tokens": 16, "messages": []map[string]any{{"role": "user", "content": q}}}
		if err := c.do(ctx, http.MethodPost, "/messages", body, &out); err != nil {
			return "", err
		}
		for _, b := range out.Content {
			text += b.Text
		}
	default:
		var out struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		body := map[string]any{"model": model, "messages": []map[string]any{{"role": "user", "content": q}}}
		if err := c.do(ctx, http.MethodPost, "/chat/completions", body, &out); err != nil {
			return "", err
		}
		if len(out.Choices) > 0 {
			text = out.Choices[0].Message.Content
		}
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("connected, but the model sent no text")
	}
	return text, nil
}

// lookup asks the provider about one model by name.
func (c *Client) lookup(ctx context.Context, model string) error {
	return c.do(ctx, http.MethodGet, "/models/"+url.PathEscape(model), nil, nil)
}
