// Package runner is the Blendartai batch loop.
//
// Each step picks two different source images at random: the first gives
// the content, the second the style. The describe model writes a prompt for
// the first image in the style of the second, the prompt is saved, and the
// image model makes a picture from that prompt alone. Steps repeat until the
// target number of images is reached, the spend guard trips, the provider
// runs out of quota, or the user stops. Pause holds the loop between steps.
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
	"time"

	"github.com/sahand363/blendartai/internal/imageprep"
	"github.com/sahand363/blendartai/internal/providers"
	"github.com/sahand363/blendartai/internal/store"
)

var (
	ErrAlreadyRunning = errors.New("a run is already in progress")
	ErrNotRunning     = errors.New("no run in progress")
)

// MaxConsecutiveErrors stops a run that keeps failing the same way.
const MaxConsecutiveErrors = 5

// Describer writes the restyled prompt from the two images.
type Describer interface {
	Describe(ctx context.Context, instruction string, imgs []imageprep.Image) (string, error)
}

// ImageMaker turns a prompt into an image.
type ImageMaker interface {
	Make(ctx context.Context, prompt string) ([]byte, string, error)
}

type Engine struct {
	Describer  Describer
	ImageMaker ImageMaker
	// Load reads and shrinks a source image. Defaults to imageprep.Load.
	Load func(path string) (imageprep.Image, error)
}

// RunConfig is a snapshot of the settings a run uses.
type RunConfig struct {
	SourceDir     string
	Target        int
	PricePerImage float64
	MaxUSD        float64 // 0 = no spend guard
	Prompt        string
	DescribeModel string // label for the sidecar, e.g. "openai/gpt-4.1"
	ImageModel    string
	ImageSize     string
}

type Runner struct {
	store  *store.Store
	logger *slog.Logger

	sleepFn func(ctx context.Context, d time.Duration) error // swapped in tests
	rng     *rand.Rand

	mu       sync.Mutex
	running  bool
	cancel   context.CancelFunc
	status   store.RunState
	doneCh   chan struct{}
	stopReq  bool
	pauseReq bool
	resumeCh chan struct{}
}

func New(st *store.Store, logger *slog.Logger) *Runner {
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{
		store:   st,
		logger:  logger,
		sleepFn: sleepCtx,
		rng:     rand.New(rand.NewSource(time.Now().UnixNano())),
		status:  store.RunState{Status: "idle"},
	}
}

func (r *Runner) Status() store.RunState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

func (r *Runner) Running() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

// Start begins a run. Only one run at a time.
func (r *Runner) Start(cfg RunConfig, eng Engine) (store.RunState, error) {
	if eng.Describer == nil || eng.ImageMaker == nil {
		return store.RunState{}, errors.New("both models must be set")
	}
	if eng.Load == nil {
		eng.Load = imageprep.Load
	}
	if cfg.Target < 1 {
		cfg.Target = 1
	}
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return store.RunState{}, ErrAlreadyRunning
	}
	runID := time.Now().Format("2006-01-02_150405")
	if _, err := r.store.CreateRun(runID); err != nil {
		r.mu.Unlock()
		return store.RunState{}, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.running, r.stopReq, r.pauseReq = true, false, false
	r.resumeCh = nil
	r.cancel = cancel
	r.doneCh = make(chan struct{})
	now := time.Now().UTC()
	r.status = store.RunState{
		RunID: runID, Status: "running", MaxImages: cfg.Target, MaxUSD: cfg.MaxUSD,
		DescribeModel: cfg.DescribeModel, ImageModel: cfg.ImageModel,
		StartedAt: now, UpdatedAt: now,
	}
	snap := r.status
	r.mu.Unlock()
	_ = r.store.WriteRunState(runID, snap)
	go r.loop(ctx, runID, cfg, eng)
	return snap, nil
}

// Pause holds the run after the current step finishes.
func (r *Runner) Pause() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.running {
		return ErrNotRunning
	}
	if !r.pauseReq && !r.stopReq {
		r.pauseReq = true
		r.resumeCh = make(chan struct{})
		if r.status.Status == "running" {
			r.status.Status = "pausing"
		}
	}
	return nil
}

