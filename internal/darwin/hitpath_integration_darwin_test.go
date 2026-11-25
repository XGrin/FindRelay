package darwin

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/XGrin/findrelay/internal/policy"
)

// Read-only integration tests of the real hit path: run with FINDRELAY_AX_INTEGRATION=1, Accessibility granted, and the
// screen unlocked. They run HitTest and Recheck on Dock icons and desktop icons that actually exist on screen, assert
// which hits each identification and recheck made, and log steady-state timings. Only counts, categories and timings
// are logged, never paths, names or coordinates.

const integrationDeadline = 50 * time.Millisecond

func integrationDeadlineNS() uint64 {
	return MonotonicNowNS() + uint64(integrationDeadline)
}

// integrationInspector creates a real probe and wraps its hit implementation with a recorder: it still calls the real
// implementation and only records the scope of each hit.
func integrationInspector(t *testing.T) (ax *AXInspector, scopes *[]axElementRef) {
	t.Helper()
	if os.Getenv("FINDRELAY_AX_INTEGRATION") != "1" {
		t.Skip("set FINDRELAY_AX_INTEGRATION=1 to run the real Accessibility hit test")
	}
	if !IsProcessTrusted(false) {
		t.Skip("Accessibility trust is required")
	}
	ax, err := NewAXInspector("")
	if err != nil {
		t.Fatalf("NewAXInspector: %v", err)
	}
	t.Cleanup(ax.Close)
	scopes = &[]axElementRef{}
	realHit := ax.hit
	ax.hit = func(scope axElementRef, pt policy.Point, deadlineNS uint64) (axElementRef, int, bool) {
		*scopes = append(*scopes, scope)
		return realHit(scope, pt, deadlineNS)
	}
	return ax, scopes
}

func assertScopes(t *testing.T, what string, got []axElementRef, want ...axElementRef) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d hits with scopes %v, want %d hits with scopes %v", what, len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: hit scopes %v, want %v", what, got, want)
		}
	}
}

func logLatency(t *testing.T, label string, samples []time.Duration) {
	t.Helper()
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	if len(samples) == 0 {
		t.Logf("%s: no samples", label)
		return
	}
	t.Logf("%s: n=%d p50=%s p95=%s max=%s", label, len(samples),
		percentile(samples, 0.50), percentile(samples, 0.95), samples[len(samples)-1])
}

func timeIt(samples *[]time.Duration, fn func()) {
	start := time.Now()
	fn()
	*samples = append(*samples, time.Since(start))
}

// Hit path with only Dock enabled: a Dock icon still gets a system-wide confirmation after the in-app hit; a recheck
// makes only one system-wide hit and compares equal to the retained element;
// a position not on the Dock makes only one hit, against the Dock application element.
func TestDockHitPathIntegration(t *testing.T) {
	ax, scopes := integrationInspector(t)

	center, status := ax.dockFinderCenter(MonotonicNowNS() + uint64(250*time.Millisecond))
	if status != 0 {
		t.Skipf("Finder dock icon not available (status %d); unlock the screen and keep the Dock shown", status)
	}

	*scopes = nil
	obs := ax.HitTest(center, integrationDeadlineNS(), InspectDock)
	assertScopes(t, "dock icon identification", *scopes, ax.dockApp, ax.ref)
	if obs.AXError != 0 || !obs.IsDockFinder || obs.HitSource != SourceSystem {
		t.Fatalf("dock icon observation = %+v, want the Dock Finder icon identified by the system-wide hit", obs)
	}
	if !ax.hasHeld {
		t.Fatal("a successful identification must retain the hit element")
	}

	*scopes = nil
	recheck := ax.Recheck(center, integrationDeadlineNS(), false)
	assertScopes(t, "dock icon recheck", *scopes, ax.ref)
	if recheck.HitSource != SourceSame || !recheck.IsDockFinder {
		t.Fatalf("dock icon recheck = %+v, want the same element", recheck)
	}
	ax.EndGesture()

	// Find a position with no Dock element: hit the Dock application element directly until it reports "no value".
	var offDock policy.Point
	for y := 100.0; y < 1000 && offDock == (policy.Point{}); y += 100 {
		for x := 200.0; x < 1800; x += 100 {
			el, axErr, _ := ax.hitAt(ax.dockApp, policy.Point{X: x, Y: y}, integrationDeadlineNS())
			if el == 0 && axErr == axErrNoValue {
				offDock = policy.Point{X: x, Y: y}
				break
			}
		}
	}
	if offDock == (policy.Point{}) {
		t.Skip("no point outside the Dock found in the scanned region")
	}
	*scopes = nil
	obs = ax.HitTest(offDock, integrationDeadlineNS(), InspectDock)
	assertScopes(t, "point outside the Dock", *scopes, ax.dockApp)
	if obs.HitSource != SourceOffDock || obs.AXError != 0 {
		t.Fatalf("observation outside the Dock = %+v, want source=off_dock", obs)
	}
	if hit := TargetFromObservation(obs); hit.Err != nil || hit.Target.Kind != policy.TargetUnknown {
		t.Fatalf("a point outside the Dock must pass through: %+v", hit)
	}

	// Steady-state timings: 200 runs each of identify, recheck, and the Dock-only pass-through.
	var identify, rechecks, misses []time.Duration
	for i := 0; i < 200; i++ {
		timeIt(&identify, func() { ax.HitTest(center, integrationDeadlineNS(), InspectDock) })
		timeIt(&rechecks, func() { ax.Recheck(center, integrationDeadlineNS(), false) })
		ax.EndGesture()
		timeIt(&misses, func() { ax.HitTest(offDock, integrationDeadlineNS(), InspectDock) })
	}
	logLatency(t, "dock: identify (app hit + system hit + role/subrole/url)", identify)
	logLatency(t, "dock: recheck (one system hit + CFEqual)", rechecks)
	logLatency(t, "dock-only: point outside the Dock (one app hit)", misses)
}

