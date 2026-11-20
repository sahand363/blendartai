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
	// TextOnly skips the image model: Target counts descriptions.
	TextOnly bool
	// Suffix is appended to every description.
	Suffix string
	// StyleDir holds the Styles box; StyleFrom picks the style pool:
	// "sources" (default when empty), "styles" or "both".
	StyleDir  string
	StyleFrom string
	// StepDelay waits between steps. LimitWait / LimitRetries: on a rate
	// limit or quota error, wait LimitWait and try again up to LimitRetries
	// times (0 = give up at once). Zero LimitWait uses 15s/30s steps.
	StepDelay    time.Duration
	LimitWait    time.Duration
	LimitRetries int
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

	evMu   sync.Mutex
	events []Event
	evSeq  int64
}

// Event is one line of the live activity log.
type Event struct {
	Seq   int64     `json:"seq"`
	At    time.Time `json:"at"`
	RunID string    `json:"run_id"`
	Step  int       `json:"step,omitempty"`
	Level string    `json:"level"` // info, ok, warn, error
	Text  string    `json:"text"`
}

// maxEvents is how many events stay in memory for the panel.
const maxEvents = 400

func (r *Runner) emit(runID string, step int, level, format string, args ...any) {
	ev := Event{At: time.Now().UTC(), RunID: runID, Step: step, Level: level, Text: fmt.Sprintf(format, args...)}
	r.evMu.Lock()
	r.evSeq++
	ev.Seq = r.evSeq
	r.events = append(r.events, ev)
	if len(r.events) > maxEvents {
		r.events = append([]Event(nil), r.events[len(r.events)-maxEvents:]...)
	}
	r.evMu.Unlock()
	prefix := ""
	if step > 0 {
		prefix = fmt.Sprintf("#%d ", step)
	}
	r.store.AppendEvent(runID, ev.At.Format(time.RFC3339)+" "+strings.ToUpper(level)+" "+prefix+ev.Text)
}

// Events returns the events after seq (oldest first).
func (r *Runner) Events(after int64) []Event {
	r.evMu.Lock()
	defer r.evMu.Unlock()
	out := []Event{}
	for _, ev := range r.events {
		if ev.Seq > after {
			out = append(out, ev)
		}
	}
	return out
}

