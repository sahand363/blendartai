package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sahand363/blendartai/internal/imageprep"
)

var png = []byte("\x89PNG\r\n\x1a\nimg")

func fake(t *testing.T, check func(r *http.Request, body map[string]any) any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		out := check(r, body)
		if e, ok := out.(*APIError); ok {
			w.WriteHeader(e.Status)
			io.WriteString(w, e.Body)
			return
		}
		json.NewEncoder(w).Encode(out)
	}))
}

var imgs = []imageprep.Image{{Data: []byte("a"), MIMEType: "image/png"}, {Data: []byte("b"), MIMEType: "image/jpeg"}}

func TestOpenAICompatible(t *testing.T) {
	srv := fake(t, func(r *http.Request, b map[string]any) any {
		if r.Header.Get("Authorization") != "Bearer k1" {
			return &APIError{Status: 401, Body: `{"error":{"message":"bad key"}}`}
		}
		switch r.URL.Path {
		case "/v1/models":
			return map[string]any{"data": []map[string]string{{"id": "z-model"}, {"id": "a-model"}}}
		case "/v1/chat/completions":
			msg := b["messages"].([]any)[0].(map[string]any)["content"].([]any)
			if len(msg) != 3 || !strings.HasPrefix(msg[1].(map[string]any)["image_url"].(map[string]any)["url"].(string), "data:image/png;base64,") {
				return &APIError{Status: 400, Body: "bad content"}
			}
			return map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": " desc "}}}}
		case "/v1/images/generations":
			if b["size"] != "1024x1024" {
				return &APIError{Status: 400, Body: "size"}
			}
			return map[string]any{"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(png)}}}
		}
		return &APIError{Status: 404, Body: "nf"}
	})
	defer srv.Close()
	c, err := New(Config{Kind: KindCustom, BaseURL: srv.URL + "/v1/", APIKey: "k1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ms, err := c.ListModels(ctx)
	if err != nil || len(ms) != 2 || ms[0] != "a-model" {
		t.Fatalf("models %v %v", ms, err)
	}
	if d, err := c.Describe(ctx, "m", "do it", imgs); err != nil || d != "desc" {
		t.Fatalf("describe %q %v", d, err)
	}
	data, mime, err := c.GenerateImage(ctx, "gpt-image-1", "p", "1K")
	if err != nil || mime != "image/png" || string(data) != string(png) {
		t.Fatalf("image %q %v", mime, err)
	}
	bad, _ := New(Config{Kind: KindCustom, BaseURL: srv.URL + "/v1", APIKey: "nope"}, "")
	_, err = bad.ListModels(ctx)
	if !IsAuth(err) || !strings.Contains(err.Error(), "bad key") {
		t.Fatalf("want auth error, got %v", err)
	}
}

func TestGemini(t *testing.T) {
	srv := fake(t, func(r *http.Request, b map[string]any) any {
		if r.Header.Get("x-goog-api-key") != "g" {
			return &APIError{Status: 403, Body: "no"}
		}
		switch {
		case r.URL.Path == "/models":
			if r.URL.Query().Get("pageToken") == "" {
				return map[string]any{"models": []map[string]any{{"name": "models/gemini-x", "supportedGenerationMethods": []string{"generateContent"}}, {"name": "models/embed", "supportedGenerationMethods": []string{"embedContent"}}}, "nextPageToken": "p2"}
			}
			return map[string]any{"models": []map[string]any{{"name": "models/imagen-4", "supportedGenerationMethods": []string{"predict"}}}}
		case r.URL.Path == "/models/gemini-x:generateContent":
			if gc, ok := b["generationConfig"].(map[string]any); ok {
				if gc["imageConfig"].(map[string]any)["imageSize"] != "2K" {
					return &APIError{Status: 400, Body: "size"}
				}
				return map[string]any{"candidates": []map[string]any{{"content": map[string]any{"parts": []map[string]any{{"text": "here"}, {"inlineData": map[string]string{"mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(png)}}}}}}}
			}
			parts := b["contents"].([]any)[0].(map[string]any)["parts"].([]any)
			if len(parts) != 3 {
				return &APIError{Status: 400, Body: "parts"}
			}
			return map[string]any{"candidates": []map[string]any{{"content": map[string]any{"parts": []map[string]string{{"text": "styled"}}}}}}
		case r.URL.Path == "/models/imagen-4:predict":
			return map[string]any{"predictions": []map[string]string{{"bytesBase64Encoded": base64.StdEncoding.EncodeToString(png), "mimeType": "image/png"}}}
		}
		return &APIError{Status: 429, Body: "RESOURCE_EXHAUSTED"}
	})
	defer srv.Close()
	c, _ := New(Config{Kind: KindGemini, BaseURL: srv.URL, APIKey: "g"}, "")
	ctx := context.Background()
	ms, err := c.ListModels(ctx)
	if err != nil || strings.Join(ms, ",") != "gemini-x,imagen-4" {
		t.Fatalf("models %v %v", ms, err)
	}
	if d, err := c.Describe(ctx, "gemini-x", "i", imgs); err != nil || d != "styled" {
		t.Fatalf("describe %q %v", d, err)
	}
	if data, _, err := c.GenerateImage(ctx, "gemini-x", "p", "2K"); err != nil || string(data) != string(png) {
		t.Fatalf("image %v", err)
	}
	if data, _, err := c.GenerateImage(ctx, "imagen-4", "p", "1K"); err != nil || string(data) != string(png) {
		t.Fatalf("imagen %v", err)
	}
	if _, _, err := c.GenerateImage(ctx, "other", "p", "1K"); !IsQuota(err) {
		t.Fatalf("want quota, got %v", err)
	}
}

func TestAnthropic(t *testing.T) {
	srv := fake(t, func(r *http.Request, b map[string]any) any {
		if r.Header.Get("x-api-key") != "a" || r.Header.Get("anthropic-version") == "" {
			return &APIError{Status: 401, Body: "no"}
		}
		if r.URL.Path == "/models" {
			return map[string]any{"data": []map[string]string{{"id": "claude-x"}}, "has_more": false}
		}
		content := b["messages"].([]any)[0].(map[string]any)["content"].([]any)
		if len(content) != 3 || content[0].(map[string]any)["type"] != "image" {
			return &APIError{Status: 400, Body: "content"}
		}
		return map[string]any{"content": []map[string]string{{"type": "text", "text": "ok"}}}
	})
	defer srv.Close()
	c, _ := New(Config{Kind: KindAnthropic, BaseURL: srv.URL, APIKey: "a"}, "")
	ctx := context.Background()
	if ms, err := c.ListModels(ctx); err != nil || len(ms) != 1 {
		t.Fatalf("%v %v", ms, err)
	}
	if d, err := c.Describe(ctx, "claude-x", "i", imgs); err != nil || d != "ok" {
		t.Fatalf("%q %v", d, err)
	}
	if _, _, err := c.GenerateImage(ctx, "claude-x", "p", "1K"); err == nil {
		t.Fatal("anthropic should not make images")
	}
}

func TestNewValidates(t *testing.T) {
	if _, err := New(Config{Kind: KindCustom, APIKey: "k"}, ""); err == nil {
		t.Fatal("custom without URL should fail")
	}
	if _, err := New(Config{Kind: KindOpenAI}, ""); err == nil {
		t.Fatal("empty key should fail")
	}
	if _, err := New(Config{Kind: KindOpenAI, APIKey: "k"}, "socks5://127.0.0.1:1080"); err != nil {
		t.Fatal(err)
	}
}

func TestPing(t *testing.T) {
	srv := fake(t, func(r *http.Request, b map[string]any) any {
		switch {
		case r.URL.Path == "/v1/chat/completions" && b["model"] == "chatty":
			return map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": "OK"}}}}
		case r.URL.Path == "/v1/chat/completions":
			return &APIError{Status: 400, Body: `{"error":{"message":"not a chat model"}}`}
		case r.URL.Path == "/v1/models/gpt-image-1", r.URL.Path == "/v1/models/embedder":
			return map[string]string{"id": "x"}
		}
		return &APIError{Status: 404, Body: "nf"}
	})
	defer srv.Close()
	c, _ := New(Config{Kind: KindOpenAI, BaseURL: srv.URL + "/v1", APIKey: "k"}, "")
	ctx := context.Background()
	if r := c.Ping(ctx, "chatty"); r.State != "ok" || !strings.Contains(r.Message, "OK") {
		t.Fatalf("chatty: %+v", r)
	}
	if r := c.Ping(ctx, "gpt-image-1"); r.State != "ok" {
		t.Fatalf("image model: %+v", r)
	}
	if r := c.Ping(ctx, "embedder"); r.State != "warn" {
		t.Fatalf("embedder: %+v", r)
	}
	if r := c.Ping(ctx, "ghost"); r.State != "fail" {
		t.Fatalf("ghost: %+v", r)
	}
}
