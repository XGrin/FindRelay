package diagnostics

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/XGrin/findrelay/internal/appbundle"
	"github.com/XGrin/findrelay/internal/service"
)

func TestRecorderBoundedQueueAndSanitization(t *testing.T) {
	var buf bytes.Buffer
	rec := NewRecorder(&buf, LogWarn, 2)

	// Push more observation samples than the capacity in a row, to ensure it never blocks and counts the drops.
	for i := 0; i < 20; i++ {
		rec.EnqueueSample(EventSample{
			Mode:           "observe",
			GestureID:      uint64(i + 1),
			Stage:          "down",
			ClickCount:     1,
			Modifiers:      "none",
			PID:            321,
			ProcessKind:    "dock",
			AXRole:         "AXDockItem",
			AXSubrole:      "AXApplicationDockItem",
			ParentChain:    "AXDockItem/AXApplicationDockItem>AXList",
			URLKind:        "finder_app",
			Classification: "dock_finder",
			Decision:       "observe",
			ElapsedUS:      420,
			InBudget:       true,
		})
	}
	rec.RecordCallbackDuration(false, 120_000, false)
	rec.RecordCallbackDuration(true, 4_500_000, false)
	rec.RecordCallbackDuration(true, 25_000_000, true)
	rec.Close()

	snap := rec.Snapshot()
	if snap.OrdinaryEvents != 1 || snap.CandidateEvents != 2 {
		t.Fatalf("unexpected event counts: %+v", snap)
	}
	if snap.MaxCallbackNS != 25_000_000 || snap.BudgetExceededCount != 1 {
		t.Fatalf("unexpected latency metrics: %+v", snap)
	}
	if snap.DroppedDiagSamples+2 < 2 {
		t.Fatalf("unexpected dropped count: %+v", snap)
	}

	out := buf.String()
	if !strings.Contains(out, "observe_sample") || !strings.Contains(out, "class=dock_finder") {
		t.Fatalf("expected sanitized observe_sample output, got:\n%s", out)
	}
}

func TestDoctorReportsAppServicePermissionsAndInstance(t *testing.T) {
	base := DoctorDeps{
		Version: "0.1.0-test",
		ValidateApp: func(string) (appbundle.BundleInfo, error) {
			return appbundle.BundleInfo{BundleID: "club.bloom.app"}, nil
		},
		SystemInfo: func() SystemVersion {
			return SystemVersion{ProductVersion: "15.1", BuildVersion: "24B83", Arch: "arm64"}
		},
		SessionInfo: func() (service.SessionInfo, error) {
			return service.SessionInfo{ID: 100015, HasGraphicAccess: true}, nil
		},
		IsAXTrusted:  func() bool { return false },
		ProbeRunning: func(_ string, _ uint32) (bool, error) { return false, nil },
	}

	var out bytes.Buffer
	configured := base
	configured.AppPath = "/Applications/Bloom.app"
	configured.AgentPlist = "/Users/test/Library/LaunchAgents/io.findrelay.agent.plist"
	configured.AgentArgs = []string{"/usr/local/bin/findrelay", "run", "--app", "/Applications/Bloom.app", "--desktop"}
	configured.RunDir = t.TempDir()
	res, err := RunDoctor(&out, configured)
	if err != nil {
		t.Fatalf("RunDoctor error = %v", err)
	}
	if !res.AppSpecified || !res.AppValid || !res.AgentInstalled || !res.SessionValid || res.AXTrusted || res.AgentRunning {
		t.Fatalf("unexpected DoctorResult: %+v", res)
	}
	report := out.String()
	for _, want := range []string{
		"[Target App]  OK (/Applications/Bloom.app, bundle=club.bloom.app)",
		"[Service]     INSTALLED",
		"args: /usr/local/bin/findrelay run --app /Applications/Bloom.app --desktop",
		"NOT GRANTED",
		"[Instance]    STOPPED in GUI session 100015",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("doctor report missing %q:\n%s", want, report)
		}
	}

	// Invalid target app: the remaining items are printed as usual, and the validation error is returned to the caller.
	out.Reset()
	validateErr := errors.New("bundle gone")
	invalid := base
	invalid.AppPath = "/Applications/Gone.app"
	invalid.ValidateApp = func(string) (appbundle.BundleInfo, error) { return appbundle.BundleInfo{}, validateErr }
	res, err = RunDoctor(&out, invalid)
	if !errors.Is(err, validateErr) || !res.AppSpecified || res.AppValid || res.AgentInstalled {
		t.Fatalf("invalid app: res=%+v err=%v", res, err)
	}
	for _, want := range []string{"[Target App]  FAIL", "[Service]     not installed", "NOT GRANTED"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("doctor report missing %q:\n%s", want, out.String())
		}
	}

	// No target app specified: not treated as an error, only a hint on how to specify one.
	out.Reset()
	res, err = RunDoctor(&out, base)
	if err != nil || res.AppSpecified || res.AppValid {
		t.Fatalf("unspecified app: res=%+v err=%v", res, err)
	}
	if !strings.Contains(out.String(), "[Target App]  not specified") {
		t.Errorf("doctor report missing the hint:\n%s", out.String())
	}
}