// setPhase records what the run is doing right now.
func (r *Runner) setPhase(runID, phase, detail string) {
	r.update(runID, func(s *store.RunState) {
		s.Phase, s.PhaseDetail, s.PhaseSince = phase, detail, time.Now().UTC()
	})
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
	if eng.Describer == nil {
		return store.RunState{}, errors.New("pick a describe model first")
	}
	if eng.ImageMaker == nil && !cfg.TextOnly {
		return store.RunState{}, errors.New("pick an image model first, or turn images off")
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
		RunID: runID, Status: "running", MaxImages: cfg.Target, MaxUSD: cfg.MaxUSD, TextOnly: cfg.TextOnly,
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
		r.status.Phase, r.status.PhaseDetail, r.status.PhaseSince = "paused", "waiting for Resume", time.Now().UTC()
		r.status.UpdatedAt = time.Now().UTC()
		snap := r.status
		r.mu.Unlock()
		_ = r.store.WriteRunState(runID, snap)
		r.logger.Info("run paused", "run_id", runID)
		r.emit(runID, 0, "warn", "Paused. Press Resume to go on.")
		select {
		case <-ch:
		case <-ctx.Done():
		}
		r.mu.Lock()
		if !r.stopReq && ctx.Err() == nil {
			r.status.Status = "running"
			r.logger.Info("run resumed", "run_id", runID)
			defer r.emit(runID, 0, "info", "Resumed.")
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
	var styleImgs []string
	if cfg.StyleDir != "" && cfg.StyleFrom != "" && cfg.StyleFrom != "sources" {
		styleImgs, _ = ListSourceImages(cfg.StyleDir)
	}
	var stylePool []string
	switch cfg.StyleFrom {
	case "styles":
		stylePool = styleImgs
	case "both":
		stylePool = append(append([]string{}, sources...), styleImgs...)
	default:
		stylePool = sources
	}
	deck := &pairDeck{contents: sources, styles: stylePool, rng: r.rng}
	if len(sources) == 0 || deck.size() == 0 {
		r.finish(runID, "not_enough_sources", "need a source image and a different style image")
		return
	}
	isStyleDir := map[string]bool{}
	for _, p := range styleImgs {
		isStyleDir[p] = true
	}
	r.logger.Info("run started", "run_id", runID, "sources", len(sources), "target", cfg.Target,
		"describe", cfg.DescribeModel, "image", cfg.ImageModel)
	if cfg.TextOnly {
		r.emit(runID, 0, "info", "Run started: %d source images, %s, target %d descriptions, images off. Describe: %s.",
			len(sources), styleNote(cfg.StyleFrom, len(stylePool)), cfg.Target, cfg.DescribeModel)
	} else {
		r.emit(runID, 0, "info", "Run started: %d source images, %s, target %d images. Describe: %s. Image: %s.",
			len(sources), styleNote(cfg.StyleFrom, len(stylePool)), cfg.Target, cfg.DescribeModel, cfg.ImageModel)
	}
	if cfg.StepDelay > 0 || cfg.LimitWait > 0 {
		r.emit(runID, 0, "info", "Pacing: %s between steps; on a rate limit wait %s and retry, up to %d times.", cfg.StepDelay, cfg.LimitWait, cfg.LimitRetries)
	}
	type loaded struct {
		img   imageprep.Image
		thumb string
	}
	cache := map[string]loaded{}
	load := func(p string) (loaded, error) {
		if l, ok := cache[p]; ok {
			return l, nil
		}
		im, err := eng.Load(p)
		if err != nil {
			return loaded{}, err
		}
		key := filepath.Base(p)
		if isStyleDir[p] {
			key = "style-" + key
		}
		thumb, _ := r.store.SaveRunSource(runID, key, im.Data, im.MIMEType)
		l := loaded{im, thumb}
		cache[p] = l
		return l, nil
	}
	failures := 0
	step := 0

	for {
		if r.checkpoint(ctx, runID) {
			r.finish(runID, "stopped_by_user", "")
			return
		}
		st := r.Status()
		if cfg.TextOnly && st.Descriptions >= cfg.Target {
			r.finish(runID, "target_reached", "")
			return
		}
		if !cfg.TextOnly && st.ImagesDone >= cfg.Target {
			r.finish(runID, "target_reached", "")
			return
		}
		if !cfg.TextOnly && cfg.MaxUSD > 0 && cfg.PricePerImage > 0 && st.EstSpendUSD+cfg.PricePerImage > cfg.MaxUSD+1e-9 {
			r.finish(runID, "max_usd_reached", "")
			return
		}
		if failures >= MaxConsecutiveErrors {
			r.finish(runID, "too_many_errors", st.LastError)
			return
		}

		if rested := step == 0 || cfg.StepDelay <= 0; !rested {
			r.setPhase(runID, "resting", fmt.Sprintf("%s pause between steps", cfg.StepDelay))
			if r.rest(ctx, cfg.StepDelay) {
				r.finish(runID, "stopped_by_user", "")
				return
			}
		}
		step++
		pair := deck.next()
		content, style := filepath.Base(pair[0]), filepath.Base(pair[1])
		label := content + " in the style of " + style
		r.update(runID, func(s *store.RunState) {
			s.CurrentPair, s.Steps, s.CurrentStep = label, step, step
			s.ContentImage, s.StyleImage, s.ContentThumb, s.StyleThumb = content, style, "", ""
			s.Phase, s.PhaseDetail, s.PhaseSince = "loading", "reading the two images", time.Now().UTC()
		})
		r.emit(runID, step, "info", "Picked %s (content) + %s (style).", content, style)

		a, err := load(pair[0])
		if err == nil {
			var b loaded
			b, err = load(pair[1])
			if err == nil {
				r.update(runID, func(s *store.RunState) { s.ContentThumb, s.StyleThumb = a.thumb, b.thumb })
				err = r.doStep(ctx, runID, step, cfg, eng, pair, a.img, b.img, a.thumb, b.thumb)
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
		r.recordError(runID, step, fmt.Errorf("%s: %w", label, err))
	}
}

func (r *Runner) doStep(ctx context.Context, runID string, step int, cfg RunConfig, eng Engine, pair [2]string, imgA, imgB imageprep.Image, thumbA, thumbB string) error {
	content, style := filepath.Base(pair[0]), filepath.Base(pair[1])
	dir, err := r.store.PairDir(runID, step, content, style)
	if err != nil {
		return err
	}
	rec := store.StepRecord{
		Step: step, Dir: filepath.Base(dir), Content: content, Style: style,
		ContentThumb: thumbA, StyleThumb: thumbB, Status: "describing",
		DescribeModel: cfg.DescribeModel, ImageModel: cfg.ImageModel, StartedAt: time.Now().UTC(),
	}
	_ = r.store.WriteStep(runID, rec)
	fail := func(stage string, err error) error {
		rec.Status, rec.Error, rec.FinishedAt = "failed", stage+": "+err.Error(), time.Now().UTC()
		_ = r.store.WriteStep(runID, rec)
		return fmt.Errorf("%s: %w", stage, err)
	}
	onWait := func(d time.Duration, err error) {
		r.setPhase(runID, "waiting", fmt.Sprintf("retry in %s", d))
		r.emit(runID, step, "warn", "Temporary problem (%s), retrying in %s.", shortErr(err), d)
	}

	r.setPhase(runID, "describing", cfg.DescribeModel)
	r.emit(runID, step, "info", "Describing with %s…", cfg.DescribeModel)
	t0 := time.Now()
	desc, err := retry(ctx, r.sleepFn, cfg.limits(), onWait, func() (string, error) {
		r.setPhase(runID, "describing", cfg.DescribeModel)
		return eng.Describer.Describe(ctx, cfg.Prompt, []imageprep.Image{imgA, imgB})
	})
	if err != nil {
		return fail("describe", err)
	}
	rec.DescribeMS = time.Since(t0).Milliseconds()
	desc = strings.TrimSpace(desc)
	if cfg.Suffix != "" {
		desc += " " + cfg.Suffix
	}
	rec.Description, rec.Status = desc, "drawing"
	_ = r.store.WriteStep(runID, rec)
	r.update(runID, func(s *store.RunState) { s.Descriptions++ })
	if err := store.WriteFileAtomic(filepath.Join(dir, "description.txt"), []byte(desc+"\n"), 0o644); err != nil {
		return fail("save", err)
	}
	if cfg.TextOnly {
		rec.Status, rec.FinishedAt = "done", time.Now().UTC()
		_ = r.store.WriteStep(runID, rec)
		r.update(runID, func(s *store.RunState) { s.PairsDone++ })
		r.emit(runID, step, "ok", "Description %d/%d ready (%d words, %.1fs). Images are off.", r.Status().Descriptions, cfg.Target, len(strings.Fields(desc)), float64(rec.DescribeMS)/1000)
		return nil
	}
	r.emit(runID, step, "ok", "Description ready (%d words, %.1fs).", len(strings.Fields(desc)), float64(rec.DescribeMS)/1000)

	type img struct {
		data []byte
		mime string
	}
	r.setPhase(runID, "drawing", cfg.ImageModel)
	r.emit(runID, step, "info", "Drawing with %s…", cfg.ImageModel)
	t1 := time.Now()
	out, err := retry(ctx, r.sleepFn, cfg.limits(), onWait, func() (img, error) {
		r.setPhase(runID, "drawing", cfg.ImageModel)
		d, m, e := eng.ImageMaker.Make(ctx, desc)
		return img{d, m}, e
	})
	if err != nil {
		return fail("image", err)
	}
	if len(out.data) == 0 {
		return fail("image", errors.New("empty answer"))
	}
	rec.DrawMS = time.Since(t1).Milliseconds()
	r.setPhase(runID, "saving", "")
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
	name, err := r.store.WriteImageAndMeta(dir, 1, out.data, out.mime, meta)
	if err != nil {
		return fail("save", err)
	}
	rec.Image = rec.Dir + "/" + name
	rec.Status, rec.FinishedAt = "done", time.Now().UTC()
	_ = r.store.WriteStep(runID, rec)
	r.update(runID, func(s *store.RunState) {
		s.ImagesDone++
		s.PairsDone++
		s.EstSpendUSD += cfg.PricePerImage
	})
	done := r.Status().ImagesDone
	r.emit(runID, step, "ok", "Image %d/%d saved (%.1fs to draw).", done, cfg.Target, float64(rec.DrawMS)/1000)
	return nil
}

func shortErr(err error) string {
	s := err.Error()
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

// limits is the list of waits used on rate-limit errors.
func (c RunConfig) limits() []time.Duration {
	if c.LimitWait <= 0 {
		return []time.Duration{15 * time.Second, 30 * time.Second}
	}
	out := make([]time.Duration, c.LimitRetries)
	for i := range out {
		out[i] = c.LimitWait
	}
	return out
}

func styleNote(from string, n int) string {
	switch from {
	case "styles":
		return fmt.Sprintf("styles from the Styles box (%d)", n)
	case "both":
		return fmt.Sprintf("styles from Sources and Styles (%d)", n)
	}
	return "styles from Sources"
}

// rest waits d between steps, waking early on Stop. It returns true when
// the run should stop.
func (r *Runner) rest(ctx context.Context, d time.Duration) bool {
	end := time.Now().Add(d)
	for {
		r.mu.Lock()
		stop := r.stopReq
		r.mu.Unlock()
		if stop || ctx.Err() != nil {
			return true
		}
		left := time.Until(end)
		if left <= 0 {
			return false
		}
		if err := r.sleepFn(ctx, min(left, 500*time.Millisecond)); err != nil {
			return true
		}
	}
}

// retry tries fn up to 4 times on temporary errors (2s, 4s, 8s) and once
// per entry of quotaWait on rate limits before giving up. onWait (optional)
// is told about each wait.
func retry[T any](ctx context.Context, sleep func(context.Context, time.Duration) error, quotaWait []time.Duration, onWait func(time.Duration, error), fn func() (T, error)) (T, error) {
	var zero T
	tempWait := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}
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
		if onWait != nil {
			onWait(d, err)
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

func (r *Runner) recordError(runID string, step int, err error) {
	r.logger.Warn("step failed", "run_id", runID, "error", err)
	r.emit(runID, step, "error", "Step failed: %s", shortErr(err))
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
		s.Phase, s.PhaseDetail, s.PhaseSince = "", "", time.Now().UTC()
		if lastErr != "" {
			s.LastError = lastErr
		}
	})
	r.logger.Info("run finished", "run_id", runID, "reason", reason, "error", lastErr)
	lvl := "ok"
	if reason != "target_reached" {
		lvl = "warn"
	}
	msg := "Run finished: " + strings.ReplaceAll(reason, "_", " ") + "."
	if lastErr != "" {
		msg += " " + shortErr(errors.New(lastErr))
	}
	r.emit(runID, 0, lvl, "%s", msg)
}

// pairDeck deals (content, style) pairs of two different images in random
// order: content from contents, style from styles. Every pair comes up once
// before any repeats; then the deck is reshuffled.
type pairDeck struct {
	contents []string
	styles   []string
	rng      *rand.Rand
	deck     [][2]string
}

func (d *pairDeck) all() [][2]string {
	var out [][2]string
	for _, c := range d.contents {
		for _, s := range d.styles {
			if c != s {
				out = append(out, [2]string{c, s})
			}
		}
	}
	return out
}

func (d *pairDeck) size() int { return len(d.all()) }

func (d *pairDeck) next() [2]string {
	if len(d.deck) == 0 {
		d.deck = d.all()
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
