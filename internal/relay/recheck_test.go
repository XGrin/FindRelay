package relay

import (
	"slices"
	"strings"
	"testing"

	"github.com/XGrin/findrelay/internal/darwin"
	"github.com/XGrin/findrelay/internal/diagnostics"
	"github.com/XGrin/findrelay/internal/policy"
)

// calls returns the number of identifications so far and whether each recheck asked for single-selection evidence.
func (p *modeProbe) calls() (hits int, rechecks []bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.modes), append([]bool(nil), p.rechecks...)
}

// evidenceProbe counts, on top of modeProbe, how many times a gesture ended (releasing the reference retained at
// identification).
type evidenceProbe struct {
	modeProbe
	ends int
}

func (p *evidenceProbe) EndGesture() { p.ends++ }

// A desktop gesture is identified only once, on the first click's down; each of the three later recheck points calls
// recheck once, and the two on the second click ask for single-selection evidence.
func TestControllerDesktopGestureIdentifiesOnceThenRechecks(t *testing.T) {
	for _, mode := range []policy.InterceptMode{policy.InterceptOnUp, policy.InterceptOnDown} {
		t.Run(mode.String(), func(t *testing.T) {
			h, ctrl := newDesktopHarness(t, mode, darwin.DirEligible)

			second, last := h.doubleClick(t, ctrl)
			wantSecond, wantLast := darwin.TapPass, darwin.TapCancelDesktopUp
			if mode == policy.InterceptOnDown {
				wantSecond, wantLast = darwin.TapSwallow, darwin.TapSwallow
			}
			if second != wantSecond || last != wantLast {
				t.Fatalf("second click dispositions = %v, %v; want %v, %v", second, last, wantSecond, wantLast)
			}
			if hits, rechecks := h.probe.calls(); hits != 1 || !slices.Equal(rechecks, []bool{false, true, true}) {
				t.Fatalf("probe calls: %d identifications and rechecks %v, want 1 identification then rechecks [false true true]", hits, rechecks)
			}

			h.close()
			h.assertOpened(t, targetAppCall)
		})
	}
}

// A recheck accepts only the same retained element: when the second recheck reports "target changed", the gesture
// passes through and no action is submitted.
func TestControllerAbandonsGestureWhenRecheckReportsChangedTarget(t *testing.T) {
	h, ctrl := newDesktopHarness(t, policy.InterceptOnUp, darwin.DirEligible)

	h.click(ctrl, policy.EventDown, 1)
	h.click(ctrl, policy.EventUp, 1)
	waitClassified(t, h.classify, 1)
	h.probe.setObservation(darwin.AXObservation{HitSource: darwin.SourceChanged})
	second := h.click(ctrl, policy.EventDown, 2)
	last := h.click(ctrl, policy.EventUp, 2)
	if second != darwin.TapPass || last != darwin.TapPass {
		t.Fatalf("a changed target must fail open, got %v, %v", second, last)
	}
	h.close()
	h.assertOpened(t)
	if logs := h.logs.String(); !strings.Contains(logs, "class=target_changed") || !strings.Contains(logs, "source=changed") {
		t.Fatalf("logs must label the recheck that found another target:\n%s", logs)
	}
}

