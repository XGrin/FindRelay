package relay

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/XGrin/findrelay/internal/darwin"
	"github.com/XGrin/findrelay/internal/diagnostics"
	"github.com/XGrin/findrelay/internal/policy"
)

func TestExplicitOpenFailureTriggersSingleFinderCompensation(t *testing.T) {
	const (
		targetApp = "/Applications/Bloom.app"
		finderApp = "/System/Library/CoreServices/Finder.app"
	)

	var (
		mu    sync.Mutex
		calls []string
	)
	rec := diagnostics.NewRecorder(nil, diagnostics.LogWarn, 32)
	defer rec.Close()

	exec := NewExecutor(ExecutorConfig{
		TargetAppPath:     targetApp,
		FinderAppPath:     finderApp,
		Capacity:          8,
		CompletionTimeout: 200 * time.Millisecond,
		Recorder:          rec,
		Opener: func(appPath string, onComplete func(error)) {
			mu.Lock()
			calls = append(calls, appPath)
			mu.Unlock()
			go func() {
				if appPath == targetApp {
					onComplete(errors.New("launch failed"))
					return
				}
				onComplete(nil)
			}()
		},
	})

	if !exec.TryReserve(1) {
		t.Fatalf("TryReserve(1) failed")
	}
	if !exec.Submit(policy.Action{GestureID: 1, Kind: policy.ActionActivateApp}) {
		t.Fatalf("Submit(1) failed")
	}
	exec.Close()

	mu.Lock()
	gotCalls := append([]string(nil), calls...)
	mu.Unlock()

	if len(gotCalls) != 2 || gotCalls[0] != targetApp || gotCalls[1] != finderApp {
		t.Fatalf("calls = %v, want [%s %s]", gotCalls, targetApp, finderApp)
	}
	snap := rec.Snapshot()
	if snap.ActionsSubmitted != 1 || snap.ActionsCompensated != 1 || snap.ActionsSucceeded != 0 {
		t.Fatalf("unexpected metrics: %+v", snap)
	}
	if exec.InUseCount() != 0 {
		t.Fatalf("expected 0 in-use slots after compensation settled, got %d", exec.InUseCount())
	}
}

func TestTargetAppInvalidatedDisablesSubsequentRedirects(t *testing.T) {
	var invalidReason string
	exec := NewExecutor(ExecutorConfig{
		TargetAppPath:     "/Applications/Removed.app",
		FinderAppPath:     "/System/Library/CoreServices/Finder.app",
		Capacity:          8,
		CompletionTimeout: 200 * time.Millisecond,
		RevalidateTarget: func(_ string) error {
			return errors.New("bundle missing")
		},
		Opener: func(_ string, onComplete func(error)) {
			onComplete(nil)
		},
		OnTargetInvalid: func(reason string) {
			invalidReason = reason
		},
	})

	if !exec.TryReserve(1) || !exec.Submit(policy.Action{GestureID: 1, Kind: policy.ActionActivateApp}) {
		t.Fatalf("initial reserve/submit failed")
	}

	// Wait for the worker to finish revalidation and disable further reservations.
	deadline := time.Now().Add(time.Second)
	for !exec.Disabled() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !exec.Disabled() || invalidReason != "target_app_invalidated" {
		t.Fatalf("expected executor disabled with target_app_invalidated, disabled=%v reason=%q", exec.Disabled(), invalidReason)
	}
	if exec.TryReserve(2) {
		t.Fatalf("TryReserve must fail once target app is invalidated")
	}
	exec.Close()
}

