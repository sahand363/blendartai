package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sahand363/blendartai/internal/config"
	"github.com/sahand363/blendartai/internal/imageprep"
	"github.com/sahand363/blendartai/internal/providers"
	"github.com/sahand363/blendartai/internal/runner"
)

// ---------- engine ----------

type describer struct {
	c     *providers.Client
	model string
}

func (d describer) Describe(ctx context.Context, instruction string, imgs []imageprep.Image) (string, error) {
	return d.c.Describe(ctx, d.model, instruction, imgs)
}

type maker struct {
	c     *providers.Client
	model string
	size  string
}

func (m maker) Make(ctx context.Context, prompt string) ([]byte, string, error) {
	return m.c.GenerateImage(ctx, m.model, prompt, m.size)
}

func clientFor(set config.Settings, ref config.ModelRef, role string) (*providers.Client, config.Provider, error) {
	if !ref.Set() {
		return nil, config.Provider{}, fmt.Errorf("pick a %s model in Models first", role)
	}
	p, ok := set.Provider(ref.ProviderID)
	if !ok {
		return nil, config.Provider{}, fmt.Errorf("the %s model's provider was removed, pick another", role)
	}
	c, err := providers.New(providers.Config{Kind: p.Kind, BaseURL: p.BaseURL, APIKey: p.APIKey}, set.ProxyURL)
	if err != nil {
		return nil, p, fmt.Errorf("%s provider %q: %w", role, p.Name, err)
	}
	return c, p, nil
}

// buildEngine makes the two active models ready for a run.
func buildEngine(set config.Settings) (runner.Engine, string, string, error) {
	dc, dp, err := clientFor(set, set.DescribeModel, "describe")
	if err != nil {
		return runner.Engine{}, "", "", err
	}
	ic, ip, err := clientFor(set, set.ImageModel, "image")
	if err != nil {
		return runner.Engine{}, "", "", err
	}
	if !providers.CanMakeImages(ip.Kind) {
		return runner.Engine{}, "", "", errors.New("Anthropic can't make images, pick another image model")
	}
	eng := runner.Engine{
		Describer:  describer{dc, set.DescribeModel.Model},
		ImageMaker: maker{ic, set.ImageModel.Model, set.ImageSize},
	}
	return eng, dp.Name + " / " + set.DescribeModel.Model, ip.Name + " / " + set.ImageModel.Model, nil
}

// ---------- view ----------

type providerView struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Name      string    `json:"name"`
	BaseURL   string    `json:"base_url"`
	KeyLast4  string    `json:"key_last4"`
	Models    []string  `json:"models"`
	CheckedAt time.Time `json:"checked_at"`
	CheckErr  string    `json:"check_error"`
	CanImages bool      `json:"can_images"`
}

func settingsView(s config.Settings) map[string]any {
	ps := make([]providerView, 0, len(s.Providers))
	for _, p := range s.Providers {
		v := providerView{ID: p.ID, Kind: p.Kind, Name: p.Name, BaseURL: p.BaseURL, Models: p.Models,
			CheckedAt: p.CheckedAt, CheckErr: p.CheckErr, CanImages: providers.CanMakeImages(p.Kind)}
		if v.Models == nil {
			v.Models = []string{}
		}
		if k := p.APIKey; len(k) >= 4 {
			v.KeyLast4 = k[len(k)-4:]
		}
		ps = append(ps, v)
	}
	defaults := map[string]string{}
	for _, k := range providers.Kinds {
		defaults[k] = providers.DefaultBaseURL(k)
	}
	return map[string]any{
		"providers":       ps,
		"describe_model":  s.DescribeModel,
		"image_model":     s.ImageModel,
		"describe_prompt": s.DescribePrompt,
		"default_prompt":  config.DefaultDescribePrompt,
		"image_size":      s.ImageSize,
		"target_images":   s.TargetImages,
		"price_per_image": s.PricePerImage,
		"max_usd":         s.MaxUSD,
		"proxy_url":       s.ProxyURL,
		"api_token_set":   s.APITokenHash != "",
		"default_urls":    defaults,
	}
}

// ---------- providers ----------

type providerReq struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
}