// The histogram uses fixed logarithmic buckets: powers of 2 land in the first bucket of each octave, bucket indexes
// are monotonic in duration, and bucket width is at most about 9%.
func TestLatencyHistogramUsesFixedLogBuckets(t *testing.T) {
	cases := []struct {
		ns  uint64
		buf int
	}{
		{0, 0}, {1, 0}, {2, 8}, {3, 12}, {4, 16}, {1_000, 79}, {1 << 20, 160}, {1 << 62, 496},
		{1 << 63, latencyBucketCount - 1}, {math.MaxUint64, latencyBucketCount - 1},
	}
	for _, tc := range cases {
		if got := latencyBucket(tc.ns); got != tc.buf {
			t.Errorf("latencyBucket(%d) = %d, want %d", tc.ns, got, tc.buf)
		}
	}
	prev := 0
	for ns := uint64(1); ns <= 1<<20; ns++ {
		got := latencyBucket(ns)
		if got < prev {
			t.Fatalf("bucket decreased at %d: %d then %d", ns-1, prev, got)
		}
		prev = got
		upper := latencyBucketUpperNS(got)
		if upper < ns || float64(upper) > float64(ns)*math.Pow(2, 1.0/latencyBucketsPerOctave)+1 {
			t.Fatalf("bucket %d upper bound %d does not cover %d within one bucket width", got, upper, ns)
		}
	}

	// On both sides of every bucket boundary in every octave: the mantissa keeps only 5 bits, and values near a
	// boundary must still land in the correct bucket.
	// When the octave's lower bound is below 16 ns, some buckets contain no integers; skip those buckets.
	for exp := 4; exp < latencyOctaves; exp++ {
		for sub := 0; sub < latencyBucketsPerOctave; sub++ {
			want := exp*latencyBucketsPerOctave + sub
			upper := latencyBucketUpperNS(want)
			edge := uint64(math.Ceil(math.Ldexp(latencyOctaveStep[sub], exp)))
			if edge >= upper {
				continue
			}
			samples := []uint64{edge}
			if edge+1 < upper {
				samples = append(samples, edge+1)
			}
			for _, ns := range samples {
				if got := latencyBucket(ns); got != want {
					t.Errorf("latencyBucket(%d) = %d, want %d", ns, got, want)
				}
				if upper < ns {
					t.Errorf("bucket %d upper bound %d is below %d", want, upper, ns)
				}
			}
		}
	}
}

func assertLatencyWithin(t *testing.T, name string, got, wantNS uint64) {
	t.Helper()
	if got < wantNS || float64(got) > float64(wantNS)*math.Pow(2, 1.0/latencyBucketsPerOctave)+1 {
		t.Errorf("%s = %d ns, want the bucket upper bound just above %d ns", name, got, wantNS)
	}
}