func TestTimeoutLateAndDuplicateCallbacksRespectCapacityAndOutcome(t *testing.T) {
	const (
		targetApp = "/Applications/SlowApp.app"
		finderApp = "/System/Library/CoreServices/Finder.app"
	)

	var (
		mu        sync.Mutex
		callbacks []func(error)
		compCount int
	)

	rec := diagnostics.NewRecorder(nil, diagnostics.LogWarn, 32)
	defer rec.Close()

	exec := NewExecutor(ExecutorConfig{
		TargetAppPath:     targetApp,
		FinderAppPath:     finderApp,
		Capacity:          2,
		CompletionTimeout: 25 * time.Millisecond,
		Recorder:          rec,
		Opener: func(appPath string, onComplete func(error)) {
			mu.Lock()
			defer mu.Unlock()
			if appPath == finderApp {
				compCount++
				onComplete(nil)
				return
			}
			callbacks = append(callbacks, onComplete)
		},
	})
	defer exec.Close()

	// Submit 2 slow requests so that both time out into the unknown in-flight state.
	for gid := uint64(1); gid <= 2; gid++ {
		if !exec.TryReserve(gid) {
			t.Fatalf("TryReserve(%d) failed", gid)
		}
		if !exec.Submit(policy.Action{GestureID: gid, Kind: policy.ActionActivateApp}) {
			t.Fatalf("Submit(%d) failed", gid)
		}
	}

	// Wait for both requests to time out.
	deadline := time.Now().Add(time.Second)
	for rec.Snapshot().ActionsTimedOut < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := rec.Snapshot().ActionsTimedOut; got != 2 {
		t.Fatalf("ActionsTimedOut = %d, want 2", got)
	}

	// The unfinished requests still occupy capacity (2/2), so the 3rd reservation must fail.
	if exec.InUseCount() != 2 {
		t.Fatalf("InUseCount = %d, want 2", exec.InUseCount())
	}
	if exec.TryReserve(3) {
		t.Fatalf("TryReserve(3) should fail when 2 timed-out requests are still in flight")
	}

	// The late callback of the 1st request arrives with an error and is repeated 3 times:
	// capacity may be released only once (1/2 left), and no late Finder compensation may be triggered!
	mu.Lock()
	cb1 := callbacks[0]
	mu.Unlock()
	cb1(errors.New("late failure"))
	cb1(errors.New("duplicate late failure"))
	cb1(nil)

	if exec.InUseCount() != 1 {
		t.Fatalf("InUseCount after late + duplicate callbacks = %d, want 1", exec.InUseCount())
	}
	mu.Lock()
	gotComp := compCount
	mu.Unlock()
	if gotComp != 0 {
		t.Fatalf("late error callback must NOT trigger Finder compensation, got %d", gotComp)
	}

	// One unit of capacity has now been released, so the 3rd reservation should succeed.
	if !exec.TryReserve(3) {
		t.Fatalf("TryReserve(3) should succeed after 1 late slot reclaimed")
	}
	exec.ReleaseReservation(3)

	// Clean up the 2nd late callback.
	mu.Lock()
	cb2 := callbacks[1]
	mu.Unlock()
	cb2(nil)
	if exec.InUseCount() != 0 {
		t.Fatalf("InUseCount after all late callbacks = %d, want 0", exec.InUseCount())
	}
}

type fakeProbe struct {
	obs darwin.AXObservation
}

func (p *fakeProbe) HitTest(_ policy.Point, _ uint64, _ darwin.InspectMode) darwin.AXObservation {
	return p.obs
}

func (p *fakeProbe) Recheck(_ policy.Point, _ uint64, _ bool) darwin.AXObservation {
	return p.obs
}

func (p *fakeProbe) EndGesture() {}

type fakeTapEnabler struct {
	allow bool
	calls int
}

func (f *fakeTapEnabler) SetEnabled(enabled bool) bool {
	if enabled {
		f.calls++
	}
	return f.allow
}

