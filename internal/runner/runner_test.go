package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sahand363/blendartai/internal/imageprep"
	"github.com/sahand363/blendartai/internal/providers"
	"github.com/sahand363/blendartai/internal/store"
)

type fakeDesc struct {
	calls atomic.Int32
	err   error
	gate  chan struct{}
}

func (f *fakeDesc) Describe(ctx context.Context, instr string, imgs []imageprep.Image) (string, error) {
	f.calls.Add(1)
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if f.err != nil {
		return "", f.err
	}
	if len(imgs) != 2 || string(imgs[0].Data) == string(imgs[1].Data) {
		return "", errors.New("want two different images")
	}
	return "a cat painted in watercolor", nil
}

type fakeMaker struct{ prompts atomic.Int32 }

func (f *fakeMaker) Make(ctx context.Context, prompt string) ([]byte, string, error) {
	f.prompts.Add(1)
	if prompt != "a cat painted in watercolor" {
		return nil, "", errors.New("image model got the wrong prompt")
	}
	return []byte("\x89PNG\r\n\x1a\nfake"), "image/png", nil
}

func setup(t *testing.T, n int) (*Runner, string, *store.Store) {
	t.Helper()
	root := t.TempDir()
	src := filepath.Join(root, "src")
	os.MkdirAll(src, 0o755)
	for i := 0; i < n; i++ {
		os.WriteFile(filepath.Join(src, string(rune('a'+i))+".png"), []byte{byte(i)}, 0o644)
	}
	st, err := store.New(filepath.Join(root, "out"))
	if err != nil {
		t.Fatal(err)
	}
	r := New(st, nil)
	r.sleepFn = func(ctx context.Context, d time.Duration) error { return ctx.Err() }
	return r, src, st
}

func fakeLoad(p string) (imageprep.Image, error) {
	return imageprep.Image{Data: []byte(filepath.Base(p)), MIMEType: "image/png"}, nil
}

func TestRunReachesTarget(t *testing.T) {
	r, src, st := setup(t, 4)
	d, m := &fakeDesc{}, &fakeMaker{}
	if _, err := r.Start(RunConfig{SourceDir: src, Target: 7, PricePerImage: 0.04, Prompt: "x"}, Engine{Describer: d, ImageMaker: m, Load: fakeLoad}); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	s := r.Status()
	if s.StopReason != "target_reached" || s.ImagesDone != 7 || s.Descriptions != 7 {
		t.Fatalf("got %+v", s)
	}
	runs, _ := st.ListRuns()
	if len(runs) != 1 || len(runs[0].Pairs) != 7 {
		t.Fatalf("want 7 saved steps, got %+v", runs)
	}
	f, err := st.ResolveRunFile(s.RunID, runs[0].Pairs[0].Name+"/description.txt")
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(f); err != nil || len(b) == 0 {
		t.Fatalf("description not saved: %v", err)
	}
}

func TestSpendGuard(t *testing.T) {
	r, src, _ := setup(t, 3)
	r.Start(RunConfig{SourceDir: src, Target: 100, PricePerImage: 0.5, MaxUSD: 1.2}, Engine{Describer: &fakeDesc{}, ImageMaker: &fakeMaker{}, Load: fakeLoad})
	r.Wait()
	if s := r.Status(); s.StopReason != "max_usd_reached" || s.ImagesDone != 2 {
		t.Fatalf("got %+v", s)
	}
}

func TestQuotaStops(t *testing.T) {
	r, src, _ := setup(t, 3)
	d := &fakeDesc{err: &providers.APIError{Status: 429, Body: "quota"}}
	r.Start(RunConfig{SourceDir: src, Target: 10}, Engine{Describer: d, ImageMaker: &fakeMaker{}, Load: fakeLoad})
	r.Wait()
	if s := r.Status(); s.StopReason != "quota_exhausted" {
		t.Fatalf("got %+v", s)
	}
}

