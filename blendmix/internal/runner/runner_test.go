package runner

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sahand363/blendartai/blendmix/internal/config"
	"github.com/sahand363/blendartai/blendmix/internal/gemini"
	"github.com/sahand363/blendartai/blendmix/internal/store"
)

func TestShuffledPairsNoRepeats(t *testing.T) {
	items := []string{"a.png", "b.png", "c.png", "d.png", "e.png"}
	pairs := ShuffledPairs(items, rand.New(rand.NewSource(42)))

	wantCount := len(items) * (len(items) - 1) / 2
	if len(pairs) != wantCount {
		t.Fatalf("got %d pairs, want %d", len(pairs), wantCount)
	}

	seen := make(map[string]bool)
	for _, p := range pairs {
		if p.A == p.B {
			t.Fatalf("self pair: %v", p)
		}
		a, b := p.A, p.B
		if a > b {
			a, b = b, a
		}
		key := a + "|" + b
		if seen[key] {
			t.Fatalf("duplicate unordered pair: %s", key)
		}
		seen[key] = true
	}
}

type fakeGenerator struct {
	describeErr error
	generateErr error
	prompts     []string
}

func (f *fakeGenerator) PrepareImage(path string) (gemini.PreparedImage, error) {
	return gemini.PreparedImage{Data: []byte("img"), MIMEType: "image/png"}, nil
}

func (f *fakeGenerator) DescribePair(ctx context.Context, imgA, imgB gemini.PreparedImage, count int) (gemini.DescribeResult, error) {
	if f.describeErr != nil {
		return gemini.DescribeResult{}, f.describeErr
	}
	p := f.prompts
	if len(p) == 0 {
		p = []string{"p1", "p2", "p3", "p4", "p5"}
	}
	return gemini.DescribeResult{Prompts: p, RawJSON: []byte(`{"prompts":["p1"]}`)}, nil
}

func (f *fakeGenerator) GenerateImage(ctx context.Context, prompt string, imgA, imgB gemini.PreparedImage) ([]byte, string, error) {
	if f.generateErr != nil {
		return nil, "", f.generateErr
	}
	return []byte("pngdata"), "image/png", nil
}

func setupTestRunner(t *testing.T, gen gemini.Generator, cfg config.Config) *Runner {
	t.Helper()
	srcDir := t.TempDir()
	for i := 0; i < 4; i++ {
		p := filepath.Join(srcDir, fmt.Sprintf("img%d.png", i))
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.SourceDir = srcDir
	if cfg.ImagesPerPair == 0 {
		cfg.ImagesPerPair = 5
	}
	if cfg.PricePerImage == 0 {
		cfg.PricePerImage = 0.0336
	}
	r := New(cfg, gen, st, nil)
	r.sleepFn = func(ctx context.Context, d time.Duration) error { return nil }
	return r
}

func TestStopConditionMaxImages(t *testing.T) {
	r := setupTestRunner(t, &fakeGenerator{}, config.Config{
		MaxImages:     3,
		MaxUSD:        100,
		PricePerImage: 0.0336,
		ImagesPerPair: 5,
	})
	if _, err := r.Start(RunOverrides{}); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	st := r.Status()
	if st.StopReason != "max_images_reached" {
		t.Fatalf("got stop_reason=%q, want max_images_reached", st.StopReason)
	}
	if st.ImagesDone != 3 {
		t.Fatalf("got images_done=%d, want 3", st.ImagesDone)
	}
}

func TestStopConditionMaxUSD(t *testing.T) {
	r := setupTestRunner(t, &fakeGenerator{}, config.Config{
		MaxImages:     100,
		MaxUSD:        0.05,
		PricePerImage: 0.03,
		ImagesPerPair: 5,
	})
	if _, err := r.Start(RunOverrides{}); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	st := r.Status()
	if st.StopReason != "max_usd_reached" {
		t.Fatalf("got stop_reason=%q, want max_usd_reached", st.StopReason)
	}
	if st.ImagesDone != 2 {
		t.Fatalf("got images_done=%d, want 2", st.ImagesDone)
	}
}

func TestStopConditionQuotaExhausted(t *testing.T) {
	r := setupTestRunner(t, &fakeGenerator{
		generateErr: errors.New("API error 429 RESOURCE_EXHAUSTED"),
	}, config.Config{
		MaxImages:     100,
		MaxUSD:        10,
		PricePerImage: 0.03,
		ImagesPerPair: 5,
	})
	if _, err := r.Start(RunOverrides{}); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	st := r.Status()
	if st.StopReason != "quota_exhausted" {
		t.Fatalf("got stop_reason=%q, want quota_exhausted", st.StopReason)
	}
}
