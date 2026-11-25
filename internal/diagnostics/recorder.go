package diagnostics

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/bits"
	"sync"
	"sync/atomic"
)

const defaultSampleQueueSize = 256

// Logarithmic histogram: 8 buckets per octave (about 9% granularity), covering 1 ns to 2^63 ns.
const (
	latencyBucketsPerOctave = 8
	latencyOctaves          = 63
	latencyBucketCount      = latencyOctaves * latencyBucketsPerOctave
)

// latencyHistogram is a bounded histogram of callback durations for one event kind. The recording path is only a
// few atomic operations, and neither blocks nor allocates memory;
// percentiles are computed when a snapshot is read and take the upper bound of the containing bucket, so they are
// slightly high (by at most about 9%).
type latencyHistogram struct {
	counts     [latencyBucketCount]atomic.Uint64
	maxNS      atomic.Uint64
	overBudget atomic.Uint64
}

// latencySubBucket maps the top 5 bits of the mantissa (32 to 63) to the bucket index within the octave,
// equivalent to floor(8·log2(mantissa/32)).
var latencySubBucket = func() (table [32]int) {
	for i := range table {
		table[i] = int(float64(latencyBucketsPerOctave) * math.Log2(float64(i+32)/32))
	}
	return
}()

// latencyOctaveStep[sub] is the ratio of bucket sub's upper bound to the octave's lower bound, that is, 2^(sub/8).
var latencyOctaveStep = func() (steps [latencyBucketsPerOctave + 1]float64) {
	for sub := range steps {
		steps[sub] = math.Pow(2, float64(sub)/latencyBucketsPerOctave)
	}
	return
}()

// latencyBucket maps a duration (nanoseconds) to a bucket: 0 lands in the smallest bucket; other values locate the
// octave by floor(log2), then subdivide it with a table lookup on the mantissa.
func latencyBucket(ns uint64) int {
	if ns == 0 {
		return 0
	}
	exp := bits.Len64(ns) - 1
	const mantissaBits = 5
	var mantissa uint64
	if exp >= mantissaBits {
		mantissa = ns >> (exp - mantissaBits)
	} else {
		mantissa = ns << (mantissaBits - exp)
	}
	sub := latencySubBucket[mantissa-32]
	// The mantissa keeps only 5 bits (relative error under 3%, smaller than the bucket width), so a value near a bucket
	// boundary may land one bucket too low; correct it once using the real duration.
	if float64(ns) >= math.Ldexp(latencyOctaveStep[sub+1], exp) {
		sub++
	}
	idx := exp*latencyBucketsPerOctave + sub
	if idx >= latencyBucketCount {
		idx = latencyBucketCount - 1
	}
	return idx
}

// latencyBucketUpperNS returns the upper bound of a bucket: bucket j of octave e covers
// [2^e·2^(j/8), 2^e·2^((j+1)/8)).
func latencyBucketUpperNS(idx int) uint64 {
	exp, sub := idx/latencyBucketsPerOctave, idx%latencyBucketsPerOctave
	return uint64(math.Ceil(math.Ldexp(latencyOctaveStep[sub+1], exp)))
}

func (h *latencyHistogram) record(elapsedNS uint64, overBudget bool) {
	h.counts[latencyBucket(elapsedNS)].Add(1)
	if elapsedNS > h.maxNS.Load() {
		h.maxNS.Store(elapsedNS)
	}
	if overBudget {
		h.overBudget.Add(1)
	}
}

// percentile returns the upper bound of the bucket that contains the p-th percentile; total is the total sample count.
func (h *latencyHistogram) percentile(total uint64, p float64) uint64 {
	target := uint64(math.Ceil(p * float64(total)))
	if target == 0 {
		target = 1
	}
	var cumulative uint64
	for i := range h.counts {
		cumulative += h.counts[i].Load()
		if cumulative >= target {
			return latencyBucketUpperNS(i)
		}
	}
	return latencyBucketUpperNS(latencyBucketCount - 1)
}

func (h *latencyHistogram) count() uint64 {
	var total uint64
	for i := range h.counts {
		total += h.counts[i].Load()
	}
	return total
}