func TestControllerEndToEndAndTapDisableRecovery(t *testing.T) {
	var opened []string
	var mu sync.Mutex

	var logBuf bytes.Buffer
	rec := diagnostics.NewRecorder(&logBuf, diagnostics.LogDebug, 64)

	exec := NewExecutor(ExecutorConfig{
		TargetAppPath:     "/Applications/Bloom.app",
		FinderAppPath:     "/System/Library/CoreServices/Finder.app",
		Capacity:          8,
		CompletionTimeout: 200 * time.Millisecond,
		Recorder:          rec,
		Opener: func(appPath string, onComplete func(error)) {
			mu.Lock()
			opened = append(opened, appPath)
			mu.Unlock()
			onComplete(nil)
		},
	})

	var fatalReason string
	trusted := true
	nowNS := uint64(10_000_000)

	ctrl := NewController(ControllerConfig{
		DockEnabled:       true,
		DockInterceptMode: policy.InterceptOnUp,
		Probe: &fakeProbe{obs: darwin.AXObservation{
			PID:          777,
			ProcessKind:  darwin.ProcessDock,
			IsDockFinder: true,
			Role:         "AXDockItem",
			Subrole:      "AXApplicationDockItem",
			URLKind:      darwin.URLFinderApp,
		}},
		Executor:  exec,
		Recorder:  rec,
		NowNS:     func() uint64 { nowNS += 200_000; return nowNS },
		IsTrusted: func() bool { return trusted },
		OnFatalStop: func(reason string) {
			fatalReason = reason
		},
	})

	// 1. Qualifying Dock Finder click: Down passes through, and Up returns TapCancelUp to clear the pressed state and
	// opens the target app.
	if got := ctrl.OnMouseEvent(policy.Event{Kind: policy.EventDown, Point: policy.Point{X: 500, Y: 1040}, ClickCount: 1, TimeNS: 100_000_000}); got != darwin.TapPass {
		t.Fatalf("expected Down TapPass in InterceptOnUp, got %v", got)
	}
	if got := ctrl.OnMouseEvent(policy.Event{Kind: policy.EventUp, Point: policy.Point{X: 500, Y: 1040}, ClickCount: 1, TimeNS: 150_000_000}); got != darwin.TapCancelUp {
		t.Fatalf("expected Up TapCancelUp in InterceptOnUp, got %v", got)
	}

	exec.Close()
	mu.Lock()
	if len(opened) != 1 || opened[0] != "/Applications/Bloom.app" {
		t.Fatalf("opened = %v, want [/Applications/Bloom.app]", opened)
	}
	mu.Unlock()

	// 2. The first TapDisabledByTimeout recovers automatically once; the second timeout stops active interception.
	enabler := &fakeTapEnabler{allow: true}
	ctrl.HandleTapDisabled(darwin.TapDisabledByTimeout, enabler)
	if fatalReason != "" || enabler.calls != 1 {
		t.Fatalf("expected first timeout to recover once, fatal=%q calls=%d", fatalReason, enabler.calls)
	}

	ctrl.HandleTapDisabled(darwin.TapDisabledByTimeout, enabler)
	if fatalReason != "tap_disabled_repeated_timeout" {
		t.Fatalf("expected repeated timeout fatal stop, got %q", fatalReason)
	}

	rec.Close()
	// Confirm the debug log contains no raw paths or coordinates.
	logs := logBuf.String()
	if strings.Contains(logs, "Bloom.app") || strings.Contains(logs, "1040") {
		t.Fatalf("debug logs leaked sensitive path or coordinate:\n%s", logs)
	}
}

func TestControllerObserveModeNeverConsumes(t *testing.T) {
	var logBuf bytes.Buffer
	rec := diagnostics.NewRecorder(&logBuf, diagnostics.LogWarn, 64)

	ctrl := NewController(ControllerConfig{
		ObserveOnly: true,
		Probe: &fakeProbe{obs: darwin.AXObservation{
			PID:          777,
			ProcessKind:  darwin.ProcessDock,
			IsDockFinder: true,
			Role:         "AXDockItem",
			Subrole:      "AXApplicationDockItem",
			ParentChain:  "AXDockItem/AXApplicationDockItem>AXList",
			URLKind:      darwin.URLFinderApp,
		}},
		Recorder: rec,
		NowNS:    func() uint64 { return 50_000_000 },
	})

	if ctrl.OnMouseEvent(policy.Event{Kind: policy.EventDown, Point: policy.Point{X: 500, Y: 1040}, ClickCount: 1, TimeNS: 50_000_000}) != darwin.TapPass {
		t.Fatalf("observe mode must never consume Down")
	}
	if ctrl.OnMouseEvent(policy.Event{Kind: policy.EventUp, Point: policy.Point{X: 500, Y: 1040}, ClickCount: 1, TimeNS: 90_000_000}) != darwin.TapPass {
		t.Fatalf("observe mode must never consume Up")
	}

	rec.Close()
	out := logBuf.String()
	if !strings.Contains(out, "observe_sample") || !strings.Contains(out, "class=dock_finder") {
		t.Fatalf("expected sanitized observe sample, got:\n%s", out)
	}
}

