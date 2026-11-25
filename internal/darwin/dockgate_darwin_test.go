package darwin

import (
	"slices"
	"testing"
	"time"

	"github.com/XGrin/findrelay/internal/policy"
)

// hitOutcome is the result one hit stub returns: a new reference (released by the code under test when non-zero),
// the AX error code, and the over-budget flag.
type hitOutcome struct {
	el         axElementRef
	err        int
	overBudget bool
}

// dispatchingHit replaces the probe's hit with a stub that dispatches by scope: hits on the Dock application element
// ask dock, hits on the system-wide element ask system, and the scope of every hit is recorded in order.
func dispatchingHit(t *testing.T, ax *AXInspector, dock, system func() hitOutcome) *[]axElementRef {
	t.Helper()
	scopes := &[]axElementRef{}
	ax.hit = func(scope axElementRef, _ policy.Point, _ uint64) (axElementRef, int, bool) {
		*scopes = append(*scopes, scope)
		out := hitOutcome{err: -25201}
		switch scope {
		case ax.dockApp:
			out = dock()
		case ax.ref:
			out = system()
		default:
			t.Errorf("unexpected hit scope %v", scope)
		}
		return out.el, out.err, out.overBudget
	}
	return scopes
}

// newDockInspector creates a probe and prepares the Dock application element; it skips the test when the current
// session has no Dock.
func newDockInspector(t *testing.T) *AXInspector {
	t.Helper()
	ax, err := NewAXInspector("")
	if err != nil {
		t.Fatalf("NewAXInspector error = %v", err)
	}
	t.Cleanup(ax.Close)
	if !ax.ensureDockApp() {
		t.Skip("Dock is not running in this session")
	}
	return ax
}

func assertHitScopes(t *testing.T, what string, got []axElementRef, want ...axElementRef) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s hits = %v, want %v", what, got, want)
	}
}

func hitDeadline() uint64 { return MonotonicNowNS() + uint64(time.Second) }

// With only Dock enabled, a position with no Dock element directly yields "not on the Dock": no system-wide hit is
// made, the clicked app is never queried, and the result is an ordinary pass-through rather than an error or over-budget.
func TestDockOnlyOrdinaryClickNeverHitsSystemWide(t *testing.T) {
	ax := newDockInspector(t)
	windows := stubTopWindow(ax, windowOther)
	scopes := dispatchingHit(t, ax,
		func() hitOutcome { return hitOutcome{err: axErrNoValue} },
		func() hitOutcome {
			t.Error("a point outside the Dock must not cause a system-wide hit")
			return hitOutcome{}
		})

	obs := ax.HitTest(policy.Point{X: 960, Y: 540}, hitDeadline(), InspectDock)

	assertHitScopes(t, "ordinary click", *scopes, ax.dockApp)
	if *windows != 0 {
		t.Fatalf("window hit queries = %d, want 0 when only the Dock entry is enabled", *windows)
	}
	if obs.HitSource != SourceOffDock || obs.AXError != 0 || obs.BudgetExceeded {
		t.Fatalf("observation = %+v, want source=off_dock without error or budget flag", obs)
	}
	if hit := TargetFromObservation(obs); hit.Err != nil || hit.BudgetExceeded || hit.Target.Kind != policy.TargetUnknown {
		t.Fatalf("result = %+v, want an unknown target that passes through", hit)
	}
}

// When the Dock has an element at the point, a system-wide hit is still made, and identification is based on its result
// rather than on the result of the in-app hit.
func TestDockHitIsConfirmedBySystemWideHit(t *testing.T) {
	ax := newDockInspector(t)
	standIn := detachedSystemElement(t)
	confirmed := detachedSystemElement(t)
	scopes := dispatchingHit(t, ax,
		func() hitOutcome { return hitOutcome{el: standIn} },
		func() hitOutcome { return hitOutcome{el: confirmed} })

	obs := ax.HitTest(policy.Point{X: 30, Y: 390}, hitDeadline(), InspectDock)

	assertHitScopes(t, "dock click", *scopes, ax.dockApp, ax.ref)
	if obs.HitSource != SourceSystem {
		t.Fatalf("source = %v, want system: the identification must come from the system-wide hit", obs.HitSource)
	}
}