func (h *latencyHistogram) stat() LatencyStat {
	s := LatencyStat{
		Count:      h.count(),
		MaxNS:      h.maxNS.Load(),
		OverBudget: h.overBudget.Load(),
	}
	if s.Count > 0 {
		s.P50NS = h.percentile(s.Count, 0.50)
		s.P95NS = h.percentile(s.Count, 0.95)
	}
	return s
}

// EventSample is a strictly redacted observation/debug sample (no raw file names, URLs, paths or coordinates).
type EventSample struct {
	Mode           string // "observe" | "debug"
	GestureID      uint64
	Stage          string // "down" | "up" | "dragged"
	ClickCount     int
	Modifiers      string
	PID            int32
	ProcessKind    string // "dock" | "finder" | "other" | "none"
	AXRole         string
	AXSubrole      string
	ParentChain    string
	Container      string // Finder hits only: "desktop_icon" | "desktop_background" | "finder_window" | "finder_other"
	Selection      string // Finder hits only: "single_self" | "single_other" | "none" | "multi" | "unknown"
	URLKind        string // "finder_app" | "other_app" | "file_url" | "non_file_url" | "none"
	Classification string // "dock_finder" | "dock_other" | "desktop_icon" | "desktop_background" | "finder_window" | "finder_element" | "other_app" | "off_dock" | "covered" | "target_changed" | "ax_error" | "budget_exceeded"
	Decision       string // "observe" | "pass" | "consume" | "consume+activate_app" | "cancel_up+activate_app" | "consume+open_directory" | "cancel_desktop_up+open_directory"
	Source         string // "system" | "off_dock" | "covered" | "same" | "changed": source of the observation, see darwin.HitSource
	ElapsedUS      uint64
	InBudget       bool
}

// logEntry is one asynchronously enqueued log entry: either a redacted event sample or a status log containing only
// the feature, state and reason code.
type logEntry struct {
	sample *EventSample

	level     slog.Level
	msg       string
	feature   string
	state     string
	detailKey string
	detail    string
}

// LatencyStat is the callback duration statistics for one event kind (plain or candidate); percentiles take the
// upper bound of the containing histogram bucket, in nanoseconds.
type LatencyStat struct {
	Count      uint64
	P50NS      uint64
	P95NS      uint64
	MaxNS      uint64
	OverBudget uint64
}

// MetricsSnapshot summarizes the lock-free runtime performance and reliability metrics.
type MetricsSnapshot struct {
	OrdinaryEvents       uint64
	CandidateEvents      uint64
	MaxCallbackNS        uint64
	BudgetExceededCount  uint64
	OrdinaryLatency      LatencyStat
	CandidateLatency     LatencyStat
	DroppedDiagSamples   uint64
	ActionsSubmitted     uint64
	ActionsSucceeded     uint64
	ActionsCompensated   uint64
	ActionsTimedOut      uint64
	PairingsInterrupted  uint64
	TapTimeoutRecoveries uint64
}

// Recorder provides non-blocking metric counters and bounded asynchronous redacted log output, guaranteeing that
// the tap thread never blocks on I/O.
// A zero-value Recorder records nothing: all methods can be called directly on nil.
type Recorder struct {
	logger   *slog.Logger
	minLevel slog.Level

	mu        sync.Mutex
	closed    bool
	entryCh   chan logEntry
	doneCh    chan struct{}
	closeOnce sync.Once

	ordinary             latencyHistogram
	candidate            latencyHistogram
	droppedSamples       atomic.Uint64
	pendingDroppedReport atomic.Uint64
	actionsSubmitted     atomic.Uint64
	actionsSucceeded     atomic.Uint64
	actionsCompensated   atomic.Uint64
	actionsTimedOut      atomic.Uint64
	pairingsInterrupted  atomic.Uint64
	tapTimeoutRecoveries atomic.Uint64
}

// NewRecorder creates an asynchronous redacted log and metrics recorder.
func NewRecorder(w io.Writer, level LogLevel, queueCap int) *Recorder {
	if w == nil {
		w = io.Discard
	}
	if queueCap <= 0 {
		queueCap = defaultSampleQueueSize
	}
	minLevel := toSlogLevel(level)
	handler := slog.NewTextHandler(w, &slog.HandlerOptions{Level: minLevel})
	r := &Recorder{
		logger:   slog.New(handler),
		minLevel: minLevel,
		entryCh:  make(chan logEntry, queueCap),
		doneCh:   make(chan struct{}),
	}
	go r.drainLoop()
	return r
}

