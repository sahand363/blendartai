// Package config holds the two kinds of Blendartai configuration:
//
//   - Env: how the server listens (port, bind address, SSL). It comes from
//     environment variables, which systemd loads from
//     /etc/blendartai/blendartai.env. The SSH menu edits that file.
//   - Settings: what the mixer does (providers, active models, limits). It lives in
//     <data>/settings.json and is edited from the web panel.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	DefaultDataDir = "/var/lib/blendartai"
	DefaultPort    = 2096
)

// SSL modes.
const (
	SSLOff         = "off"         // plain HTTP
	SSLSelfSigned  = "selfsigned"  // HTTPS with a generated certificate
	SSLLetsEncrypt = "letsencrypt" // HTTPS with a Let's Encrypt certificate for Domain
	SSLFiles       = "files"       // HTTPS with CertFile/KeyFile (e.g. from certbot or x-ui)
	SSLProxy       = "proxy"       // plain HTTP on 127.0.0.1 behind nginx/Caddy/Apache
)

type Env struct {
	Port     int
	Bind     string
	DataDir  string
	BasePath string
	SSLMode  string
	Domain   string
	Email    string
	CertFile string
	KeyFile  string
}

func LoadEnv() (Env, error) {
	e := Env{
		Port:     DefaultPort,
		Bind:     strings.TrimSpace(os.Getenv("BLENDARTAI_BIND")),
		DataDir:  envString("BLENDARTAI_DATA", DefaultDataDir),
		BasePath: NormalizeBasePath(os.Getenv("BLENDARTAI_BASE_PATH")),
		SSLMode:  strings.ToLower(envString("BLENDARTAI_SSL", SSLOff)),
		Domain:   strings.TrimSpace(os.Getenv("BLENDARTAI_DOMAIN")),
		Email:    strings.TrimSpace(os.Getenv("BLENDARTAI_EMAIL")),
		CertFile: strings.TrimSpace(os.Getenv("BLENDARTAI_CERT")),
		KeyFile:  strings.TrimSpace(os.Getenv("BLENDARTAI_KEY")),
	}
	if v := strings.TrimSpace(os.Getenv("BLENDARTAI_PORT")); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil || p < 1 || p > 65535 {
			return Env{}, fmt.Errorf("invalid BLENDARTAI_PORT %q", v)
		}
		e.Port = p
	}
	switch e.SSLMode {
	case SSLOff, SSLSelfSigned, SSLProxy:
	case SSLLetsEncrypt:
		if e.Domain == "" {
			return Env{}, errors.New("BLENDARTAI_SSL=letsencrypt needs BLENDARTAI_DOMAIN")
		}
	case SSLFiles:
		if e.CertFile == "" || e.KeyFile == "" {
			return Env{}, errors.New("BLENDARTAI_SSL=files needs BLENDARTAI_CERT and BLENDARTAI_KEY")
		}
	default:
		return Env{}, fmt.Errorf("unknown BLENDARTAI_SSL %q (off, selfsigned, letsencrypt, files, proxy)", e.SSLMode)
	}
	if e.SSLMode == SSLProxy && e.Bind == "" {
		e.Bind = "127.0.0.1"
	}
	return e, nil
}

func (e Env) Addr() string {
	if strings.Contains(e.Bind, ":") { // IPv6 literal
		return fmt.Sprintf("[%s]:%d", e.Bind, e.Port)
	}
	return fmt.Sprintf("%s:%d", e.Bind, e.Port)
}

func (e Env) TLS() bool {
	switch e.SSLMode {
	case SSLSelfSigned, SSLLetsEncrypt, SSLFiles:
		return true
	}
	return false
}

func (e Env) SourcesDir() string { return filepath.Join(e.DataDir, "sources") }
func (e Env) StylesDir() string  { return filepath.Join(e.DataDir, "styles") }

func NormalizeBasePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return strings.TrimRight(p, "/")
}

func envString(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// Provider is one saved AI account.
type Provider struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"` // openai, gemini, anthropic, custom
	Name      string    `json:"name"`
	BaseURL   string    `json:"base_url,omitempty"`
	APIKey    string    `json:"api_key"`
	Models    []string  `json:"models,omitempty"`
	CheckedAt time.Time `json:"checked_at,omitempty"`
	CheckErr  string    `json:"check_error,omitempty"`
	// Tests holds the last test result per model name.
	Tests map[string]ModelTest `json:"model_tests,omitempty"`
}