func TestTooManyErrors(t *testing.T) {
	r, src, _ := setup(t, 3)
	d := &fakeDesc{err: &providers.APIError{Status: 400, Body: "bad"}}
	r.Start(RunConfig{SourceDir: src, Target: 10}, Engine{Describer: d, ImageMaker: &fakeMaker{}, Load: fakeLoad})
	r.Wait()
	if s := r.Status(); s.StopReason != "too_many_errors" || s.Errors != MaxConsecutiveErrors {
		t.Fatalf("got %+v", s)
	}
}

func TestPauseResumeStop(t *testing.T) {
	r, src, _ := setup(t, 3)
	d := &fakeDesc{gate: make(chan struct{})}
	r.Start(RunConfig{SourceDir: src, Target: 50}, Engine{Describer: d, ImageMaker: &fakeMaker{}, Load: fakeLoad})
	waitFor(t, func() bool { return d.calls.Load() >= 1 }) // a step is in flight
	if err := r.Pause(); err != nil {
		t.Fatal(err)
	}
	d.gate <- struct{}{} // finish the step in flight
	waitFor(t, func() bool { return r.Status().Status == "paused" })
	done := r.Status().ImagesDone
	time.Sleep(50 * time.Millisecond)
	if r.Status().ImagesDone != done {
		t.Fatal("made images while paused")
	}
	r.Resume()
	d.gate <- struct{}{}
	waitFor(t, func() bool { return r.Status().ImagesDone > done })
	r.Stop()
	close(d.gate)
	r.Wait()
	if s := r.Status(); s.StopReason != "stopped_by_user" {
		t.Fatalf("got %+v", s)
	}
}

func TestNeedsTwoSources(t *testing.T) {
	r, src, _ := setup(t, 1)
	r.Start(RunConfig{SourceDir: src, Target: 5}, Engine{Describer: &fakeDesc{}, ImageMaker: &fakeMaker{}, Load: fakeLoad})
	r.Wait()
	if s := r.Status(); s.StopReason != "not_enough_sources" {
		t.Fatalf("got %+v", s)
	}
}

func TestPairDeckOrderedNoSelf(t *testing.T) {
	d := &pairDeck{contents: []string{"a", "b", "c"}, styles: []string{"a", "b", "c"}, rng: New(nil, nil).rng}
	seen := map[[2]string]int{}
	for i := 0; i < 6; i++ {
		p := d.next()
		if p[0] == p[1] {
			t.Fatal("paired an image with itself")
		}
		seen[p]++
	}
	if len(seen) != 6 {
		t.Fatalf("want all 6 ordered pairs once, got %v", seen)
	}
}

func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out")
}

func TestStepsAndEvents(t *testing.T) {
	r, src, st := setup(t, 3)
	r.Start(RunConfig{SourceDir: src, Target: 4, Prompt: "x", DescribeModel: "d", ImageModel: "i"}, Engine{Describer: &fakeDesc{}, ImageMaker: &fakeMaker{}, Load: fakeLoad})
	r.Wait()
	s := r.Status()
	steps, err := st.ListSteps(s.RunID)
	if err != nil || len(steps) != 4 {
		t.Fatalf("want 4 steps, got %d (%v)", len(steps), err)
	}
	for _, x := range steps {
		if x.Status != "done" || x.Description != "a cat painted in watercolor" || x.Image == "" ||
			x.Content == x.Style || x.ContentThumb == "" || x.StyleThumb == "" {
			t.Fatalf("bad step %+v", x)
		}
		if _, err := st.ResolveRunFile(s.RunID, x.ContentThumb); err != nil {
			t.Fatal(err)
		}
	}
	if steps[0].Step != 4 {
		t.Fatalf("want newest first, got %d", steps[0].Step)
	}
	evs := r.Events(0)
	if len(evs) < 4*4 || evs[len(evs)-1].Text != "Run finished: target reached." {
		t.Fatalf("events: %d, last %+v", len(evs), evs[len(evs)-1])
	}
	if later := r.Events(evs[len(evs)-2].Seq); len(later) != 1 {
		t.Fatalf("after filter: %d", len(later))
	}
	runs, _ := st.ListRuns()
	if len(runs[0].Pairs) != 4 {
		t.Fatalf("sources folder leaked into the gallery: %+v", runs[0].Pairs)
	}
}