func toSlogLevel(lvl LogLevel) slog.Level {
	switch lvl {
	case LogDebug:
		return slog.LevelDebug
	case LogInfo:
		return slog.LevelInfo
	case LogError:
		return slog.LevelError
	default:
		return slog.LevelWarn
	}
}

// DebugEnabled reports whether per-candidate-event redacted debug output is currently enabled.
func (r *Recorder) DebugEnabled() bool {
	return r != nil && r.minLevel <= slog.LevelDebug
}

// RecordCallbackDuration records one callback duration: plain events and candidate events each go into their own
// bounded logarithmic histogram.
// It performs only a few atomic operations, neither blocks nor allocates memory, and can be called per event on the
// tap thread.
func (r *Recorder) RecordCallbackDuration(isCandidate bool, elapsedNS uint64, overBudget bool) {
	if r == nil {
		return
	}
	if isCandidate {
		r.candidate.record(elapsedNS, overBudget)
	} else {
		r.ordinary.record(elapsedNS, overBudget)
	}
}

func (r *Recorder) IncActionSubmitted() {
	if r != nil {
		r.actionsSubmitted.Add(1)
	}
}
func (r *Recorder) IncActionSucceeded() {
	if r != nil {
		r.actionsSucceeded.Add(1)
	}
}
func (r *Recorder) IncActionCompensated() {
	if r != nil {
		r.actionsCompensated.Add(1)
	}
}
func (r *Recorder) IncActionTimedOut() {
	if r != nil {
		r.actionsTimedOut.Add(1)
	}
}
func (r *Recorder) IncPairingInterrupted() {
	if r != nil {
		r.pairingsInterrupted.Add(1)
	}
}
func (r *Recorder) IncTapTimeoutRecovery() {
	if r != nil {
		r.tapTimeoutRecoveries.Add(1)
	}
}

func (r *Recorder) enqueue(e logEntry) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return false
	}
	select {
	case r.entryCh <- e:
		r.mu.Unlock()
		return true
	default:
		r.mu.Unlock()
		r.droppedSamples.Add(1)
		r.pendingDroppedReport.Add(1)
		return false
	}
}

// EnqueueSample enqueues one redacted observation or debug sample without blocking; when the queue is full, the
// sample is counted and dropped.
func (r *Recorder) EnqueueSample(s EventSample) bool {
	return r.enqueue(logEntry{sample: &s})
}

// Warn asynchronously enqueues a warning log containing only the feature, state and error category.
func (r *Recorder) Warn(feature, state, reason string) {
	r.log(slog.LevelWarn, "findrelay warning", feature, state, "reason", reason)
}

// Error asynchronously enqueues an error log containing only the feature, state and error category.
func (r *Recorder) Error(feature, state, reason string) {
	r.log(slog.LevelError, "findrelay error", feature, state, "reason", reason)
}

// Info asynchronously enqueues a lifecycle notice log.
func (r *Recorder) Info(feature, state, detail string) {
	r.log(slog.LevelInfo, "findrelay status", feature, state, "detail", detail)
}

// Debug asynchronously enqueues a redacted status log that is visible only at the --debug level.
func (r *Recorder) Debug(feature, state, detail string) {
	r.log(slog.LevelDebug, "findrelay status", feature, state, "detail", detail)
}

// log enqueues one status log; logs below the current level are dropped directly on the tap thread and do not
// occupy the queue.
func (r *Recorder) log(level slog.Level, msg, feature, state, key, detail string) {
	if r == nil || level < r.minLevel {
		return
	}
	r.enqueue(logEntry{level: level, msg: msg, feature: feature, state: state, detailKey: key, detail: detail})
}