// check lists the models with the provider's key and stores the result.
func (s *Server) check(ctx context.Context, p *config.Provider, proxy string) {
	p.CheckedAt = time.Now().UTC()
	c, err := providers.New(providers.Config{Kind: p.Kind, BaseURL: p.BaseURL, APIKey: p.APIKey}, proxy)
	if err == nil {
		cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		var models []string
		models, err = c.ListModels(cctx)
		cancel()
		if err == nil {
			p.Models = models
			p.CheckErr = ""
			return
		}
	}
	p.CheckErr = err.Error()
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func cleanProvider(req providerReq) (providerReq, error) {
	req.Kind = strings.ToLower(strings.TrimSpace(req.Kind))
	req.Name = strings.TrimSpace(req.Name)
	req.BaseURL = strings.TrimRight(strings.TrimSpace(req.BaseURL), "/")
	req.APIKey = strings.TrimSpace(req.APIKey)
	ok := false
	for _, k := range providers.Kinds {
		ok = ok || k == req.Kind
	}
	if !ok {
		return req, errors.New("pick OpenAI, Gemini, Anthropic or Custom")
	}
	if req.Kind == providers.KindCustom && req.BaseURL == "" {
		return req, errors.New("custom providers need a base URL, like https://host/v1")
	}
	if req.BaseURL == providers.DefaultBaseURL(req.Kind) {
		req.BaseURL = ""
	}
	if len(req.Name) > 40 {
		req.Name = req.Name[:40]
	}
	if req.Name == "" {
		req.Name = map[string]string{"openai": "OpenAI", "gemini": "Gemini", "anthropic": "Anthropic", "custom": "Custom"}[req.Kind]
	}
	return req, nil
}

func (s *Server) handleAddProvider(w http.ResponseWriter, r *http.Request) {
	var req providerReq
	if !readJSON(w, r, &req) {
		return
	}
	req, err := cleanProvider(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.APIKey == "" {
		writeErr(w, http.StatusBadRequest, "paste the API key")
		return
	}
	p := config.Provider{ID: newID(), Kind: req.Kind, Name: req.Name, BaseURL: req.BaseURL, APIKey: req.APIKey}
	s.check(r.Context(), &p, s.settings.Get().ProxyURL)
	next, err := s.settings.Update(func(c *config.Settings) error {
		c.Providers = append(c.Providers, p)
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, settingsView(next))
}

func (s *Server) handleEditProvider(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	var req providerReq
	if !readJSON(w, r, &req) {
		return
	}
	set := s.settings.Get()
	p, ok := set.Provider(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "provider not found")
		return
	}
	req.Kind = p.Kind
	req, err := cleanProvider(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p.Name, p.BaseURL = req.Name, req.BaseURL
	if req.APIKey != "" {
		p.APIKey = req.APIKey
	}
	s.check(r.Context(), &p, set.ProxyURL)
	next, err := s.settings.Update(func(c *config.Settings) error {
		for i := range c.Providers {
			if c.Providers[i].ID == id {
				c.Providers[i] = p
				return nil
			}
		}
		return errors.New("provider not found")
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, settingsView(next))
}

func (s *Server) handleCheckProvider(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	set := s.settings.Get()
	p, ok := set.Provider(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "provider not found")
		return
	}
	s.check(r.Context(), &p, set.ProxyURL)
	next, err := s.settings.Update(func(c *config.Settings) error {
		for i := range c.Providers {
			if c.Providers[i].ID == id {
				c.Providers[i].Models, c.Providers[i].CheckErr, c.Providers[i].CheckedAt = p.Models, p.CheckErr, p.CheckedAt
			}
		}
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, settingsView(next))
}

func (s *Server) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	next, err := s.settings.Update(func(c *config.Settings) error {
		out := c.Providers[:0]
		found := false
		for _, p := range c.Providers {
			if p.ID == id {
				found = true
				continue
			}
			out = append(out, p)
		}
		if !found {
			return errors.New("provider not found")
		}
		c.Providers = out
		return nil // Validate clears active models that pointed at it
	})
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, settingsView(next))
}

func (s *Server) handleSetActive(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Role       string `json:"role"` // describe | image
		ProviderID string `json:"provider_id"`
		Model      string `json:"model"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	req.Model = strings.TrimSpace(req.Model)
	if req.Model == "" || len(req.Model) > 200 {
		writeErr(w, http.StatusBadRequest, "model name is empty")
		return
	}
	next, err := s.settings.Update(func(c *config.Settings) error {
		p, ok := c.Provider(req.ProviderID)
		if !ok {
			return errors.New("provider not found")
		}
		ref := config.ModelRef{ProviderID: p.ID, Model: req.Model}
		switch req.Role {
		case "describe":
			c.DescribeModel = ref
		case "image":
			if !providers.CanMakeImages(p.Kind) {
				return errors.New("Anthropic can't make images, use it for describing")
			}
			c.ImageModel = ref
		default:
			return errors.New("role must be describe or image")
		}
		return nil
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, settingsView(next))
}
