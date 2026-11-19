package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	SourceDir     string
	OutDir        string
	ListenAddr    string
	BasePath      string
	Token         string
	GeminiAPIKey  string
	TextModel     string
	ImageModel    string
	ImageSize     string
	PricePerImage float64
	MaxImages     int
	MaxUSD        float64
	ImagesPerPair int
}

func Load() (Config, error) {
	cfg := Config{
		SourceDir:     envString("SOURCE_DIR", "./sources"),
		OutDir:        envString("OUT_DIR", "./out"),
		ListenAddr:    envString("LISTEN_ADDR", "127.0.0.1:8090"),
		BasePath:      normalizeBasePath(envString("BASE_PATH", "")),
		Token:         os.Getenv("BLENDMIX_TOKEN"),
		GeminiAPIKey:  os.Getenv("GEMINI_API_KEY"),
		TextModel:     envString("TEXT_MODEL", "gemini-3.1-flash-lite"),
		ImageModel:    envString("IMAGE_MODEL", "gemini-nano-banana-2.1"),
		ImageSize:     envString("IMAGE_SIZE", "1K"),
		PricePerImage: 0.0336,
		MaxImages:     100,
		MaxUSD:        5.00,
		ImagesPerPair: 5,
	}

	if cfg.Token == "" {
		return Config{}, errors.New("BLENDMIX_TOKEN is required")
	}
	if cfg.GeminiAPIKey == "" {
		return Config{}, errors.New("GEMINI_API_KEY is required")
	}

	var err error
	if cfg.PricePerImage, err = envFloat("PRICE_PER_IMAGE", cfg.PricePerImage); err != nil {
		return Config{}, err
	}
	if cfg.MaxImages, err = envInt("MAX_IMAGES", cfg.MaxImages); err != nil {
		return Config{}, err
	}
	if cfg.MaxUSD, err = envFloat("MAX_USD", cfg.MaxUSD); err != nil {
		return Config{}, err
	}
	if cfg.ImagesPerPair, err = envInt("IMAGES_PER_PAIR", cfg.ImagesPerPair); err != nil {
		return Config{}, err
	}
	if cfg.ImagesPerPair < 1 {
		return Config{}, errors.New("IMAGES_PER_PAIR must be >= 1")
	}
	if cfg.MaxImages < 1 {
		return Config{}, errors.New("MAX_IMAGES must be >= 1")
	}
	if cfg.MaxUSD <= 0 {
		return Config{}, errors.New("MAX_USD must be > 0")
	}

	return cfg, nil
}

func normalizeBasePath(p string) string {
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

func envInt(key string, def int) (int, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", key, err)
	}
	return n, nil
}

func envFloat(key string, def float64) (float64, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", key, err)
	}
	return f, nil
}
