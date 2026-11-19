package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sahand363/blendartai/blendmix/internal/config"
	"github.com/sahand363/blendartai/blendmix/internal/gemini"
	"github.com/sahand363/blendartai/blendmix/internal/store"
)

var ErrAlreadyRunning = errors.New("a run is already in progress")

type RunOverrides struct {
	MaxImages *int     `json:"max_images,omitempty"`
	MaxUSD    *float64 `json:"max_usd,omitempty"`
}

type Pair struct {
	A string
	B string
}

type Runner struct {
	cfg    config.Config
	gen    gemini.Generator
	store  *store.Store
	logger *slog.Logger

	// sleepFn allows deterministic fast tests.
	sleepFn func(ctx context.Context, d time.Duration) error

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	status  store.RunState
	doneCh  chan struct{}

	// stopReq asks the loop to stop before starting the next image,
	// so an in-flight (already billed) generation is never thrown away.
	stopReq atomic.Bool
}

func New(cfg config.Config, gen gemini.Generator, st *store.Store, logger *slog.Logger) *Runner {
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{
		cfg:     cfg,
		gen:     gen,
		store:   st,
		logger:  logger,
		sleepFn: sleepWithContext,
		status: store.RunState{
			Status: "idle",
		},
	}
}

func (r *Runner) Status() store.RunState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

func (r *Runner) Start(overrides RunOverrides) (store.RunState, error) {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return store.RunState{}, ErrAlreadyRunning
	}

	maxImages := r.cfg.MaxImages
	if overrides.MaxImages != nil && *overrides.MaxImages > 0 {
		maxImages = *overrides.MaxImages
	}
	maxUSD := r.cfg.MaxUSD
	if overrides.MaxUSD != nil && *overrides.MaxUSD > 0 {
		maxUSD = *overrides.MaxUSD
	}

	runID := time.Now().Format("2006-01-02_150405")
	if _, err := r.store.CreateRun(runID); err != nil {
		r.mu.Unlock()
		return store.RunState{}, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	r.running = true
	r.stopReq.Store(false)
	r.cancel = cancel
	r.doneCh = make(chan struct{})
	r.status = store.RunState{
		RunID:     runID,
		Status:    "running",
		StartedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	initial := r.status
	r.mu.Unlock()

	_ = r.store.WriteRunState(runID, initial)

	go r.loop(ctx, runID, maxImages, maxUSD)
	return initial, nil
}

// Stop requests a graceful stop: the run ends after the current image.
func (r *Runner) Stop() {
	r.mu.Lock()
	if r.running && r.status.Status == "running" {
		r.status.Status = "stopping"
	}
	r.mu.Unlock()
	r.stopReq.Store(true)
}

// Abort cancels the run immediately, including any in-flight request.
// Used on process shutdown.
func (r *Runner) Abort() {
	r.stopReq.Store(true)
	r.mu.Lock()
	cancel := r.cancel
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Wait blocks until the current run finishes (useful in tests).
func (r *Runner) Wait() {
	r.mu.Lock()
	ch := r.doneCh
	r.mu.Unlock()
	if ch != nil {
		<-ch
	}
}

func (r *Runner) loop(ctx context.Context, runID string, maxImages int, maxUSD float64) {
	defer func() {
		r.mu.Lock()
		r.running = false
		if r.cancel != nil {
			r.cancel()
		}
		r.cancel = nil
		close(r.doneCh)
		r.mu.Unlock()
	}()

	sources, err := ListSourceImages(r.cfg.SourceDir)
	if err != nil {
		r.finish(runID, "error", err.Error())
		return
	}
	if len(sources) < 2 {
		r.finish(runID, "not_enough_sources", "need at least 2 source images")
		return
	}

	pairs := ShuffledPairs(sources, rand.New(rand.NewSource(time.Now().UnixNano())))
	r.logger.Info("run started", "run_id", runID, "sources", len(sources), "total_pairs", len(pairs), "max_images", maxImages, "max_usd", maxUSD)

	for i, pair := range pairs {
		if ctx.Err() != nil || r.stopReq.Load() {
			r.finish(runID, "stop_requested", "")
			return
		}

		st := r.Status()
		if st.ImagesDone >= maxImages {
			r.finish(runID, "max_images_reached", "")
			return
		}
		if st.EstSpendUSD >= maxUSD {
			r.finish(runID, "max_usd_reached", "")
			return
		}

		pairLabel := fmt.Sprintf("%s + %s", filepath.Base(pair.A), filepath.Base(pair.B))
		r.update(runID, func(s *store.RunState) {
			s.CurrentPair = pairLabel
		})

		imgA, err := r.gen.PrepareImage(pair.A)
		if err != nil {
			r.recordError(runID, fmt.Errorf("prepare %s: %w", pair.A, err))
			continue
		}
		imgB, err := r.gen.PrepareImage(pair.B)
		if err != nil {
			r.recordError(runID, fmt.Errorf("prepare %s: %w", pair.B, err))
			continue
		}

		desc, quotaHit, err := r.describeWithRetry(ctx, imgA, imgB)
		if err != nil {
			if ctx.Err() != nil {
				r.finish(runID, "stop_requested", "")
				return
			}
			if quotaHit {
				r.finish(runID, "quota_exhausted", err.Error())
				return
			}
			r.recordError(runID, fmt.Errorf("describe %s: %w", pairLabel, err))
			continue
		}

		pairDir, err := r.store.PairDir(runID, i+1, filepath.Base(pair.A), filepath.Base(pair.B))
		if err != nil {
			r.finish(runID, "store_error", err.Error())
			return
		}
		if err := r.store.WriteDescribe(pairDir, desc.RawJSON); err != nil {
			r.recordError(runID, err)
		}

		prompts := desc.Prompts
		if len(prompts) > r.cfg.ImagesPerPair {
			prompts = prompts[:r.cfg.ImagesPerPair]
		}

		for idx, prompt := range prompts {
			if ctx.Err() != nil || r.stopReq.Load() {
				r.finish(runID, "stop_requested", "")
				return
			}
			cur := r.Status()
			if cur.ImagesDone >= maxImages {
				r.finish(runID, "max_images_reached", "")
				return
			}
			if cur.EstSpendUSD >= maxUSD {
				r.finish(runID, "max_usd_reached", "")
				return
			}

			imgBytes, mime, quotaHit, err := r.generateWithRetry(ctx, prompt, imgA, imgB)
			if err != nil {
				if ctx.Err() != nil {
					r.finish(runID, "stop_requested", "")
					return
				}
				if quotaHit {
					r.finish(runID, "quota_exhausted", err.Error())
					return
				}
				r.recordError(runID, fmt.Errorf("generate %s #%d: %w", pairLabel, idx+1, err))
				continue
			}

			meta := store.ImageMeta{
				SourceFiles: []string{filepath.Base(pair.A), filepath.Base(pair.B)},
				Prompt:      prompt,
				Model:       r.cfg.ImageModel,
				Size:        r.cfg.ImageSize,
				Timestamp:   time.Now().UTC(),
				EstCostUSD:  r.cfg.PricePerImage,
			}
			if err := r.store.WriteImageAndMeta(pairDir, idx+1, imgBytes, mime, meta); err != nil {
				r.recordError(runID, fmt.Errorf("save image: %w", err))
				continue
			}

			r.update(runID, func(s *store.RunState) {
				s.ImagesDone++
				s.EstSpendUSD += r.cfg.PricePerImage
			})
		}

		r.update(runID, func(s *store.RunState) {
			s.PairsDone++
		})
	}

	r.finish(runID, "all_pairs_used", "")
}

func (r *Runner) describeWithRetry(ctx context.Context, imgA, imgB gemini.PreparedImage) (gemini.DescribeResult, bool, error) {
	return callWithRetry(ctx, r.sleepFn, func() (gemini.DescribeResult, error) {
		return r.gen.DescribePair(ctx, imgA, imgB, r.cfg.ImagesPerPair)
	})
}

func (r *Runner) generateWithRetry(ctx context.Context, prompt string, imgA, imgB gemini.PreparedImage) ([]byte, string, bool, error) {
	type res struct {
		data []byte
		mime string
	}
	out, quotaHit, err := callWithRetry(ctx, r.sleepFn, func() (res, error) {
		d, m, e := r.gen.GenerateImage(ctx, prompt, imgA, imgB)
		return res{data: d, mime: m}, e
	})
	return out.data, out.mime, quotaHit, err
}

func callWithRetry[T any](
	ctx context.Context,
	sleepFn func(context.Context, time.Duration) error,
	fn func() (T, error),
) (T, bool, error) {
	var zero T
	backoffs := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}

	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		val, err := fn()
		if err == nil {
			return val, false, nil
		}
		lastErr = err

		isQuota := gemini.IsQuotaError(err)
		isUnavail := gemini.IsUnavailableError(err)
		if !isQuota && !isUnavail {
			return zero, false, err
		}

		// 3 retries (4 total tries) with 2s, 4s, 8s backoff.
		if attempt == 3 {
			return zero, isQuota, lastErr
		}
		if err := sleepFn(ctx, backoffs[attempt]); err != nil {
			return zero, false, err
		}
	}
	return zero, false, lastErr
}