// Hit path with the desktop entry enabled: identifying a desktop icon first passes two gates, the Dock and the window
// hit, then makes a system-wide hit; identification reads only roles, the parent chain stops at AXApplication, and
// subroles are not read;
// a recheck makes only one system-wide hit and compares equal to the retained element, with identity and path
// unchanged; when the element at the hit position changes, the target is judged changed.
func TestDesktopHitPathIntegration(t *testing.T) {
	ax, scopes := integrationInspector(t)

	// Use system-wide hits to find a position whose topmost element really is a desktop icon, and along the way note
	// positions on the empty desktop and on a Finder window.
	var icon, background, window *policy.Point
	for y := 24.0; y < 1080 && icon == nil; y += 40 {
		for x := 80.0; x < 1920; x += 40 {
			at := policy.Point{X: x, Y: y}
			obs := ax.HitTest(at, MonotonicNowNS()+uint64(250*time.Millisecond), InspectActive)
			if obs.AXError == -25200 {
				t.Skip("Accessibility API disabled although the process is trusted: unlock the screen and wake the display")
			}
			ax.EndGesture()
			point := at
			switch {
			case obs.ProcessKind == ProcessFinder && obs.FinderContainer == ContainerDesktopIcon && obs.URLKind == URLFile:
				if status, _, _ := ClassifyDirectory(obs.FilePath); status == DirEligible && isDesktopFolder(filepath.Dir(obs.FilePath)) {
					icon = &point
				}
			case obs.ProcessKind == ProcessFinder && obs.FinderContainer == ContainerDesktopBackground && background == nil:
				background = &point
			case obs.ProcessKind == ProcessFinder && obs.FinderContainer == ContainerFinderWindow && window == nil:
				window = &point
			}
		}
	}
	if icon == nil {
		t.Skip("no visible desktop directory icon found in the scanned region")
	}

	*scopes = nil
	obs := ax.HitTest(*icon, integrationDeadlineNS(), InspectActive)
	assertScopes(t, "desktop icon identification", *scopes, ax.dockApp, ax.ref)
	if obs.AXError != 0 || obs.FinderContainer != ContainerDesktopIcon || obs.URLKind != URLFile {
		t.Fatalf("desktop icon observation = %+v", obs)
	}
	if !slices.Equal(obs.Roles, []string{"AXImage", "AXGroup", "AXScrollArea", "AXApplication"}) {
		t.Fatalf("desktop icon roles = %v, want the four roles up to AXApplication", obs.Roles)
	}
	if obs.Subrole != "" {
		t.Fatalf("active identification must not read the Finder subrole, got %q", obs.Subrole)
	}
	identified := TargetFromObservation(obs)
	if identified.Err != nil || identified.Target.Kind != policy.TargetDesktopDirectory {
		t.Fatalf("desktop icon did not resolve to a desktop directory target: %+v", identified)
	}

	*scopes = nil
	recheck := ax.Recheck(*icon, integrationDeadlineNS(), true)
	assertScopes(t, "desktop icon recheck", *scopes, ax.ref)
	if recheck.HitSource != SourceSame || recheck.FinderContainer != ContainerDesktopIcon || recheck.ElementHash != obs.ElementHash {
		t.Fatalf("desktop icon recheck = %+v, want the same element", recheck)
	}
	confirmed := TargetFromObservation(recheck)
	if confirmed.Err != nil || confirmed.Target.Kind != policy.TargetDesktopDirectory ||
		confirmed.Target.Identity != identified.Target.Identity || confirmed.Target.Path != identified.Target.Path {
		t.Fatalf("recheck target = %+v, want the identified target %+v", confirmed, identified)
	}
	if background != nil {
		if changed := ax.Recheck(*background, integrationDeadlineNS(), true); changed.HitSource != SourceChanged {
			t.Fatalf("recheck on the desktop background = %+v, want target changed", changed)
		}
	}
	ax.EndGesture()

	// A hit inside a window can never be a desktop icon: in active mode the parent chain stops at AXWindow.
	if window != nil {
		*scopes = nil
		inWindow := ax.HitTest(*window, integrationDeadlineNS(), InspectActive)
		assertScopes(t, "finder window identification", *scopes, ax.dockApp, ax.ref)
		if inWindow.FinderContainer != ContainerFinderWindow || len(inWindow.Roles) == 0 || inWindow.Roles[len(inWindow.Roles)-1] != "AXWindow" {
			t.Fatalf("finder window roles = %v, want the chain to stop at AXWindow", inWindow.Roles)
		}
	} else {
		t.Log("no Finder window found in the scanned region; the early stop at AXWindow was not exercised")
	}

	var identify, rechecks []time.Duration
	for i := 0; i < 200; i++ {
		timeIt(&identify, func() { ax.HitTest(*icon, integrationDeadlineNS(), InspectActive) })
		timeIt(&rechecks, func() { ax.Recheck(*icon, integrationDeadlineNS(), true) })
		ax.EndGesture()
	}
	logLatency(t, "desktop: identify (dock app hit + window hit + system hit + roles + url + selection)", identify)
	logLatency(t, "desktop: recheck (one system hit + CFEqual + selection)", rechecks)
}