// ModelTest is the last result of the Test button for one model.
type ModelTest struct {
	State     string    `json:"state"` // ok, warn, fail
	Message   string    `json:"message"`
	LatencyMS int64     `json:"latency_ms"`
	At        time.Time `json:"at"`
}

// ModelRef points at one model of one provider.
type ModelRef struct {
	ProviderID string `json:"provider_id"`
	Model      string `json:"model"`
}

func (m ModelRef) Set() bool { return m.ProviderID != "" && m.Model != "" }

// DefaultDescribePrompt is Sahand's prompt from the desktop app.
const DefaultDescribePrompt = "Analyze the subject in the first image and the artistic style of the second image. " +
	"Generate a single-paragraph standalone image generation prompt describing the subject from the first image " +
	"reimagined in the exact style, color palette, linework, medium, and atmosphere of the second image.\n" +
	"CRITICAL INSTRUCTIONS:\n" +
	"1. DO NOT refer to 'the first image', 'image 1', or 'the second image' anywhere in your text.\n" +
	"2. Describe the subject directly (e.g., write 'A portrait of a man with...' or 'A figure wearing...').\n" +
	"3. Output ONLY the standalone prompt text."

// DefaultPromptSuffix is added to the end of every description.
const DefaultPromptSuffix = "Centered portrait, fully in-frame subject with ample negative space around all edges, no cropping. Set against a completely solid, pure black background."

// oldDefaultPrompt was the v1.0 default; settings still holding it move to
// the new default.
const oldDefaultPrompt = `You are given two images. The FIRST image is the content: its subject, composition, pose, objects and setting. The SECOND image is the style: its medium, technique, brushwork or rendering, color palette, lighting, texture and mood.

Write ONE detailed image-generation prompt, a single paragraph of 80 to 160 words, that recreates the content of the first image rendered entirely in the visual style of the second image. Describe the style in concrete visual terms. Do not mention "first image", "second image", "reference", "photo of", or any artist names. Output only the prompt text.`

const (
	StyleFromSources = "sources"
	StyleFromStyles  = "styles"
	StyleFromBoth    = "both"
)

// Settings is what the panel edits.
type Settings struct {
	Providers      []Provider `json:"providers"`
	DescribeModel  ModelRef   `json:"describe_model"`
	ImageModel     ModelRef   `json:"image_model"`
	DescribePrompt string     `json:"describe_prompt"`
	// PromptSuffix is appended to every description (also what the image
	// model draws). Empty means nothing is added.
	PromptSuffix string `json:"prompt_suffix"`
	// TextOnly skips drawing: a run only writes descriptions.
	TextOnly bool `json:"text_only"`
	// StyleFrom says where the second (style) image comes from: "sources",
	// "styles" or "both". The first (content) image always comes from Sources.
	StyleFrom string `json:"style_from"`
	// Pacing. StepDelaySec waits between steps. When a provider says "too
	// many requests" / quota, the call waits LimitWaitSec and tries again,
	// up to LimitRetries times, before the run stops.
	StepDelaySec int    `json:"step_delay_sec"`
	LimitWaitSec int    `json:"limit_wait_sec"`
	LimitRetries int    `json:"limit_retries"`
	ImageSize    string `json:"image_size"` // 1K, 2K, 4K or WxH
	TargetImages int    `json:"target_images"`
	// Optional spend guard: PricePerImage > 0 and MaxUSD > 0 stop the run
	// when the estimate reaches MaxUSD.
	PricePerImage float64 `json:"price_per_image"`
	MaxUSD        float64 `json:"max_usd"`
	ProxyURL      string  `json:"proxy_url,omitempty"` // outbound http(s):// or socks5://
}

func DefaultSettings() Settings {
	return Settings{
		Providers:      []Provider{},
		DescribePrompt: DefaultDescribePrompt,
		PromptSuffix:   DefaultPromptSuffix,
		StyleFrom:      StyleFromBoth,
		LimitWaitSec:   60,
		LimitRetries:   3,
		ImageSize:      "1K",
		TargetImages:   100,
	}
}

func (s *Settings) Provider(id string) (Provider, bool) {
	for _, p := range s.Providers {
		if p.ID == id {
			return p, true
		}
	}
	return Provider{}, false
}