// When the in-app hit fails, it falls back to a system-wide hit and forwarding is not interrupted; the stale application
// element is discarded, and the next query rebuilds it from the process ID.
func TestDockGateErrorFallsBackToSystemWideAndRebuildsAppElement(t *testing.T) {
	ax := newDockInspector(t)
	stale := ax.dockApp
	failing := true
	scopes := dispatchingHit(t, ax,
		func() hitOutcome {
			if failing {
				return hitOutcome{err: axErrorCannotComplete}
			}
			return hitOutcome{el: detachedSystemElement(t)}
		},
		func() hitOutcome { return hitOutcome{el: detachedSystemElement(t)} })

	ax.HitTest(policy.Point{X: 30, Y: 390}, hitDeadline(), InspectDock)

	assertHitScopes(t, "fallback click", *scopes, stale, ax.ref)
	if ax.dockApp != 0 {
		t.Fatalf("dock app element = %v after an error, want it dropped", ax.dockApp)
	}

	// Next click: the application element has been rebuilt, and once the in-app hit is back to normal it is still
	// followed by a system-wide confirmation.
	// The old and new elements may share an address (memory reused immediately), so only assert that it is non-nil and
	// took part in the hit.
	failing = false
	*scopes = nil
	ax.HitTest(policy.Point{X: 30, Y: 390}, hitDeadline(), InspectDock)

	if ax.dockApp == 0 {
		t.Fatal("dock app element was not rebuilt for the next query")
	}
	assertHitScopes(t, "next click", *scopes, ax.dockApp, ax.ref)
}

// stubTopWindow replaces the window hit query with a stub that gives a fixed answer, and returns the call count.
func stubTopWindow(ax *AXInspector, owner windowOwner) *int {
	calls := new(int)
	ax.topWindow = func(policy.Point) windowOwner {
		*calls++
		return owner
	}
	return calls
}

func dockMiss() hitOutcome { return hitOutcome{err: axErrNoValue} }

// With the desktop entry enabled, no Dock element at the point, and the topmost window owned by another app, only the
// Dock and the window server are asked: no system-wide hit is made, the clicked app is never queried, and the result is
// an ordinary pass-through.
func TestDesktopModeClickCoveredByOtherAppSkipsSystemWide(t *testing.T) {
	ax := newDockInspector(t)
	windows := stubTopWindow(ax, windowOther)
	scopes := dispatchingHit(t, ax, dockMiss, func() hitOutcome {
		t.Error("a point covered by another app must not cause a system-wide hit")
		return hitOutcome{}
	})

	obs := ax.HitTest(policy.Point{X: 960, Y: 540}, hitDeadline(), InspectActive)

	assertHitScopes(t, "covered click", *scopes, ax.dockApp)
	if *windows != 1 {
		t.Fatalf("window hit queries = %d, want 1", *windows)
	}
	if obs.HitSource != SourceCovered || obs.AXError != 0 || obs.BudgetExceeded {
		t.Fatalf("observation = %+v, want source=covered without error or budget flag", obs)
	}
	if hit := TargetFromObservation(obs); hit.Err != nil || hit.BudgetExceeded || hit.Target.Kind != policy.TargetUnknown {
		t.Fatalf("result = %+v, want an unknown target that passes through", hit)
	}
}