func TestShutdownIgnoresTapDisabledByUserInput(t *testing.T) {
	var logBuf bytes.Buffer
	rec := diagnostics.NewRecorder(&logBuf, diagnostics.LogDebug, 32)
	fatalCalled := false

	ctrl := NewController(ControllerConfig{
		DockEnabled:       true,
		DockInterceptMode: policy.InterceptOnDown,
		Recorder:          rec,
		OnFatalStop: func(_ string) {
			fatalCalled = true
		},
	})

	ctrl.DrainForShutdown(50 * time.Millisecond)
	ctrl.HandleTapDisabled(darwin.TapDisabledByUserInput, nil)
	rec.Close()

	if fatalCalled {
		t.Fatalf("expected TapDisabledByUserInput during shutdown to be ignored")
	}
	if strings.Contains(logBuf.String(), "tap_disabled_by_user_input") {
		t.Fatalf("unexpected error log during graceful shutdown:\n%s", logBuf.String())
	}
}

func TestConcurrentSubmitAndCloseNoPanic(t *testing.T) {
	for round := 0; round < 20; round++ {
		rec := diagnostics.NewRecorder(nil, diagnostics.LogWarn, 64)
		exec := NewExecutor(ExecutorConfig{
			TargetAppPath:     "/Applications/Bloom.app",
			FinderAppPath:     "/System/Library/CoreServices/Finder.app",
			Capacity:          8,
			CompletionTimeout: 100 * time.Millisecond,
			Recorder:          rec,
			Opener: func(_ string, onComplete func(error)) {
				onComplete(nil)
			},
		})

		var wg sync.WaitGroup
		for g := 0; g < 16; g++ {
			wg.Add(1)
			go func(base uint64) {
				defer wg.Done()
				for i := uint64(1); i <= 8; i++ {
					gid := base*100 + i
					if exec.TryReserve(gid) {
						if !exec.Submit(policy.Action{GestureID: gid, Kind: policy.ActionActivateApp}) {
							exec.ReleaseReservation(gid)
						}
					}
				}
			}(uint64(g + 1))
		}
		go func() {
			time.Sleep(time.Duration(round%3) * time.Millisecond)
			exec.Close()
		}()
		wg.Wait()
		exec.Close()
		rec.Close()
	}
}

func TestOrdinaryVsCandidateEventMetrics(t *testing.T) {
	rec := diagnostics.NewRecorder(nil, diagnostics.LogWarn, 32)
	defer rec.Close()

	probe := &fakeProbe{}
	ctrl := NewController(ControllerConfig{
		DockEnabled:       true,
		DockInterceptMode: policy.InterceptOnUp,
		Probe:             probe,
		Recorder:          rec,
		NowNS:             func() uint64 { return 10_000_000 },
	})

	// A click on a third-party app window should count toward OrdinaryEvents and not pollute CandidateEvents.
	probe.obs = darwin.AXObservation{PID: 500, ProcessKind: darwin.ProcessOther}
	ctrl.OnMouseEvent(policy.Event{Kind: policy.EventDown, Point: policy.Point{X: 300, Y: 300}, ClickCount: 1, TimeNS: 10_000_000})
	ctrl.OnMouseEvent(policy.Event{Kind: policy.EventUp, Point: policy.Point{X: 300, Y: 300}, ClickCount: 1, TimeNS: 40_000_000})

	// A click on a Dock icon should count toward CandidateEvents.
	probe.obs = darwin.AXObservation{PID: 777, ProcessKind: darwin.ProcessDock, IsDockFinder: true}
	ctrl.OnMouseEvent(policy.Event{Kind: policy.EventDown, Point: policy.Point{X: 40, Y: 200}, ClickCount: 1, TimeNS: 50_000_000})

	snap := rec.Snapshot()
	if snap.OrdinaryEvents != 2 || snap.CandidateEvents != 1 {
		t.Fatalf("unexpected Ordinary/Candidate counts: %+v", snap)
	}
}

// recordingWarmer records the hit mode and the total time limit the warm-up received.
type recordingWarmer struct {
	fakeProbe
	calls    int
	mode     darwin.InspectMode
	budgetNS uint64
}

