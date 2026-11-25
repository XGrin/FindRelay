package darwin

import (
	"testing"
	"time"

	"github.com/XGrin/findrelay/internal/policy"
)

// kAXErrorCannotComplete.
const axErrorCannotComplete = -25204

var dockFinderObservation = AXObservation{
	PID:          777,
	ProcessKind:  ProcessDock,
	IsDockFinder: true,
	Role:         "AXDockItem",
	Subrole:      "AXApplicationDockItem",
}

// detachedSystemElement returns a real system-wide element reference; ownership passes to the caller.
// Two AXUIElementCreateSystemWide calls return different object pointers, but CFEqual is true.
func detachedSystemElement(t *testing.T) axElementRef {
	t.Helper()
	other, err := NewAXInspector("")
	if err != nil {
		t.Fatalf("NewAXInspector error = %v", err)
	}
	t.Cleanup(other.Close)
	el := other.ref
	other.ref = 0
	return el
}

// newInspectorWithHeld returns a probe that already retains an element, with obs as the retained observation.
// The retained element is a real CF object; ownership passes to the probe, which releases it on Close.
func newInspectorWithHeld(t *testing.T, held axElementRef, obs AXObservation) *AXInspector {
	t.Helper()
	ax, err := NewAXInspector("")
	if err != nil {
		t.Fatalf("NewAXInspector error = %v", err)
	}
	t.Cleanup(ax.Close)
	ax.retain(held, 0, obs)
	return ax
}

// recordingHit replaces the bounded hit implementation with the given result and records the scope of each hit.
func recordingHit(t *testing.T, ax *AXInspector, result func() (axElementRef, int, bool)) *[]axElementRef {
	t.Helper()
	scopes := &[]axElementRef{}
	ax.hit = func(scope axElementRef, _ policy.Point, _ uint64) (axElementRef, int, bool) {
		*scopes = append(*scopes, scope)
		return result()
	}
	return scopes
}

func assertOnlySystemHit(t *testing.T, ax *AXInspector, scopes *[]axElementRef, want int) {
	t.Helper()
	if len(*scopes) != want {
		t.Fatalf("hits = %d, want %d", len(*scopes), want)
	}
	for _, scope := range *scopes {
		if scope != ax.ref {
			t.Fatalf("every hit must be a system-wide hit, got scope %v", scope)
		}
	}
}

// A recheck makes only one system-wide hit: when the hit element is CFEqual to the retained element, it returns the
// observation from identification without resolving attributes again.
func TestRecheckAcceptsSameElementAfterOneSystemHit(t *testing.T) {
	ax := newInspectorWithHeld(t, detachedSystemElement(t), dockFinderObservation)
	el := detachedSystemElement(t)
	scopes := recordingHit(t, ax, func() (axElementRef, int, bool) { return el, 0, false })

	obs := ax.Recheck(policy.Point{X: 500, Y: 1040}, MonotonicNowNS()+uint64(time.Second), false)

	assertOnlySystemHit(t, ax, scopes, 1)
	if obs.HitSource != SourceSame || !obs.IsDockFinder || obs.PID != 777 || obs.Role != "AXDockItem" {
		t.Fatalf("recheck observation = %+v, want the retained Dock Finder observation with source=same", obs)
	}
	hit := TargetFromObservation(obs)
	if hit.Err != nil || hit.Target.Kind != policy.TargetDockFinder || hit.Target.Identity != "dock:777:finder" {
		t.Fatalf("recheck result = %+v, want the same Dock Finder target", hit)
	}
}

// The hit is now a different element: it does not become a candidate target, and no second query is made.
func TestRecheckRejectsDifferentElement(t *testing.T) {
	// Retain the Dock application element while the hit returns the system-wide element: both are real objects, but
	// CFEqual is false.
	donor, err := NewAXInspector("")
	if err != nil {
		t.Fatalf("NewAXInspector error = %v", err)
	}
	t.Cleanup(donor.Close)
	if !donor.ensureDockApp() {
		t.Skip("Dock is not running in this session")
	}
	ax := newInspectorWithHeld(t, donor.dockApp, dockFinderObservation)
	donor.dockApp = 0
	el := detachedSystemElement(t)
	scopes := recordingHit(t, ax, func() (axElementRef, int, bool) { return el, 0, false })

	obs := ax.Recheck(policy.Point{X: 500, Y: 1040}, MonotonicNowNS()+uint64(time.Second), false)

	assertOnlySystemHit(t, ax, scopes, 1)
	if obs.HitSource != SourceChanged {
		t.Fatalf("source = %v, want changed", obs.HitSource)
	}
	if hit := TargetFromObservation(obs); hit.Err != nil || hit.Target.Kind != policy.TargetUnknown {
		t.Fatalf("a changed element must not become a candidate: %+v", hit)
	}
}