func (s *Settings) Validate() error {
	s.ImageSize = strings.TrimSpace(s.ImageSize)
	s.ProxyURL = strings.TrimSpace(s.ProxyURL)
	s.PromptSuffix = strings.TrimSpace(s.PromptSuffix)
	switch s.StyleFrom {
	case StyleFromSources, StyleFromStyles, StyleFromBoth:
	default:
		s.StyleFrom = StyleFromBoth
	}
	if strings.TrimSpace(s.DescribePrompt) == "" || s.DescribePrompt == oldDefaultPrompt {
		s.DescribePrompt = DefaultDescribePrompt
	}
	switch {
	case s.ImageSize == "" || len(s.ImageSize) > 12:
		return errors.New("image size must be 1K, 2K, 4K or like 1024x1024")
	case s.TargetImages < 1 || s.TargetImages > 10000:
		return errors.New("target images must be 1 to 10000")
	case s.PricePerImage < 0 || s.PricePerImage > 10:
		return errors.New("price per image must be between 0 and 10")
	case s.MaxUSD < 0 || s.MaxUSD > 10000:
		return errors.New("max USD must be between 0 and 10000")
	case s.StepDelaySec < 0 || s.StepDelaySec > 3600:
		return errors.New("pause between steps must be 0 to 3600 seconds")
	case s.LimitWaitSec < 5 || s.LimitWaitSec > 3600:
		return errors.New("wait on a rate limit must be 5 to 3600 seconds")
	case s.LimitRetries < 0 || s.LimitRetries > 100:
		return errors.New("rate-limit retries must be 0 to 100")
	case len(s.PromptSuffix) > 2000:
		return errors.New("text added to every description is too long")
	case len(s.DescribePrompt) > 8000:
		return errors.New("describe prompt is too long")
	}
	if s.ProxyURL != "" && !(strings.HasPrefix(s.ProxyURL, "http://") || strings.HasPrefix(s.ProxyURL, "https://") || strings.HasPrefix(s.ProxyURL, "socks5://")) {
		return errors.New("proxy must start with http://, https:// or socks5://")
	}
	ids := map[string]bool{}
	for _, p := range s.Providers {
		if p.ID == "" || ids[p.ID] {
			return errors.New("provider ids must be unique")
		}
		ids[p.ID] = true
	}
	if s.DescribeModel.Set() && !ids[s.DescribeModel.ProviderID] {
		s.DescribeModel = ModelRef{}
	}
	if s.ImageModel.Set() && !ids[s.ImageModel.ProviderID] {
		s.ImageModel = ModelRef{}
	}
	return nil
}

// SettingsStore keeps settings.json in sync. It re-reads the file when it
// changes on disk, so `blendartai set-key` from the menu takes effect without
// a restart.
type SettingsStore struct {
	path string
	mu   sync.Mutex
	cur  Settings
	mod  int64
}

func OpenSettings(dataDir string) (*SettingsStore, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	s := &SettingsStore{path: filepath.Join(dataDir, "settings.json"), cur: DefaultSettings()}
	if _, err := os.Stat(s.path); errors.Is(err, os.ErrNotExist) {
		if err := s.save(s.cur); err != nil {
			return nil, err
		}
	}
	if err := s.reloadLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *SettingsStore) reloadLocked() error {
	fi, err := os.Stat(s.path)
	if err != nil {
		return err
	}
	if fi.ModTime().UnixNano() == s.mod {
		return nil
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	next := DefaultSettings()
	if err := json.Unmarshal(raw, &next); err != nil {
		return fmt.Errorf("bad settings.json: %w", err)
	}
	if next.DescribePrompt == oldDefaultPrompt {
		next.DescribePrompt = DefaultDescribePrompt
	}
	s.cur = next
	s.mod = fi.ModTime().UnixNano()
	return nil
}

func (s *SettingsStore) Get() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.reloadLocked()
	return s.cur
}

// Update applies fn to a copy of the current settings, validates and saves.
func (s *SettingsStore) Update(fn func(*Settings) error) (Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.reloadLocked()
	next := s.cur
	if err := fn(&next); err != nil {
		return s.cur, err
	}
	if err := next.Validate(); err != nil {
		return s.cur, err
	}
	if err := s.save(next); err != nil {
		return s.cur, err
	}
	s.cur = next
	if fi, err := os.Stat(s.path); err == nil {
		s.mod = fi.ModTime().UnixNano()
	}
	return next, nil
}

func (s *SettingsStore) save(v Settings) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(s.path, raw, 0o600)
}

func writeAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