func (w *recordingWarmer) WarmUp(mode darwin.InspectMode, budgetNS uint64) darwin.AXWarmupReport {
	w.calls++
	w.mode, w.budgetNS = mode, budgetNS
	return darwin.AXWarmupReport{Steps: []darwin.WarmupStep{{Name: "dock_read", Status: 0, ElapsedNS: 1_000_000}}}
}

// Warm-up has a fixed total time limit and runs with the controller's hit mode; when every step succeeds it logs only
// one redacted line at debug level.
func TestWarmUpAXPassesTotalBudgetAndMode(t *testing.T) {
	var logBuf bytes.Buffer
	rec := diagnostics.NewRecorder(&logBuf, diagnostics.LogDebug, 32)
	w := &recordingWarmer{}
	WarmUpAX(w, darwin.InspectActive, rec)
	rec.Close()

	if w.calls != 1 || w.mode != darwin.InspectActive || w.budgetNS != DefaultWarmupBudgetNS {
		t.Fatalf("warmup calls=%d mode=%v budget=%d, want 1 call with InspectActive and the %d ns budget", w.calls, w.mode, w.budgetNS, DefaultWarmupBudgetNS)
	}
	out := logBuf.String()
	for _, want := range []string{"state=warmup_complete", "dock_read=ok/1.0ms"} {
		if !strings.Contains(out, want) {
			t.Errorf("warmup log missing %q:\n%s", want, out)
		}
	}
}

// Probes that do not support warm-up are skipped and leave no log.
func TestWarmUpAXSkipsProbesWithoutWarmUp(t *testing.T) {
	var logBuf bytes.Buffer
	rec := diagnostics.NewRecorder(&logBuf, diagnostics.LogDebug, 32)
	WarmUpAX(&fakeProbe{}, darwin.InspectDock, rec)
	rec.Close()

	if logBuf.Len() != 0 {
		t.Fatalf("unexpected warmup log:\n%s", logBuf.String())
	}
}

// Under --debug, ordinary downs and ups each log one gesture_event line (with elapsed_us); drag events in the idle
// state are not logged one by one,
// but their durations still go into the histogram.
func TestDebugLogsOrdinaryClicksButNotIdleDrags(t *testing.T) {
	var logBuf bytes.Buffer
	rec := diagnostics.NewRecorder(&logBuf, diagnostics.LogDebug, 256)
	probe := &fakeProbe{obs: darwin.AXObservation{PID: 500, ProcessKind: darwin.ProcessOther}}
	nowNS := uint64(10_000_000)
	next := func() uint64 {
		nowNS += 1_000_000
		return nowNS
	}
	ctrl := NewController(ControllerConfig{
		DockEnabled: true,
		Probe:       probe,
		Recorder:    rec,
		NowNS:       next,
	})

	for i := 0; i < 50; i++ {
		ctrl.OnMouseEvent(policy.Event{Kind: policy.EventDragged, Point: policy.Point{X: 300, Y: 300}, ClickCount: 1, TimeNS: next()})
	}
	ctrl.OnMouseEvent(policy.Event{Kind: policy.EventDown, Point: policy.Point{X: 300, Y: 300}, ClickCount: 1, TimeNS: next()})
	ctrl.OnMouseEvent(policy.Event{Kind: policy.EventUp, Point: policy.Point{X: 300, Y: 300}, ClickCount: 1, TimeNS: next()})
	rec.Close()

	logs := logBuf.String()
	if n := strings.Count(logs, "gesture_event"); n != 2 {
		t.Fatalf("gesture_event lines = %d, want exactly one per ordinary down and up:\n%s", n, logs)
	}
	for _, want := range []string{"stage=down", "stage=up", "elapsed_us=1000", "decision=pass", "class=other_app"} {
		if !strings.Contains(logs, want) {
			t.Errorf("debug log missing %q:\n%s", want, logs)
		}
	}
	// Ordinary clicks do not record the clicked app's process ID; recording it every time would amount to a click trail.
	if strings.Contains(logs, "pid=500") || strings.Count(logs, "pid=0 ") != 2 {
		t.Errorf("ordinary click samples must carry pid=0:\n%s", logs)
	}
	if snap := rec.Snapshot(); snap.OrdinaryEvents != 52 {
		t.Errorf("ordinary events = %d, want 52 (50 idle drags counted without being logged)", snap.OrdinaryEvents)
	}
}