// Window hit gate with the desktop entry enabled: scan grid points on screen; for every point the gate passes through as
// "covered by another app's window", the system-wide hit must not land on Finder or the Dock, otherwise a target that
// should have been identified was missed. Also compare the timings of the two paths at these points.
func TestDesktopWindowGateIntegration(t *testing.T) {
	ax, _ := integrationInspector(t)
	if !ax.ensureDockApp() {
		t.Skip("Dock is not running in this session")
	}
	const step = 64.0
	var covered, asked, missed, transient, disabled int
	var gated, full []time.Duration
	for y := -2200.0 + step/2; y < 2200; y += step {
		for x := step / 2; x < 4000; x += step {
			at := policy.Point{X: x, Y: y}
			start := time.Now()
			obs := ax.HitTest(at, integrationDeadlineNS(), InspectActive)
			gatedElapsed := time.Since(start)
			ax.EndGesture()
			// Coordinates outside the displays have no windows; the system-wide hit reports -25200 and is not counted.
			if obs.AXError == -25200 {
				disabled++
				continue
			}
			if obs.HitSource != SourceCovered {
				asked++
				continue
			}
			covered++
			start = time.Now()
			truth := ax.hitSystem(at, integrationDeadlineNS(), InspectActive)
			full = append(full, time.Since(start))
			gated = append(gated, gatedElapsed)
			ax.EndGesture()
			if truth.ProcessKind == ProcessFinder || truth.ProcessKind == ProcessDock {
				// Query again right away in the reverse order: only when both results agree is it a gate miss;
				// otherwise the screen changed between the two queries.
				again := ax.hitSystem(at, integrationDeadlineNS(), InspectActive)
				ax.EndGesture()
				stillCovered := ax.topWindow(at) == windowOther
				if stillCovered && (again.ProcessKind == ProcessFinder || again.ProcessKind == ProcessDock) {
					missed++
					if missed <= 5 {
						t.Logf("missed: system-wide hit proc=%v role=%s container=%v, window hit still reports another app", again.ProcessKind, again.Role, again.FinderContainer)
					}
				} else {
					transient++
				}
			}
		}
	}
	if covered+asked == 0 {
		t.Skip("Accessibility API disabled although the process is trusted: unlock the screen and wake the display")
	}
	t.Logf("window gate: covered=%d asked_system_wide=%d covered_but_finder_or_dock=%d changed_between_queries=%d off_screen=%d", covered, asked, missed, transient, disabled)
	if missed != 0 {
		t.Fatalf("%d points were skipped as covered although the system-wide hit lands on Finder or the Dock", missed)
	}
	if covered == 0 {
		t.Log("no point covered by another app's window; open a non-Finder window to exercise the gate")
		return
	}
	logLatency(t, "desktop mode: covered point (dock app hit + window hit)", gated)
	logLatency(t, "same points: system-wide hit (the path the gate skips)", full)
}