// A Dock gesture is also identified only once: identify on down, and on up one recheck confirms it is still the same
// icon, under both intercept policies.
func TestControllerDockGestureIdentifiesOnceThenRechecks(t *testing.T) {
	for _, mode := range []policy.InterceptMode{policy.InterceptOnUp, policy.InterceptOnDown} {
		t.Run(mode.String(), func(t *testing.T) {
			rec := diagnostics.NewRecorder(nil, diagnostics.LogWarn, 64)
			exec := NewExecutor(ExecutorConfig{
				TargetAppPath: "/Applications/Bloom.app",
				FinderAppPath: finderBundleDir,
				Capacity:      8,
				Recorder:      rec,
				Opener:        func(_ string, onComplete func(error)) { onComplete(nil) },
			})
			probe := &modeProbe{obs: darwin.AXObservation{
				PID: 777, ProcessKind: darwin.ProcessDock, IsDockFinder: true, Role: "AXDockItem", Subrole: "AXApplicationDockItem",
			}}
			nowNS := uint64(1_000_000_000)
			ctrl := NewController(ControllerConfig{
				DockEnabled:       true,
				DockInterceptMode: mode,
				Probe:             probe,
				Executor:          exec,
				Recorder:          rec,
				NowNS:             func() uint64 { nowNS += 50_000_000; return nowNS },
				IsTrusted:         func() bool { return true },
			})
			pt := policy.Point{X: 500, Y: 1040}
			down := ctrl.OnMouseEvent(policy.Event{Kind: policy.EventDown, Point: pt, ClickCount: 1, TimeNS: nowNS})
			up := ctrl.OnMouseEvent(policy.Event{Kind: policy.EventUp, Point: pt, ClickCount: 1, TimeNS: nowNS})
			exec.Close()
			rec.Close()

			wantDown, wantUp := darwin.TapPass, darwin.TapCancelUp
			if mode == policy.InterceptOnDown {
				wantDown, wantUp = darwin.TapSwallow, darwin.TapSwallow
			}
			if down != wantDown || up != wantUp {
				t.Fatalf("dispositions = %v, %v; want %v, %v", down, up, wantDown, wantUp)
			}
			if hits, rechecks := probe.calls(); hits != 1 || !slices.Equal(rechecks, []bool{false}) {
				t.Fatalf("probe calls: %d identifications and rechecks %v, want 1 identification then one recheck without selection", hits, rechecks)
			}
		})
	}
}

// The element reference retained at identification is released on gesture end, abort, drain and system reset: after
// every event that returns the state machine to idle,
// the probe receives one EndGesture; it receives none while a candidate or pair is in progress.
func TestControllerEndsGestureEvidenceOnEveryReturnToIdle(t *testing.T) {
	rec := diagnostics.NewRecorder(nil, diagnostics.LogWarn, 64)
	exec := NewExecutor(ExecutorConfig{
		TargetAppPath: "/Applications/Bloom.app",
		FinderAppPath: finderBundleDir,
		Capacity:      8,
		Recorder:      rec,
		Opener:        func(_ string, onComplete func(error)) { onComplete(nil) },
	})
	t.Cleanup(exec.Close)
	probe := &evidenceProbe{modeProbe: modeProbe{obs: darwin.AXObservation{
		PID: 777, ProcessKind: darwin.ProcessDock, IsDockFinder: true,
	}}}
	nowNS := uint64(1_000_000_000)
	ctrl := NewController(ControllerConfig{
		DockEnabled:       true,
		DockInterceptMode: policy.InterceptOnDown,
		Probe:             probe,
		Executor:          exec,
		Recorder:          rec,
		NowNS:             func() uint64 { nowNS += 50_000_000; return nowNS },
		IsTrusted:         func() bool { return true },
	})
	pt := policy.Point{X: 500, Y: 1040}
	down := func() darwin.TapDisposition {
		return ctrl.OnMouseEvent(policy.Event{Kind: policy.EventDown, Point: pt, ClickCount: 1, TimeNS: nowNS})
	}
	up := func() darwin.TapDisposition {
		return ctrl.OnMouseEvent(policy.Event{Kind: policy.EventUp, Point: pt, ClickCount: 1, TimeNS: nowNS})
	}
	assertEnds := func(after string, want int) {
		t.Helper()
		if probe.ends != want {
			t.Fatalf("EndGesture calls %s = %d, want %d", after, probe.ends, want)
		}
	}

	// Gesture ends.
	if down() != darwin.TapSwallow {
		t.Fatal("down must be consumed in InterceptOnDown")
	}
	assertEnds("during a pending pairing", 0)
	if up() != darwin.TapSwallow {
		t.Fatal("paired up must be consumed")
	}
	assertEnds("after the gesture", 1)

	// Abort: the tap recovers automatically once after a timeout, and the paired gesture is aborted with it.
	down()
	ctrl.HandleTapDisabled(darwin.TapDisabledByTimeout, &fakeTapEnabler{allow: true})
	assertEnds("after an interrupted pairing", 2)

	// System reset.
	down()
	ctrl.OnSystemReset()
	assertEnds("after a system reset", 3)

	// Drain: with no wait time, the pair is aborted directly.
	down()
	ctrl.DrainForShutdown(0)
	assertEnds("after draining", 4)
	rec.Close()
}