// Snapshot returns a snapshot of the current cumulative metrics.
func (r *Recorder) Snapshot() MetricsSnapshot {
	if r == nil {
		return MetricsSnapshot{}
	}
	ordinary, candidate := r.ordinary.stat(), r.candidate.stat()
	return MetricsSnapshot{
		OrdinaryEvents:       ordinary.Count,
		CandidateEvents:      candidate.Count,
		MaxCallbackNS:        max(ordinary.MaxNS, candidate.MaxNS),
		BudgetExceededCount:  ordinary.OverBudget + candidate.OverBudget,
		OrdinaryLatency:      ordinary,
		CandidateLatency:     candidate,
		DroppedDiagSamples:   r.droppedSamples.Load(),
		ActionsSubmitted:     r.actionsSubmitted.Load(),
		ActionsSucceeded:     r.actionsSucceeded.Load(),
		ActionsCompensated:   r.actionsCompensated.Load(),
		ActionsTimedOut:      r.actionsTimedOut.Load(),
		PairingsInterrupted:  r.pairingsInterrupted.Load(),
		TapTimeoutRecoveries: r.tapTimeoutRecoveries.Load(),
	}
}

// LatencySummary returns a one-line callback duration summary at process exit: for plain and candidate events
// separately, the count, p50, p95, maximum and
// over-budget count (in microseconds). It contains only counts and durations, so it can be written directly to the
// terminal or the system unified log.
func (r *Recorder) LatencySummary() string {
	snap := r.Snapshot()
	o, c := snap.OrdinaryLatency, snap.CandidateLatency
	return fmt.Sprintf("callback latency: ordinary n=%d p50_us=%d p95_us=%d max_us=%d over_budget=%d | "+
		"candidate n=%d p50_us=%d p95_us=%d max_us=%d over_budget=%d",
		o.Count, o.P50NS/1_000, o.P95NS/1_000, o.MaxNS/1_000, o.OverBudget,
		c.Count, c.P50NS/1_000, c.P95NS/1_000, c.MaxNS/1_000, c.OverBudget)
}

// Close closes the queue and waits for the background writer goroutine to drain all enqueued logs.
func (r *Recorder) Close() {
	if r == nil {
		return
	}
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		close(r.entryCh)
		r.mu.Unlock()
		<-r.doneCh
	})
}

func (r *Recorder) drainLoop() {
	defer close(r.doneCh)
	for entry := range r.entryCh {
		if entry.sample == nil {
			r.logger.Log(context.Background(), entry.level, entry.msg,
				slog.String("feature", entry.feature),
				slog.String("state", entry.state),
				slog.String(entry.detailKey, entry.detail),
			)
			continue
		}
		r.writeSample(entry.sample)
	}
	if dropped := r.pendingDroppedReport.Swap(0); dropped > 0 {
		r.logger.Warn("diagnostic_samples_dropped", slog.Uint64("count", dropped))
	}
}

// writeSample writes one event sample and also reports how many were dropped because the queue was full since the
// previous sample.
func (r *Recorder) writeSample(s *EventSample) {
	dropped := r.pendingDroppedReport.Swap(0)
	attrs := []any{
		slog.Uint64("gesture_id", s.GestureID),
		slog.String("stage", s.Stage),
		slog.Int("clicks", s.ClickCount),
		slog.String("mods", s.Modifiers),
		slog.Int("pid", int(s.PID)),
		slog.String("proc", s.ProcessKind),
		slog.String("role", s.AXRole),
		slog.String("subrole", s.AXSubrole),
		slog.String("url", s.URLKind),
		slog.String("class", s.Classification),
		slog.String("source", s.Source),
		slog.String("decision", s.Decision),
		slog.Uint64("elapsed_us", s.ElapsedUS),
		slog.Bool("in_budget", s.InBudget),
	}
	if s.ParentChain != "" {
		attrs = append(attrs, slog.String("chain", s.ParentChain))
	}
	if s.Container != "" {
		attrs = append(attrs, slog.String("container", s.Container))
	}
	if s.Selection != "" {
		attrs = append(attrs, slog.String("sel", s.Selection))
	}
	if dropped > 0 {
		attrs = append(attrs, slog.Uint64("dropped_samples", dropped))
	}
	if s.Mode == "observe" {
		r.logger.Warn("observe_sample", attrs...)
	} else {
		r.logger.Debug("gesture_event", attrs...)
	}
}
