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
	// running counts background stages still running; superseded marks a
	// phase that a later phase replaced while they ran. Such a phase ends
	// when its last background stage stops.
	running    int
	superseded bool
}

type stageTiming struct {
	id, label, outcome string
	attempt            int
	// background marks a stage that ran alongside the foreground stages.
	background bool
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

type timingStageKey struct{}

// step times one installer operation when ctx carries a recorder.
func (a App) step(ctx context.Context, id string) func(error) {
	return timingStep(ctx, id, 0)
}

// timingStep starts a step in the stage ctx is bound to, or else in the
// current stage.
func timingStep(ctx context.Context, id string, workers int) func(error) {
	t := TimingsFrom(ctx)
	if t == nil {
		return func(error) {}
	}
	s, _ := ctx.Value(timingStageKey{}).(*stageTiming)
	return t.startStep(s, id, workers)
}

// TimingOutcome classifies the result of timed work. Work is interrupted only
// when every cause of err is a cancellation; a failure joined with
// cancellations is a failure.
func TimingOutcome(err error) string {
	switch {
	case err == nil:
		return TimingSucceeded
	case Interrupted(err):
		return TimingInterrupted
	default:
		return TimingFailed
	}
}

// Interrupted reports whether err only records a cancellation: every leaf of
// its wrapped and joined errors is context.Canceled. errors.Is would also
// match a real failure joined with the cancellations beside it. A deadline is
// not an interruption: a step that runs out of time failed.
func Interrupted(err error) bool {
	return OnlyCauses(err, context.Canceled)
}

// OnlyCauses reports whether err is non-nil and every leaf of its wrapped and
// joined errors matches one of causes.
func OnlyCauses(err error, causes ...error) bool {
	switch e := err.(type) {
	case nil:
		return false
	case interface{ Unwrap() []error }:
		errs := e.Unwrap()
		for _, err := range errs {
			if !OnlyCauses(err, causes...) {
				return false
			}
		}
		return len(errs) > 0
	case interface{ Unwrap() error }:
		if inner := e.Unwrap(); inner != nil {
			return OnlyCauses(inner, causes...)
		}
	}
	for _, cause := range causes {
		if errors.Is(err, cause) {
			return true
		}
	}
	return false
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
		if prev := t.phases[n-1]; prev.running > 0 {
			prev.superseded = true
		} else {
			prev.end, prev.done = now, true
		}
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
	_, stop := t.StageContext(context.Background(), id, label)
	return stop
}

// StageContext is Stage, also returning ctx bound to the attempt: installer
// steps timed under the returned context belong to it even while a stage in
// another goroutine is current.
func (t *Timings) StageContext(ctx context.Context, id, label string) (context.Context, func(error)) {
	return t.startStage(ctx, id, label, false)
}

// BackgroundStage starts one attempt of a stage that runs alongside the
// foreground stages. It belongs to the current phase but never becomes the
// current stage, so only steps timed under the returned context attach to
// it. The phase stays open until its last background stage stops, even
// after a later phase starts. Its active time ignores waits for the user,
// since its work continues while a prompt waits.
func (t *Timings) BackgroundStage(ctx context.Context, id, label string) (context.Context, func(error)) {
	return t.startStage(ctx, id, label, true)
}

func (t *Timings) startStage(ctx context.Context, id, label string, background bool) (context.Context, func(error)) {
	if t == nil {
		return ctx, func(error) {}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	id = timingID(id)
	t.stages[id]++
	s := &stageTiming{id: id, label: label, attempt: t.stages[id], background: background, timingSpan: timingSpan{start: t.now()}}
	p := t.currentPhase()
	p.stages = append(p.stages, s)
	if background {
		p.running++
	} else {
		t.stage = s
	}
	return context.WithValue(ctx, timingStageKey{}, s), func(err error) {
		t.mu.Lock()
		defer t.mu.Unlock()
		if s.done {
			return
		}
		now := t.now()
		s.end, s.done, s.outcome = now, true, TimingOutcome(err)
		if t.stage == s {
			t.stage = nil
		}
		if background {
			if p.running--; p.running == 0 && p.superseded {
				p.end, p.done = now, true
			}
		}
	}
}

// Step starts a timed step in the current stage.
func (t *Timings) Step(id string) func(error) {
	return t.StepWorkers(id, 0)
}

// StepWorkers starts a timed step that runs up to workers operations at once.
func (t *Timings) StepWorkers(id string, workers int) func(error) {
	return t.startStep(nil, id, workers)
}

// startStep starts a step in stage, or in the current stage when stage is
// nil, or in the current phase outside any stage.
func (t *Timings) startStep(stage *stageTiming, id string, workers int) func(error) {
	if t == nil {
		return func(error) {}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := &stepTiming{id: timingID(id), workers: workers, timingSpan: timingSpan{start: t.now()}}
	if stage == nil {
		stage = t.stage
	}
	if stage != nil {
		stage.steps = append(stage.steps, s)
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
// Version 2 adds concurrent_with and overlap_ms to phases and background to
// stages: a phase's wall time may overlap later phases, which version 1
// reports never did. Every version 1 field keeps its meaning.
const TimingReportVersion = 2

// TimingReport is the setup timing report. Times are RFC 3339 UTC with
// millisecond precision; wall_ms is elapsed time and active_ms excludes
// time spent waiting for the user, except in background stages and the
// overlapped part of a phase, whose work continued during the wait.
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
	// ConcurrentWith lists the later phases that ran while this phase's
	// background stages were still running.
	ConcurrentWith []string `json:"concurrent_with,omitempty"`
	// OverlapMS is the part of wall_ms that overlaps later phases. Summing
	// wall_ms - overlap_ms over all phases counts no time twice.
	OverlapMS int64               `json:"overlap_ms,omitempty"`
	Stages    []StageTimingReport `json:"stages"`
	// Steps lists steps that ran outside any stage.
	Steps []StepTimingReport `json:"steps,omitempty"`
}

type StageTimingReport struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Attempt int    `json:"attempt"`
	Outcome string `json:"outcome"`
	// Background marks a stage that ran alongside the foreground stages.
	Background bool `json:"background,omitempty"`
	TimingInterval
	Steps []StepTimingReport `json:"steps"`
}

// Overlap returns the part of the phase's wall time that overlaps later
// phases.
func (p PhaseTimingReport) Overlap() time.Duration {
	return time.Duration(p.OverlapMS) * time.Millisecond
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
	stopOf := func(s timingSpan) time.Time {
		if s.done {
			return s.end
		}
		return end
	}
	// measure subtracts only the waits before waitsUntil from active time.
	measure := func(s timingSpan, waitsUntil time.Time) TimingInterval {
		stop := stopOf(s)
		wall := max(stop.Sub(s.start), 0)
		waitsUntil = minTime(stop, waitsUntil)
		var waited time.Duration
		for _, w := range waits {
			waited += max(minTime(waitsUntil, w.end).Sub(maxTime(s.start, w.start)), 0)
		}
		return TimingInterval{
			Started: timingStamp(s.start), Ended: timingStamp(stop),
			WallMS: wall.Milliseconds(), ActiveMS: max(wall-waited, 0).Milliseconds(),
		}
	}
	interval := func(s timingSpan, background bool) TimingInterval {
		if background {
			return measure(s, s.start)
		}
		return measure(s, stopOf(s))
	}
	steps := func(records []*stepTiming, background bool) []StepTimingReport {
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
			out = append(out, StepTimingReport{ID: s.id, Outcome: outcome, Workers: s.workers, TimingInterval: interval(s.timingSpan, background)})
		}
		return out
	}
	r := TimingReport{
		Version: TimingReportVersion, Outcome: outcome,
		TimingInterval: interval(timingSpan{start: t.started, end: end, done: true}, false),
		Phases:         make([]PhaseTimingReport, 0, len(t.phases)), ended: end,
	}
	for i, p := range t.phases {
		// Phases start when the previous one hands over, so a phase that
		// outlives the next one's start overlaps every later phase from
		// then on; waits count against it only until then.
		stop, handover := stopOf(p.timingSpan), stopOf(p.timingSpan)
		var concurrent []string
		if i+1 < len(t.phases) {
			handover = minTime(stop, t.phases[i+1].start)
			for _, later := range t.phases[i+1:] {
				if later.start.Before(stop) {
					concurrent = append(concurrent, later.id)
				}
			}
		}
		phase := PhaseTimingReport{ID: p.id, Title: p.title, TimingInterval: measure(p.timingSpan, handover),
			ConcurrentWith: concurrent, OverlapMS: stop.Sub(handover).Milliseconds(),
			Stages: make([]StageTimingReport, 0, len(p.stages))}
		if len(p.steps) > 0 {
			phase.Steps = steps(p.steps, false)
		}
		for _, s := range p.stages {
			outcome := s.outcome
			if !s.done {
				outcome = TimingUnfinished
			}
			phase.Stages = append(phase.Stages, StageTimingReport{ID: s.id, Label: s.label, Attempt: s.attempt,
				Outcome: outcome, Background: s.background, TimingInterval: interval(s.timingSpan, s.background),
				Steps: steps(s.steps, s.background)})
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