// Ordinary clicks passed through without a system-wide hit (not on the Dock with only Dock enabled, covered by another
// app's window with the desktop entry enabled):
// they count as ordinary events, and the --debug log marks the source.
func TestOrdinaryClickOutsideDockIsLabeledWithoutSystemHit(t *testing.T) {
	testOrdinaryClickPassedWithoutSystemHit(t, darwin.SourceOffDock, false, "off_dock")
}

func TestOrdinaryClickCoveredByOtherAppIsLabeledWithoutSystemHit(t *testing.T) {
	testOrdinaryClickPassedWithoutSystemHit(t, darwin.SourceCovered, true, "covered")
}

func testOrdinaryClickPassedWithoutSystemHit(t *testing.T, source darwin.HitSource, desktop bool, label string) {
	t.Helper()
	var logBuf syncBuffer
	rec := diagnostics.NewRecorder(&logBuf, diagnostics.LogDebug, 256)
	probe := &modeProbe{obs: darwin.AXObservation{HitSource: source}}
	nowNS := uint64(1_000_000_000)
	cfg := ControllerConfig{
		DockEnabled: true,
		Probe:       probe,
		Recorder:    rec,
		NowNS:       func() uint64 { nowNS += 50_000_000; return nowNS },
	}
	if desktop {
		classifier := NewClassifier(func(string) (darwin.DirStatus, uint64, uint64) { return darwin.DirEligible, 1, 1 }, rec, nil)
		t.Cleanup(classifier.Close)
		cfg.DesktopEnabled = true
		cfg.Snapshots = classifier
	}
	ctrl := NewController(cfg)
	pt := policy.Point{X: 960, Y: 540}
	for _, kind := range []policy.EventKind{policy.EventDown, policy.EventUp} {
		if got := ctrl.OnMouseEvent(policy.Event{Kind: kind, Point: pt, ClickCount: 1, TimeNS: nowNS}); got != darwin.TapPass {
			t.Fatalf("%v disposition = %v, want pass", kind, got)
		}
	}
	rec.Close()

	if snap := rec.Snapshot(); snap.OrdinaryEvents != 2 || snap.CandidateEvents != 0 {
		t.Fatalf("event counts: ordinary=%d candidate=%d, want 2 and 0", snap.OrdinaryEvents, snap.CandidateEvents)
	}
	logs := logBuf.String()
	for _, want := range []string{"class=" + label, "source=" + label, "decision=pass"} {
		if !strings.Contains(logs, want) {
			t.Errorf("debug log missing %q:\n%s", want, logs)
		}
	}
}

// After shutdown closes the probe, the system reset callback on the main thread must not touch it: CloseProbe closes
// it and drops the reference under stateMu.
func TestSystemResetAfterCloseProbeDoesNotTouchProbe(t *testing.T) {
	rec := diagnostics.NewRecorder(nil, diagnostics.LogWarn, 64)
	defer rec.Close()
	probe := &evidenceProbe{}
	ctrl := NewController(ControllerConfig{DockEnabled: true, Probe: probe, Recorder: rec})

	ctrl.OnSystemReset()
	if probe.ends != 1 {
		t.Fatalf("EndGesture calls before close = %d, want 1", probe.ends)
	}
	closed := false
	ctrl.CloseProbe(func() { closed = true })
	ctrl.OnSystemReset()

	if !closed {
		t.Fatal("CloseProbe did not run the close function")
	}
	if probe.ends != 1 {
		t.Fatalf("EndGesture calls after close = %d, want still 1", probe.ends)
	}
}