// When the topmost window belongs to Finder, there is no window, or it cannot be determined, the system-wide hit runs as
// usual: desktop icons can only appear at these positions.
func TestDesktopModeAsksSystemWideUnlessCoveredByOtherApp(t *testing.T) {
	for _, owner := range []windowOwner{windowFinder, windowUnknown} {
		ax := newDockInspector(t)
		windows := stubTopWindow(ax, owner)
		hitElement := detachedSystemElement(t)
		scopes := dispatchingHit(t, ax, dockMiss, func() hitOutcome { return hitOutcome{el: hitElement} })

		obs := ax.HitTest(policy.Point{X: 960, Y: 540}, hitDeadline(), InspectActive)

		assertHitScopes(t, "desktop click", *scopes, ax.dockApp, ax.ref)
		if *windows != 1 || obs.HitSource != SourceSystem {
			t.Fatalf("owner %d: window queries = %d, source = %v; want 1 query and a system-wide hit", owner, *windows, obs.HitSource)
		}
	}
}

// With the desktop entry enabled and a Dock element at the point, the window hit is not queried (the Dock sits above
// other apps' windows), and the system-wide hit confirms directly.
func TestDesktopModeDockHitSkipsWindowHit(t *testing.T) {
	ax := newDockInspector(t)
	windows := stubTopWindow(ax, windowOther)
	standIn := detachedSystemElement(t)
	confirmed := detachedSystemElement(t)
	scopes := dispatchingHit(t, ax,
		func() hitOutcome { return hitOutcome{el: standIn} },
		func() hitOutcome { return hitOutcome{el: confirmed} })

	obs := ax.HitTest(policy.Point{X: 30, Y: 390}, hitDeadline(), InspectActive)

	assertHitScopes(t, "dock click", *scopes, ax.dockApp, ax.ref)
	if *windows != 0 || obs.HitSource != SourceSystem {
		t.Fatalf("window queries = %d, source = %v; want no query and a system-wide hit", *windows, obs.HitSource)
	}
}

// Observe mode skips every gate: the system-wide hit is the only hit, and the window hit is not queried.
func TestObserveModeUsesNoGate(t *testing.T) {
	ax := newDockInspector(t)
	windows := stubTopWindow(ax, windowOther)
	hitElement := detachedSystemElement(t)
	scopes := dispatchingHit(t, ax,
		func() hitOutcome {
			t.Error("observe mode must not hit the Dock app element")
			return hitOutcome{}
		},
		func() hitOutcome { return hitOutcome{el: hitElement} })

	ax.HitTest(policy.Point{X: 30, Y: 390}, hitDeadline(), InspectObserve)

	assertHitScopes(t, "observe", *scopes, ax.ref)
	if *windows != 0 {
		t.Fatalf("window hit queries = %d, want 0", *windows)
	}
}

// When the Dock is not running, the process table scan (1 to 3 ms) must not repeat on every event: during the cooldown
// it falls back to the system-wide hit directly, and it looks again once the cooldown expires.
func TestMissingDockIsNotLookedUpOnEveryEvent(t *testing.T) {
	ax, err := NewAXInspector("")
	if err != nil {
		t.Fatalf("NewAXInspector error = %v", err)
	}
	t.Cleanup(ax.Close)
	lookups := 0
	ax.dockPID = func() int32 {
		lookups++
		return 0
	}
	systemHits := 0
	ax.hit = func(scope axElementRef, _ policy.Point, _ uint64) (axElementRef, int, bool) {
		if scope != ax.ref {
			t.Errorf("unexpected hit scope %v without a Dock app element", scope)
		}
		systemHits++
		return 0, axErrNoValue, false
	}

	for i := 0; i < 5; i++ {
		ax.HitTest(policy.Point{X: 960, Y: 540}, hitDeadline(), InspectDock)
	}
	if lookups != 1 || systemHits != 5 {
		t.Fatalf("lookups = %d, system hits = %d; want 1 lookup and a system-wide hit per event", lookups, systemHits)
	}

	ax.dockLookupAfterNS = 0 // cooldown has elapsed
	ax.HitTest(policy.Point{X: 960, Y: 540}, hitDeadline(), InspectDock)
	if lookups != 2 {
		t.Fatalf("lookups after the cooldown = %d, want 2", lookups)
	}
}