// Histogram and summary line: the two event kinds are counted separately for count, p50, p95, maximum and
// over-budget count.
func TestRecorderLatencyHistogramAndSummaryLine(t *testing.T) {
	rec := NewRecorder(nil, LogWarn, 8)
	defer rec.Close()

	for i := 0; i < 50; i++ {
		rec.RecordCallbackDuration(false, 100_000, false)
	}
	for i := 0; i < 45; i++ {
		rec.RecordCallbackDuration(false, 400_000, false)
	}
	for i := 0; i < 5; i++ {
		rec.RecordCallbackDuration(false, 2_000_000, false)
	}
	for i := 0; i < 9; i++ {
		rec.RecordCallbackDuration(true, 300_000, false)
	}
	rec.RecordCallbackDuration(true, 30_000_000, true)

	snap := rec.Snapshot()
	o, c := snap.OrdinaryLatency, snap.CandidateLatency
	if o.Count != 100 || snap.OrdinaryEvents != 100 {
		t.Errorf("ordinary count = %d / %d, want 100", o.Count, snap.OrdinaryEvents)
	}
	assertLatencyWithin(t, "ordinary p50", o.P50NS, 100_000)
	assertLatencyWithin(t, "ordinary p95", o.P95NS, 400_000)
	if o.MaxNS != 2_000_000 || o.OverBudget != 0 {
		t.Errorf("ordinary max/over budget = %d / %d, want 2000000 / 0", o.MaxNS, o.OverBudget)
	}
	if c.Count != 10 || snap.CandidateEvents != 10 {
		t.Errorf("candidate count = %d / %d, want 10", c.Count, snap.CandidateEvents)
	}
	assertLatencyWithin(t, "candidate p50", c.P50NS, 300_000)
	assertLatencyWithin(t, "candidate p95", c.P95NS, 30_000_000)
	if c.MaxNS != 30_000_000 || c.OverBudget != 1 {
		t.Errorf("candidate max/over budget = %d / %d, want 30000000 / 1", c.MaxNS, c.OverBudget)
	}
	if snap.MaxCallbackNS != 30_000_000 || snap.BudgetExceededCount != 1 {
		t.Errorf("overall max/over budget = %d / %d, want 30000000 / 1", snap.MaxCallbackNS, snap.BudgetExceededCount)
	}

	want := fmt.Sprintf("callback latency: ordinary n=100 p50_us=%d p95_us=%d max_us=2000 over_budget=0 | "+
		"candidate n=10 p50_us=%d p95_us=%d max_us=30000 over_budget=1",
		o.P50NS/1_000, o.P95NS/1_000, c.P50NS/1_000, c.P95NS/1_000)
	if got := rec.LatencySummary(); got != want {
		t.Errorf("LatencySummary() =\n%s\nwant\n%s", got, want)
	}
}

// The recording path on the tap thread does not allocate memory.
func TestRecordCallbackDurationDoesNotAllocate(t *testing.T) {
	rec := NewRecorder(nil, LogWarn, 8)
	defer rec.Close()

	allocs := testing.AllocsPerRun(1000, func() {
		rec.RecordCallbackDuration(false, 123_456, false)
		rec.RecordCallbackDuration(true, 7_000_000, true)
	})
	if allocs != 0 {
		t.Fatalf("RecordCallbackDuration allocates %.1f objects per pair of events, want 0", allocs)
	}
}

// Before any samples exist, the percentiles and maximum in the summary line are all 0.
func TestLatencySummaryWithoutSamples(t *testing.T) {
	rec := NewRecorder(nil, LogWarn, 8)
	defer rec.Close()
	want := "callback latency: ordinary n=0 p50_us=0 p95_us=0 max_us=0 over_budget=0 | " +
		"candidate n=0 p50_us=0 p95_us=0 max_us=0 over_budget=0"
	if got := rec.LatencySummary(); got != want {
		t.Fatalf("LatencySummary() = %q, want %q", got, want)
	}
}