// Resume continues a paused run.
func (r *Runner) Resume() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.running {
		return ErrNotRunning
	}
	if r.pauseReq {
		r.pauseReq = false
		close(r.resumeCh)
		r.resumeCh = nil
		if r.status.Status == "paused" || r.status.Status == "pausing" {
			r.status.Status = "running"
		}
	}
	return nil
}

// Stop ends the run after the current step (or at once when paused).
func (r *Runner) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.running {
		return
	}
	r.stopReq = true
	if r.pauseReq {
		r.pauseReq = false
		close(r.resumeCh)
		r.resumeCh = nil
	}
	r.status.Status = "stopping"
}

// Abort cancels everything at once, including requests in flight.
func (r *Runner) Abort() {
	r.Stop()
	r.mu.Lock()
	cancel := r.cancel
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Wait blocks until the current run ends.
func (r *Runner) Wait() {
	r.mu.Lock()
	ch := r.doneCh
	r.mu.Unlock()
	if ch != nil {
		<-ch
	}
}

// checkpoint is called between steps. It waits while paused and reports
// whether the loop must end.
func (r *Runner) checkpoint(ctx context.Context, runID string) bool {
	r.mu.Lock()
	if r.pauseReq && !r.stopReq {
		ch := r.resumeCh
		r.status.Status = "paused"
		r.status.CurrentPair = ""
		r.status.UpdatedAt = time.Now().UTC()
		snap := r.status
		r.mu.Unlock()
		_ = r.store.WriteRunState(runID, snap)
		r.logger.Info("run paused", "run_id", runID)
		select {
		case <-ch:
		case <-ctx.Done():
		}
		r.mu.Lock()
		if !r.stopReq && ctx.Err() == nil {
			r.status.Status = "running"
			r.logger.Info("run resumed", "run_id", runID)
		}
	}
	stop := r.stopReq || ctx.Err() != nil
	r.mu.Unlock()
	return stop
}

func (r *Runner) loop(ctx context.Context, runID string, cfg RunConfig, eng Engine) {
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

	sources, err := ListSourceImages(cfg.SourceDir)
	if err != nil {
		r.finish(runID, "error", err.Error())
		return
	}
	if len(sources) < 2 {
		r.finish(runID, "not_enough_sources", "need at least 2 source images")
		return
	}
	r.logger.Info("run started", "run_id", runID, "sources", len(sources), "target", cfg.Target,
		"describe", cfg.DescribeModel, "image", cfg.ImageModel)

	deck := &pairDeck{items: sources, rng: r.rng}
	cache := map[string]imageprep.Image{}
	load := func(p string) (imageprep.Image, error) {
		if im, ok := cache[p]; ok {
			return im, nil
		}
		im, err := eng.Load(p)
		if err == nil {
			cache[p] = im
		}
		return im, err
	}
	failures := 0
	step := 0

	for {
		if r.checkpoint(ctx, runID) {
			r.finish(runID, "stopped_by_user", "")
			return
		}
		st := r.Status()
		if st.ImagesDone >= cfg.Target {
			r.finish(runID, "target_reached", "")
			return
		}
		if cfg.MaxUSD > 0 && cfg.PricePerImage > 0 && st.EstSpendUSD+cfg.PricePerImage > cfg.MaxUSD+1e-9 {
			r.finish(runID, "max_usd_reached", "")
			return
		}
		if failures >= MaxConsecutiveErrors {
			r.finish(runID, "too_many_errors", st.LastError)
			return
		}

		step++
		pair := deck.next()
		content, style := filepath.Base(pair[0]), filepath.Base(pair[1])
		label := content + " in the style of " + style
		r.update(runID, func(s *store.RunState) { s.CurrentPair = label; s.Steps = step })

		imgA, err := load(pair[0])
		if err == nil {
			var imgB imageprep.Image
			imgB, err = load(pair[1])
			if err == nil {
				err = r.doStep(ctx, runID, step, cfg, eng, pair, imgA, imgB)
			}
		}
		if err == nil {
			failures = 0
			continue
		}
		if ctx.Err() != nil {
			r.finish(runID, "stopped_by_user", "")
			return
		}
		if providers.IsQuota(err) {
			r.finish(runID, "quota_exhausted", err.Error())
			return
		}
		if providers.IsAuth(err) {
			r.finish(runID, "api_key_rejected", err.Error())
			return
		}
		failures++
		r.recordError(runID, fmt.Errorf("%s: %w", label, err))
	}
}

func (r *Runner) doStep(ctx context.Context, runID string, step int, cfg RunConfig, eng Engine, pair [2]string, imgA, imgB imageprep.Image) error {
	content, style := filepath.Base(pair[0]), filepath.Base(pair[1])

	desc, err := retry(ctx, r.sleepFn, func() (string, error) {
		return eng.Describer.Describe(ctx, cfg.Prompt, []imageprep.Image{imgA, imgB})
	})
	if err != nil {
		return fmt.Errorf("describe: %w", err)
	}
	r.update(runID, func(s *store.RunState) { s.Descriptions++ })

	dir, err := r.store.PairDir(runID, step, content, style)
	if err != nil {
		return err
	}
	if err := store.WriteFileAtomic(filepath.Join(dir, "description.txt"), []byte(desc+"\n"), 0o644); err != nil {
		return err
	}

	type img struct {
		data []byte
		mime string
	}
	out, err := retry(ctx, r.sleepFn, func() (img, error) {
		d, m, e := eng.ImageMaker.Make(ctx, desc)
		return img{d, m}, e
	})
	if err != nil {
		return fmt.Errorf("image: %w", err)
	}
	if len(out.data) == 0 {
		return errors.New("image: empty answer")
	}
	meta := store.ImageMeta{
		SourceFiles:   []string{content, style},
		ContentImage:  content,
		StyleImage:    style,
		Prompt:        desc,
		DescribeModel: cfg.DescribeModel,
		Model:         cfg.ImageModel,
		Size:          cfg.ImageSize,
		Timestamp:     time.Now().UTC(),
		EstCostUSD:    cfg.PricePerImage,
	}
	if err := r.store.WriteImageAndMeta(dir, 1, out.data, out.mime, meta); err != nil {
		return fmt.Errorf("save: %w", err)
	}
	r.update(runID, func(s *store.RunState) {
		s.ImagesDone++
		s.PairsDone++
		s.EstSpendUSD += cfg.PricePerImage
	})
	return nil
}

// retry tries fn up to 4 times on temporary errors (2s, 4s, 8s) and up to
// 3 times on rate limits (15s, 30s) before giving up.
func retry[T any](ctx context.Context, sleep func(context.Context, time.Duration) error, fn func() (T, error)) (T, error) {
	var zero T
	tempWait := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}
	quotaWait := []time.Duration{15 * time.Second, 30 * time.Second}
	ti, qi := 0, 0
	for {
		v, err := fn()
		if err == nil {
			return v, nil
		}
		var d time.Duration
		switch {
		case providers.IsQuota(err) && qi < len(quotaWait):
			d = quotaWait[qi]
			qi++
		case providers.IsRetryable(err) && ti < len(tempWait):
			d = tempWait[ti]
			ti++
		default:
			return zero, err
		}
		if serr := sleep(ctx, d); serr != nil {
			return zero, serr
		}
	}
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
	r.logger.Warn("step failed", "run_id", runID, "error", err)
	r.update(runID, func(s *store.RunState) {
		s.LastError = err.Error()
		s.Errors++
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
	r.logger.Info("run finished", "run_id", runID, "reason", reason, "error", lastErr)
}

// pairDeck deals ordered (content, style) pairs of two different images in
// random order. Every ordered pair comes up once before any repeats; then
// the deck is reshuffled.
type pairDeck struct {
	items []string
	rng   *rand.Rand
	deck  [][2]string
}

func (d *pairDeck) next() [2]string {
	if len(d.deck) == 0 {
		for i := range d.items {
			for j := range d.items {
				if i != j {
					d.deck = append(d.deck, [2]string{d.items[i], d.items[j]})
				}
			}
		}
		d.rng.Shuffle(len(d.deck), func(i, j int) { d.deck[i], d.deck[j] = d.deck[j], d.deck[i] })
	}
	p := d.deck[len(d.deck)-1]
	d.deck = d.deck[:len(d.deck)-1]
	return p
}

// ListSourceImages returns sorted paths of .png, .jpg, .jpeg, .webp files.
func ListSourceImages(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".png", ".jpg", ".jpeg", ".webp":
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
