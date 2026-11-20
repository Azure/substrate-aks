package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// Timings records how long guided setup spends in each phase, stage and step.
// Durations come from the monotonic clock: time.Now readings carry it, and
// Sub uses it. The UTC stamps in a report are wall-clock readings, so a
// system clock change during setup shifts the stamps but not the durations.
//
// A nil *Timings records nothing, so standalone commands pay no cost. Every
// method is safe for concurrent use.
//
// Only fixed identifiers reach a report: step IDs are constants or validated
// public image names, and stage IDs and labels come from the fixed setup
// stages. Reports never hold command arguments, error text, or the names of
// the installation, subscription, resource group, or registry.
type Timings struct {
	mu      sync.Mutex
	now     func() time.Time
	started time.Time
	phases  []*phaseTiming
	stage   *stageTiming
	waits   []*timingSpan
	stages  map[string]int
}

type timingSpan struct {
	start, end time.Time
	done       bool
}

type phaseTiming struct {
	id, title string
	timingSpan
	stages []*stageTiming
	steps  []*stepTiming
}

type stageTiming struct {
	id, label, outcome string
	attempt            int
	timingSpan
	steps []*stepTiming
}

type stepTiming struct {
	id, outcome string
	workers     int
	timingSpan
}

// Timing outcomes.
const (
	TimingSucceeded   = "succeeded"
	TimingFailed      = "failed"
	TimingInterrupted = "interrupted"
	// TimingUnfinished marks a stage or step still running when the report
	// was built.
	TimingUnfinished = "unfinished"
)

// invalidTimingID replaces an identifier that does not match timingIDPattern.
const invalidTimingID = "invalid"

var timingIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z0-9][a-z0-9_-]*)*$`)

func timingID(id string) string {
	if !timingIDPattern.MatchString(id) {
		return invalidTimingID
	}
	return id
}

// NewTimings starts a recorder; now is time.Now outside tests.
func NewTimings(now func() time.Time) *Timings {
	return &Timings{now: now, started: now(), stages: map[string]int{}}
}

type timingsKey struct{}

// WithTimings carries t to the installer operations run under ctx.
func WithTimings(ctx context.Context, t *Timings) context.Context {
	return context.WithValue(ctx, timingsKey{}, t)
}

// TimingsFrom returns the recorder carried by ctx, or nil.
func TimingsFrom(ctx context.Context) *Timings {
	t, _ := ctx.Value(timingsKey{}).(*Timings)
	return t
}

// step times one installer operation when ctx carries a recorder.
func (a App) step(ctx context.Context, id string) func(error) {
	return TimingsFrom(ctx).Step(id)
}

// TimingOutcome classifies the result of timed work.
func TimingOutcome(err error) string {
	switch {
	case err == nil:
		return TimingSucceeded
	case errors.Is(err, context.Canceled):
		return TimingInterrupted
	default:
		return TimingFailed
	}
}

// Phase starts phase id, ending the current one, unless id is already
// current. title is the phase's fixed display title.
func (t *Timings) Phase(id, title string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.phase(timingID(id), title)
}

func (t *Timings) phase(id, title string) *phaseTiming {
	if n := len(t.phases); n > 0 && t.phases[n-1].id == id {
		return t.phases[n-1]
	}
	now := t.now()
	if n := len(t.phases); n > 0 {
		t.phases[n-1].end, t.phases[n-1].done = now, true
	}
	p := &phaseTiming{id: id, title: title, timingSpan: timingSpan{start: now}}
	t.phases = append(t.phases, p)
	return p
}

func (t *Timings) currentPhase() *phaseTiming {
	if len(t.phases) == 0 {
		return t.phase("setup", "Setup")
	}
	return t.phases[len(t.phases)-1]
}

// Stage starts one attempt of a setup stage in the current phase. Steps
// started before the returned func is called belong to this attempt.
func (t *Timings) Stage(id, label string) func(error) {
	if t == nil {
		return func(error) {}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	id = timingID(id)
	t.stages[id]++
	s := &stageTiming{id: id, label: label, attempt: t.stages[id], timingSpan: timingSpan{start: t.now()}}
	p := t.currentPhase()
	p.stages = append(p.stages, s)
	t.stage = s
	return func(err error) {
		t.mu.Lock()
		defer t.mu.Unlock()
		if s.done {
			return
		}
		s.end, s.done, s.outcome = t.now(), true, TimingOutcome(err)
		if t.stage == s {
			t.stage = nil
		}
	}
}

// Step starts a timed step in the current stage.
func (t *Timings) Step(id string) func(error) {
	return t.StepWorkers(id, 0)
}

// StepWorkers starts a timed step that runs up to workers operations at once.
func (t *Timings) StepWorkers(id string, workers int) func(error) {
	if t == nil {
		return func(error) {}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := &stepTiming{id: timingID(id), workers: workers, timingSpan: timingSpan{start: t.now()}}
	if t.stage != nil {
		t.stage.steps = append(t.stage.steps, s)
	} else {
		p := t.currentPhase()
		p.steps = append(p.steps, s)
	}
	return func(err error) {
		t.mu.Lock()
		defer t.mu.Unlock()
		if s.done {
			return
		}
		s.end, s.done, s.outcome = t.now(), true, TimingOutcome(err)
	}
}

// Wait marks time spent waiting for the user; active time excludes it.
func (t *Timings) Wait() func() {
	if t == nil {
		return func() {}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	w := &timingSpan{start: t.now()}
	t.waits = append(t.waits, w)
	return func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if !w.done {
			w.end, w.done = t.now(), true
		}
	}
}

// TimingReportVersion is the schema version of a written TimingReport.
const TimingReportVersion = 1

// TimingReport is the setup timing report. Times are RFC 3339 UTC with
// millisecond precision; wall_ms is elapsed time and active_ms excludes
// time spent waiting for the user.
type TimingReport struct {
	Version int    `json:"version"`
	Outcome string `json:"outcome"`
	TimingInterval
	Phases []PhaseTimingReport `json:"phases"`

	ended time.Time
}

// TimingInterval is the measured interval of a report entry.
type TimingInterval struct {
	Started  string `json:"started"`
	Ended    string `json:"ended"`
	WallMS   int64  `json:"wall_ms"`
	ActiveMS int64  `json:"active_ms"`
}

// Wall and Active return the interval's durations.
func (i TimingInterval) Wall() time.Duration   { return time.Duration(i.WallMS) * time.Millisecond }
func (i TimingInterval) Active() time.Duration { return time.Duration(i.ActiveMS) * time.Millisecond }

type PhaseTimingReport struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	TimingInterval
	Stages []StageTimingReport `json:"stages"`
	// Steps lists steps that ran outside any stage.
	Steps []StepTimingReport `json:"steps,omitempty"`
}

type StageTimingReport struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Attempt int    `json:"attempt"`
	Outcome string `json:"outcome"`
	TimingInterval
	Steps []StepTimingReport `json:"steps"`
}

type StepTimingReport struct {
	ID      string `json:"id"`
	Outcome string `json:"outcome"`
	Workers int    `json:"workers,omitempty"`
	TimingInterval
}

// Report summarizes everything recorded so far, ending the run now with the
// given overall outcome. Phases and stages are in run order; steps are
// sorted by start time, then ID.
func (t *Timings) Report(outcome string) TimingReport {
	if t == nil {
		return TimingReport{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	end := t.now()
	waits := mergedWaits(t.waits, end)
	interval := func(s timingSpan) TimingInterval {
		stop := end
		if s.done {
			stop = s.end
		}
		wall := max(stop.Sub(s.start), 0)
		var waited time.Duration
		for _, w := range waits {
			waited += max(minTime(stop, w.end).Sub(maxTime(s.start, w.start)), 0)
		}
		return TimingInterval{
			Started: timingStamp(s.start), Ended: timingStamp(stop),
			WallMS: wall.Milliseconds(), ActiveMS: max(wall-waited, 0).Milliseconds(),
		}
	}
	steps := func(records []*stepTiming) []StepTimingReport {
		sorted := append([]*stepTiming(nil), records...)
		sort.SliceStable(sorted, func(i, j int) bool {
			if !sorted[i].start.Equal(sorted[j].start) {
				return sorted[i].start.Before(sorted[j].start)
			}
			return sorted[i].id < sorted[j].id
		})
		out := make([]StepTimingReport, 0, len(sorted))
		for _, s := range sorted {
			outcome := s.outcome
			if !s.done {
				outcome = TimingUnfinished
			}
			out = append(out, StepTimingReport{ID: s.id, Outcome: outcome, Workers: s.workers, TimingInterval: interval(s.timingSpan)})
		}
		return out
	}
	r := TimingReport{
		Version: TimingReportVersion, Outcome: outcome,
		TimingInterval: interval(timingSpan{start: t.started, end: end, done: true}),
		Phases:         make([]PhaseTimingReport, 0, len(t.phases)), ended: end,
	}
	for _, p := range t.phases {
		phase := PhaseTimingReport{ID: p.id, Title: p.title, TimingInterval: interval(p.timingSpan),
			Stages: make([]StageTimingReport, 0, len(p.stages))}
		if len(p.steps) > 0 {
			phase.Steps = steps(p.steps)
		}
		for _, s := range p.stages {
			outcome := s.outcome
			if !s.done {
				outcome = TimingUnfinished
			}
			phase.Stages = append(phase.Stages, StageTimingReport{ID: s.id, Label: s.label, Attempt: s.attempt,
				Outcome: outcome, TimingInterval: interval(s.timingSpan), Steps: steps(s.steps)})
		}
		r.Phases = append(r.Phases, phase)
	}
	return r
}

// mergedWaits returns the waits as sorted, disjoint intervals; a wait still
// open ends at end.
func mergedWaits(waits []*timingSpan, end time.Time) []timingSpan {
	spans := make([]timingSpan, 0, len(waits))
	for _, w := range waits {
		s := *w
		if !s.done {
			s.end = end
		}
		spans = append(spans, s)
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start.Before(spans[j].start) })
	var merged []timingSpan
	for _, s := range spans {
		if n := len(merged); n > 0 && !s.start.After(merged[n-1].end) {
			merged[n-1].end = maxTime(merged[n-1].end, s.end)
			continue
		}
		merged = append(merged, s)
	}
	return merged
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func timingStamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// Steps returns every step in the report, in report order.
func (r TimingReport) Steps() []StepTimingReport {
	var steps []StepTimingReport
	for _, p := range r.Phases {
		steps = append(steps, p.Steps...)
		for _, s := range p.Stages {
			steps = append(steps, s.Steps...)
		}
	}
	return steps
}

// WriteTimingReport saves r as a private file in the installation's logs
// directory and returns its path. It never creates the installation
// directory: an installation discarded before provisioning has no report.
func WriteTimingReport(c Config, r TimingReport) (string, error) {
	info, err := os.Stat(c.workspace())
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("installation directory %s is not a directory", c.workspace())
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", err
	}
	dir := filepath.Join(c.workspace(), "logs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	ended := r.ended
	if ended.IsZero() {
		ended = time.Now()
	}
	path := filepath.Join(dir, ended.UTC().Format("20060102T150405.000000000")+"-setup-timings.json")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	_, writeErr := file.Write(append(data, '\n'))
	if err := errors.Join(writeErr, file.Close()); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}