func (r *Runner) update(runID string, fn func(*store.RunState)) {
	r.mu.Lock()
	fn(&r.status)
	r.status.UpdatedAt = time.Now().UTC()
	snap := r.status
	r.mu.Unlock()
	_ = r.store.WriteRunState(runID, snap)
}

func (r *Runner) recordError(runID string, err error) {
	r.logger.Warn("run step error", "run_id", runID, "error", err)
	r.update(runID, func(s *store.RunState) {
		s.LastError = err.Error()
	})
}

func (r *Runner) finish(runID, reason, lastErr string) {
	r.update(runID, func(s *store.RunState) {
		s.Status = "stopped"
		s.StopReason = reason
		s.CurrentPair = ""
		if lastErr != "" {
			s.LastError = lastErr
		}
	})
	r.logger.Info("run finished", "run_id", runID, "stop_reason", reason, "last_error", lastErr)
}

// ListSourceImages returns sorted paths of .png, .jpg, .jpeg, .webp files in dir.
func ListSourceImages(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		switch ext {
		case ".png", ".jpg", ".jpeg", ".webp":
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

// ShuffledPairs generates all unique unordered pairs from items in random order.
func ShuffledPairs(items []string, rng *rand.Rand) []Pair {
	n := len(items)
	if n < 2 {
		return nil
	}
	pairs := make([]Pair, 0, n*(n-1)/2)
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			pairs = append(pairs, Pair{A: items[i], B: items[j]})
		}
	}
	rng.Shuffle(len(pairs), func(i, j int) {
		pairs[i], pairs[j] = pairs[j], pairs[i]
	})
	return pairs
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