// When the hit has no result, the AX error code and the over-budget flag are passed back unchanged, and the state
// machine passes the event through.
func TestRecheckReportsHitFailure(t *testing.T) {
	ax := newInspectorWithHeld(t, detachedSystemElement(t), dockFinderObservation)
	scopes := recordingHit(t, ax, func() (axElementRef, int, bool) { return 0, axErrorCannotComplete, true })

	obs := ax.Recheck(policy.Point{X: 500, Y: 1040}, MonotonicNowNS()+uint64(time.Second), false)

	assertOnlySystemHit(t, ax, scopes, 1)
	if obs.AXError != axErrorCannotComplete || !obs.BudgetExceeded {
		t.Fatalf("recheck observation = %+v, want the hit error and the budget flag", obs)
	}
	if hit := TargetFromObservation(obs); !hit.BudgetExceeded {
		t.Fatalf("recheck result = %+v, want budget exceeded", hit)
	}
}

// Without a retained element, no query is made and the result never becomes a candidate target.
func TestRecheckWithoutRetainedElementDoesNotQuery(t *testing.T) {
	ax, err := NewAXInspector("")
	if err != nil {
		t.Fatalf("NewAXInspector error = %v", err)
	}
	t.Cleanup(ax.Close)
	scopes := recordingHit(t, ax, func() (axElementRef, int, bool) {
		t.Error("recheck without a retained element must not hit")
		return 0, 0, false
	})

	if obs := ax.Recheck(policy.Point{X: 500, Y: 1040}, MonotonicNowNS()+uint64(time.Second), true); obs.HitSource != SourceChanged {
		t.Fatalf("source = %v, want changed", obs.HitSource)
	}
	assertOnlySystemHit(t, ax, scopes, 0)
}

// After the gesture ends, the retained element is released: later rechecks no longer query. Close can be called
// repeatedly, including after EndGesture.
func TestEndGestureReleasesRetainedElement(t *testing.T) {
	ax := newInspectorWithHeld(t, detachedSystemElement(t), dockFinderObservation)
	if !ax.hasHeld {
		t.Fatal("expected a retained element")
	}
	ax.EndGesture()
	ax.EndGesture()
	if ax.hasHeld || ax.held != 0 || ax.heldGroup != 0 {
		t.Fatalf("retained element survived EndGesture: held=%v group=%v", ax.held, ax.heldGroup)
	}

	scopes := recordingHit(t, ax, func() (axElementRef, int, bool) {
		t.Error("no hit expected after the gesture ended")
		return 0, 0, false
	})
	ax.Recheck(policy.Point{X: 500, Y: 1040}, MonotonicNowNS()+uint64(time.Second), false)
	assertOnlySystemHit(t, ax, scopes, 0)
	ax.Close()
}

// A new successful identification replaces the retained element: the previous reference is released, and only the
// one from the latest gesture is kept.
func TestRetainReplacesPreviouslyRetainedElement(t *testing.T) {
	ax, err := NewAXInspector("")
	if err != nil {
		t.Fatalf("NewAXInspector error = %v", err)
	}
	t.Cleanup(ax.Close)
	second := detachedSystemElement(t)

	ax.retain(detachedSystemElement(t), 0, dockFinderObservation)
	ax.retain(second, 0, AXObservation{PID: 778, ProcessKind: ProcessFinder, Role: "AXImage"})

	if ax.held != second || ax.heldObs.PID != 778 {
		t.Fatalf("retained element = %v with observation %+v, want the second identification", ax.held, ax.heldObs)
	}
}