func TestFailedStepRecorded(t *testing.T) {
	r, src, st := setup(t, 2)
	d := &fakeDesc{err: &providers.APIError{Status: 400, Body: "bad"}}
	r.Start(RunConfig{SourceDir: src, Target: 3}, Engine{Describer: d, ImageMaker: &fakeMaker{}, Load: fakeLoad})
	r.Wait()
	steps, _ := st.ListSteps(r.Status().RunID)
	if len(steps) != MaxConsecutiveErrors || steps[0].Status != "failed" || steps[0].Error == "" {
		t.Fatalf("got %+v", steps)
	}
}

func TestTextOnly(t *testing.T) {
	r, src, st := setup(t, 4)
	d := &fakeDesc{}
	cfg := RunConfig{SourceDir: src, Target: 6, PricePerImage: 0.04, MaxUSD: 0.05, Prompt: "x", TextOnly: true, Suffix: "Pure black background."}
	if _, err := r.Start(cfg, Engine{Describer: d, Load: fakeLoad}); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	s := r.Status()
	if s.StopReason != "target_reached" || s.Descriptions != 6 || s.ImagesDone != 0 || s.EstSpendUSD != 0 {
		t.Fatalf("got %+v", s)
	}
	steps, err := st.ListSteps(s.RunID)
	if err != nil || len(steps) != 6 {
		t.Fatalf("steps %d %v", len(steps), err)
	}
	for _, x := range steps {
		if x.Status != "done" || x.Image != "" || x.Description != "a cat painted in watercolor Pure black background." {
			t.Fatalf("step %+v", x)
		}
	}
}

func TestStylesBox(t *testing.T) {
	r, src, st := setup(t, 2)
	sty := filepath.Join(t.TempDir(), "styles")
	os.MkdirAll(sty, 0o755)
	os.WriteFile(filepath.Join(sty, "s1.png"), []byte{9}, 0o644)
	os.WriteFile(filepath.Join(sty, "s2.png"), []byte{8}, 0o644)
	cfg := RunConfig{SourceDir: src, StyleDir: sty, StyleFrom: "styles", Target: 4, Prompt: "x", TextOnly: true}
	if _, err := r.Start(cfg, Engine{Describer: &fakeDesc{}, Load: fakeLoad}); err != nil {
		t.Fatal(err)
	}
	r.Wait()
	steps, _ := st.ListSteps(r.Status().RunID)
	if len(steps) != 4 {
		t.Fatalf("steps %d %+v", len(steps), r.Status())
	}
	seen := map[string]bool{}
	for _, x := range steps {
		if x.Style != "s1.png" && x.Style != "s2.png" || x.Content != "a.png" && x.Content != "b.png" {
			t.Fatalf("bad pair %s + %s", x.Content, x.Style)
		}
		seen[x.Content+x.Style] = true
	}
	if len(seen) != 4 {
		t.Fatalf("pairs repeated before deck ran out: %v", seen)
	}
}

func TestLimitWaits(t *testing.T) {
	r, src, _ := setup(t, 3)
	var waits atomic.Int32
	r.sleepFn = func(ctx context.Context, d time.Duration) error {
		if d == 90*time.Second {
			waits.Add(1)
		}
		return ctx.Err()
	}
	d := &fakeDesc{err: &providers.APIError{Status: 429, Body: "rate limit"}}
	cfg := RunConfig{SourceDir: src, Target: 2, Prompt: "x", TextOnly: true, LimitWait: 90 * time.Second, LimitRetries: 4}
	r.Start(cfg, Engine{Describer: d, Load: fakeLoad})
	r.Wait()
	if s := r.Status(); s.StopReason != "quota_exhausted" || waits.Load() != 4 || d.calls.Load() != 5 {
		t.Fatalf("reason %s waits %d calls %d", s.StopReason, waits.Load(), d.calls.Load())
	}
}
