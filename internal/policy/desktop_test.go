package policy

import (
	"errors"
	"testing"
)

var errTestLookup = errors.New("lookup failed")

const (
	testDesktopPath = "/Users/test/Desktop/Work"
	testDesktopDev  = 0x1a
	testDesktopIno  = 0x2b
	testDesktopID   = "desk:4321:55:66"
)

// fakeSnapshots is a fake snapshot source that completes synchronously: entries are available right after Start, and
// test cases can mark entries as not ready, unqualified, or expired as needed.
type fakeSnapshots struct {
	entries map[uint64]*fakeSnapshotEntry
	starts  []uint64
	cancels []uint64
}

type fakeSnapshotEntry struct {
	ready     bool
	eligible  bool
	expiresNS uint64
	identity  string
	path      string
}

func newFakeSnapshots() *fakeSnapshots {
	return &fakeSnapshots{entries: make(map[uint64]*fakeSnapshotEntry)}
}

func (s *fakeSnapshots) StartClassification(gestureID uint64, path, identity string, expiresNS uint64) {
	s.entries[gestureID] = &fakeSnapshotEntry{ready: true, eligible: true, expiresNS: expiresNS, identity: identity, path: path}
	s.starts = append(s.starts, gestureID)
}

func (s *fakeSnapshots) LookupSnapshot(gestureID uint64, identity, path string, nowNS uint64) (DirectorySnapshot, bool) {
	entry, exists := s.entries[gestureID]
	if !exists || !entry.ready || !entry.eligible || nowNS >= entry.expiresNS ||
		entry.identity != identity || entry.path != path {
		return DirectorySnapshot{}, false
	}
	return DirectorySnapshot{Path: path, Dev: testDesktopDev, Ino: testDesktopIno}, true
}

func (s *fakeSnapshots) CancelClassification(gestureID uint64) {
	delete(s.entries, gestureID)
	s.cancels = append(s.cancels, gestureID)
}

func desktopHit(identity, path string, selected bool) TargetResolver {
	return func(_ Point, _ uint64) HitResult {
		return HitResult{Target: Target{
			Kind:           TargetDesktopDirectory,
			Path:           path,
			Identity:       identity,
			SingleSelected: selected,
		}}
	}
}

func newDesktopMachine(mode InterceptMode, snaps *fakeSnapshots) *Machine {
	return NewMachine(Config{
		DesktopEnabled:        true,
		DesktopInterceptMode:  mode,
		DoubleClickIntervalNS: ms(500),
		Snapshots:             snaps,
	})
}

func desktopEvent(kind EventKind, atMS uint64, clicks int) Event {
	return Event{Kind: kind, Point: Point{X: 100, Y: 100}, ClickCount: clicks, TimeNS: ms(atMS)}
}

func assertNoAction(t *testing.T, d Decision) {
	t.Helper()
	if d.Consume || d.CancelUp || d.CancelDesktopUp || d.Action != nil {
		t.Fatalf("expected pass without action, got %+v", d)
	}
}

func assertOpenDirectoryAction(t *testing.T, d Decision, gestureID uint64) {
	t.Helper()
	if !d.Consume || d.Action == nil {
		t.Fatalf("expected consumed decision with action, got %+v", d)
	}
	action := d.Action
	if action.GestureID != gestureID || action.Kind != ActionOpenDirectory || action.Path != testDesktopPath ||
		action.Dev != testDesktopDev || action.Ino != testDesktopIno {
		t.Fatalf("unexpected action %+v", *action)
	}
}

func assertClean(t *testing.T, m *Machine, snaps *fakeSnapshots, res *fakeReserver) {
	t.Helper()
	if m.State() != StateIdle {
		t.Fatalf("state = %s, want idle", m.State())
	}
	if len(snaps.entries) != 0 {
		t.Fatalf("leaked %d snapshot entries", len(snaps.entries))
	}
	if len(res.reserved) != 0 {
		t.Fatalf("leaked %d reservations", len(res.reserved))
	}
}

func TestDesktopDoubleClickRedirectsOnceOnUp(t *testing.T) {
	snaps := newFakeSnapshots()
	res := newFakeReserver(4)
	m := newDesktopMachine(InterceptOnUp, snaps)
	hit := desktopHit(testDesktopID, testDesktopPath, true)

	// First click: press and release pass through unchanged, and directory classification starts.
	d := m.Handle(desktopEvent(EventDown, 100, 1), hit, res)
	if d.Consume || d.GestureID == 0 || len(snaps.starts) != 1 {
		t.Fatalf("first down = %+v, starts = %v", d, snaps.starts)
	}
	gid := d.GestureID
	d = m.Handle(desktopEvent(EventUp, 160, 1), hit, res)
	assertNoAction(t, d)
	if m.State() != StateFirstClickPassed {
		t.Fatalf("state after first up = %s, want first_click_passed", m.State())
	}

	// Second click: the press passes through; after the release recheck passes, the release is rewritten into an
	// off-screen release and one OpenDirectory is submitted.
	d = m.Handle(desktopEvent(EventDown, 260, 2), hit, res)
	assertNoAction(t, d)
	if m.State() != StateSecondClickCandidate {
		t.Fatalf("state after second down = %s, want second_click_candidate", m.State())
	}
	d = m.Handle(desktopEvent(EventUp, 320, 2), hit, res)
	if !d.CancelDesktopUp || d.CancelUp {
		t.Fatalf("second up disposition = %+v, want CancelDesktopUp", d)
	}
	assertOpenDirectoryAction(t, d, gid)
	// The reserved capacity goes to the executor with the action and is reclaimed only after the executor submits it.
	res.commit(gid)
	assertClean(t, m, snaps, res)

	// Third and later clicks based on the old snapshot no longer trigger.
	for _, clicks := range []int{3, 4} {
		base := ms(uint64(clicks) * 100)
		dDown := m.Handle(Event{Kind: EventDown, Point: Point{X: 101, Y: 101}, ClickCount: clicks, TimeNS: base}, hit, res)
		dUp := m.Handle(Event{Kind: EventUp, Point: Point{X: 101, Y: 101}, ClickCount: clicks, TimeNS: base + ms(40)}, hit, res)
		assertNoAction(t, dDown)
		assertNoAction(t, dUp)
	}
	if len(snaps.starts) != 1 {
		t.Fatalf("follow-up clicks must not start new classifications, starts = %v", snaps.starts)
	}
	assertClean(t, m, snaps, res)
}

func TestDesktopDoubleClickRedirectsOnceOnDown(t *testing.T) {
	snaps := newFakeSnapshots()
	res := newFakeReserver(4)
	m := newDesktopMachine(InterceptOnDown, snaps)
	hit := desktopHit(testDesktopID, testDesktopPath, true)

	assertNoAction(t, m.Handle(desktopEvent(EventDown, 100, 1), hit, res))
	assertNoAction(t, m.Handle(desktopEvent(EventUp, 160, 1), hit, res))

	// Press interception: the second-click press is consumed and reserves capacity, the paired drag and release are
	// consumed too, and the action is submitted only after the release qualifies.
	d := m.Handle(desktopEvent(EventDown, 260, 2), hit, res)
	if !d.Consume || d.CancelDesktopUp || d.Action != nil || !m.HasPendingPairing() {
		t.Fatalf("second down = %+v, pairing = %v", d, m.HasPendingPairing())
	}
	gid := d.GestureID
	if len(res.reserved) != 1 {
		t.Fatalf("expected 1 reservation after consumed down, got %d", len(res.reserved))
	}

	drag := Event{Kind: EventDragged, Point: Point{X: 103, Y: 102}, ClickCount: 2, TimeNS: ms(280)}
	if d := m.Handle(drag, hit, res); !d.Consume || d.Action != nil {
		t.Fatalf("paired drag = %+v, want consumed without action", d)
	}

	d = m.Handle(desktopEvent(EventUp, 320, 2), hit, res)
	if !d.Consume || d.CancelDesktopUp || d.CancelUp {
		t.Fatalf("second up disposition = %+v, want plain consume", d)
	}
	assertOpenDirectoryAction(t, d, gid)
	// The reserved capacity goes to the executor with the action and is reclaimed only after the executor submits it.
	res.commit(gid)
	assertClean(t, m, snaps, res)
}

func TestDesktopSingleClickAndOutOfScopeEntriesPass(t *testing.T) {
	snaps := newFakeSnapshots()
	res := newFakeReserver(4)
	m := newDesktopMachine(InterceptOnUp, snaps)
	hit := desktopHit(testDesktopID, testDesktopPath, true)

	// Single click on a directory: both events pass through, no action is produced, and the snapshot is kept until the
	// next press.
	assertNoAction(t, m.Handle(desktopEvent(EventDown, 100, 1), hit, res))
	assertNoAction(t, m.Handle(desktopEvent(EventUp, 160, 1), hit, res))
	if m.State() != StateFirstClickPassed || len(snaps.entries) != 1 {
		t.Fatalf("state = %s, entries = %d", m.State(), len(snaps.entries))
	}

	// Then a single click on a regular file (classification unqualified): the first click passes through and replaces
	// the old snapshot, and a double click also passes through unchanged.
	fileHit := desktopHit("desk:4321:55:77", "/Users/test/Desktop/Notes.txt", true)
	assertNoAction(t, m.Handle(desktopEvent(EventDown, 400, 1), fileHit, res))
	assertNoAction(t, m.Handle(desktopEvent(EventUp, 450, 1), fileHit, res))
	for _, id := range snaps.entries {
		id.eligible = false
	}
	assertNoAction(t, m.Handle(desktopEvent(EventDown, 550, 2), fileHit, res))
	assertNoAction(t, m.Handle(desktopEvent(EventUp, 600, 2), fileHit, res))
	assertClean(t, m, snaps, res)
	if len(snaps.starts) != 2 {
		t.Fatalf("starts = %v, want one per first click", snaps.starts)
	}
}

func TestDesktopEntryDisabledIgnoresDesktopHits(t *testing.T) {
	snaps := newFakeSnapshots()
	res := newFakeReserver(4)
	m := NewMachine(Config{DockEnabled: true, DockInterceptMode: InterceptOnUp})
	hit := desktopHit(testDesktopID, testDesktopPath, true)

	assertNoAction(t, m.Handle(desktopEvent(EventDown, 100, 1), hit, res))
	assertNoAction(t, m.Handle(desktopEvent(EventUp, 160, 1), hit, res))
	assertNoAction(t, m.Handle(desktopEvent(EventDown, 260, 2), hit, res))
	assertNoAction(t, m.Handle(desktopEvent(EventUp, 320, 2), hit, res))
	if len(snaps.starts) != 0 {
		t.Fatalf("disabled desktop entry must not classify, starts = %v", snaps.starts)
	}
	assertClean(t, m, snaps, res)

	// When the switch is set but there is no snapshot source, the desktop entry is also treated as disabled.
	noSource := NewMachine(Config{DesktopEnabled: true, Snapshots: nil})
	noSource.Handle(desktopEvent(EventDown, 100, 1), hit, res)
	noSource.Handle(desktopEvent(EventUp, 160, 1), hit, res)
	if noSource.State() != StateIdle {
		t.Fatalf("desktop without snapshot source must stay idle, got %s", noSource.State())
	}
}

func TestDesktopNewSingleClickReplacesPassedFirstClick(t *testing.T) {
	snaps := newFakeSnapshots()
	res := newFakeReserver(4)
	m := newDesktopMachine(InterceptOnUp, snaps)

	iconA := desktopHit("desk:4321:55:1", "/Users/test/Desktop/A", true)
	iconB := desktopHit("desk:4321:55:2", "/Users/test/Desktop/B", true)

	// Click A, then click B within the system double-click interval: A's snapshot is cleared and B becomes the new first
	// click.
	assertNoAction(t, m.Handle(desktopEvent(EventDown, 100, 1), iconA, res))
	assertNoAction(t, m.Handle(desktopEvent(EventUp, 140, 1), iconA, res))
	bDown := Event{Kind: EventDown, Point: Point{X: 400, Y: 100}, ClickCount: 1, TimeNS: ms(300)}
	bUp := Event{Kind: EventUp, Point: Point{X: 400, Y: 100}, ClickCount: 1, TimeNS: ms(340)}
	assertNoAction(t, m.Handle(bDown, iconB, res))
	assertNoAction(t, m.Handle(bUp, iconB, res))
	if len(snaps.entries) != 1 || m.State() != StateFirstClickPassed {
		t.Fatalf("entries = %d, state = %s", len(snaps.entries), m.State())
	}

	bSecondDown := Event{Kind: EventDown, Point: Point{X: 401, Y: 100}, ClickCount: 2, TimeNS: ms(440)}
	bSecondUp := Event{Kind: EventUp, Point: Point{X: 401, Y: 100}, ClickCount: 2, TimeNS: ms(480)}
	assertNoAction(t, m.Handle(bSecondDown, iconB, res))
	d := m.Handle(bSecondUp, iconB, res)
	if !d.CancelDesktopUp || d.Action == nil {
		t.Fatalf("double click on B = %+v, want redirected", d)
	}
	if got := d.Action.Path; got != "/Users/test/Desktop/B" {
		t.Fatalf("action path = %q, want the second icon", got)
	}
	res.commit(d.Action.GestureID)
	assertClean(t, m, snaps, res)
}

func TestDesktopSecondClickFailsOpen(t *testing.T) {
	cases := []struct {
		name  string
		setup func(snaps *fakeSnapshots)
		// mutate rewrites the hit result by resolver call order (1 = first-click press, 2 = first-click release,
		// 3 = second-click press, 4 = second-click release).
		mutate         func(call int, target *Target)
		second         Event // when empty, a default event at the same position as the first click is used
		wantOverBudget bool
	}{
		{name: "different icon on second click", mutate: func(call int, tg *Target) {
			if call >= 3 {
				tg.Identity = "desk:4321:55:2"
			}
		}},
		{name: "finder restart changes object generation", mutate: func(call int, tg *Target) {
			if call >= 3 {
				tg.Identity = "desk:4321:99:66"
			}
		}},
		{name: "same identity resolves to another path", mutate: func(call int, tg *Target) {
			if call >= 3 {
				tg.Path = "/Users/test/Desktop/Other"
			}
		}},
		{name: "multi selection", mutate: func(call int, tg *Target) {
			tg.SingleSelected = call < 3
		}},
		{name: "second click too far", second: Event{Kind: EventDown, Point: Point{X: 140, Y: 140}, ClickCount: 2, TimeNS: ms(260)}},
		{name: "second click beyond double click interval", second: Event{Kind: EventDown, Point: Point{X: 100, Y: 100}, ClickCount: 2, TimeNS: ms(610)}},
		{name: "snapshot not ready", setup: func(snaps *fakeSnapshots) {
			for _, entry := range snaps.entries {
				entry.ready = false
			}
		}},
		{name: "snapshot classified out of scope", setup: func(snaps *fakeSnapshots) {
			for _, entry := range snaps.entries {
				entry.eligible = false
			}
		}},
		{name: "snapshot expired", setup: func(snaps *fakeSnapshots) {
			for _, entry := range snaps.entries {
				entry.expiresNS = ms(200)
			}
		}},
	}

	for _, tc := range cases {
		for _, mode := range []InterceptMode{InterceptOnUp, InterceptOnDown} {
			t.Run(tc.name+"/"+mode.String(), func(t *testing.T) {
				snaps := newFakeSnapshots()
				res := newFakeReserver(4)
				m := newDesktopMachine(mode, snaps)

				calls := 0
				resolver := func(_ Point, _ uint64) HitResult {
					calls++
					hit := desktopHit(testDesktopID, testDesktopPath, true)(Point{}, 0)
					if tc.mutate != nil {
						tc.mutate(calls, &hit.Target)
					}
					return hit
				}

				assertNoAction(t, m.Handle(desktopEvent(EventDown, 100, 1), resolver, res))
				assertNoAction(t, m.Handle(desktopEvent(EventUp, 160, 1), resolver, res))
				if tc.setup != nil {
					tc.setup(snaps)
				}

				secondDown := tc.second
				if secondDown.Kind == 0 {
					secondDown = desktopEvent(EventDown, 260, 2)
				}
				d := m.Handle(secondDown, resolver, res)
				assertNoAction(t, d)
				if d.OverBudget != tc.wantOverBudget {
					t.Fatalf("second down over budget = %v, want %v", d.OverBudget, tc.wantOverBudget)
				}

				secondUp := secondDown
				secondUp.Kind = EventUp
				secondUp.TimeNS += ms(50)
				assertNoAction(t, m.Handle(secondUp, resolver, res))
				assertClean(t, m, snaps, res)
			})
		}
	}
}

func TestDesktopAXFailuresOnSecondClickPass(t *testing.T) {
	cases := []struct {
		name       string
		hit        func(call int) HitResult
		wantBudget bool
	}{
		{name: "over budget on second down", hit: func(call int) HitResult {
			if call == 3 {
				return HitResult{BudgetExceeded: true}
			}
			return desktopHit(testDesktopID, testDesktopPath, true)(Point{}, 0)
		}, wantBudget: true},
		{name: "lookup error on second up", hit: func(call int) HitResult {
			if call == 4 {
				return HitResult{Err: errTestLookup}
			}
			return desktopHit(testDesktopID, testDesktopPath, true)(Point{}, 0)
		}},
		{name: "element without url on second down", hit: func(call int) HitResult {
			if call >= 3 {
				return HitResult{Target: Target{Kind: TargetUnknown}}
			}
			return desktopHit(testDesktopID, testDesktopPath, true)(Point{}, 0)
		}},
	}

	for _, tc := range cases {
		for _, mode := range []InterceptMode{InterceptOnUp, InterceptOnDown} {
			t.Run(tc.name+"/"+mode.String(), func(t *testing.T) {
				snaps := newFakeSnapshots()
				res := newFakeReserver(4)
				m := newDesktopMachine(mode, snaps)
				calls := 0
				resolver := func(_ Point, _ uint64) HitResult {
					calls++
					return tc.hit(calls)
				}

				assertNoAction(t, m.Handle(desktopEvent(EventDown, 100, 1), resolver, res))
				assertNoAction(t, m.Handle(desktopEvent(EventUp, 160, 1), resolver, res))

				// In press-interception mode the second-click press may already have been consumed: the release must be consumed
				// too, and no action may be submitted.
				dDown := m.Handle(desktopEvent(EventDown, 260, 2), resolver, res)
				dUp := m.Handle(desktopEvent(EventUp, 320, 2), resolver, res)
				if dDown.Action != nil || dUp.Action != nil {
					t.Fatalf("unexpected action: down=%+v up=%+v", dDown, dUp)
				}
				if mode == InterceptOnDown && dDown.Consume != dUp.Consume {
					t.Fatalf("consumed down must pair with consumed up: down=%+v up=%+v", dDown, dUp)
				}
				if dDown.OverBudget != tc.wantBudget {
					t.Fatalf("second down over budget = %v, want %v", dDown.OverBudget, tc.wantBudget)
				}
				assertClean(t, m, snaps, res)
			})
		}
	}
}

func TestDesktopModifiersDragAndLongPressBypass(t *testing.T) {
	mod := ModCommand

	t.Run("modifier on first up abandons", func(t *testing.T) {
		snaps := newFakeSnapshots()
		res := newFakeReserver(4)
		m := newDesktopMachine(InterceptOnUp, snaps)
		hit := desktopHit(testDesktopID, testDesktopPath, true)
		assertNoAction(t, m.Handle(desktopEvent(EventDown, 100, 1), hit, res))
		up := desktopEvent(EventUp, 160, 1)
		up.Modifiers = mod
		assertNoAction(t, m.Handle(up, hit, res))
		assertClean(t, m, snaps, res)
	})

	for _, mode := range []InterceptMode{InterceptOnUp, InterceptOnDown} {
		t.Run("modifier on second down/"+mode.String(), func(t *testing.T) {
			snaps := newFakeSnapshots()
			res := newFakeReserver(4)
			m := newDesktopMachine(mode, snaps)
			hit := desktopHit(testDesktopID, testDesktopPath, true)
			assertNoAction(t, m.Handle(desktopEvent(EventDown, 100, 1), hit, res))
			assertNoAction(t, m.Handle(desktopEvent(EventUp, 160, 1), hit, res))
			down := desktopEvent(EventDown, 260, 2)
			down.Modifiers = mod
			assertNoAction(t, m.Handle(down, hit, res))
			assertNoAction(t, m.Handle(desktopEvent(EventUp, 320, 2), hit, res))
			assertClean(t, m, snaps, res)
		})

		t.Run("modifier on second up/"+mode.String(), func(t *testing.T) {
			snaps := newFakeSnapshots()
			res := newFakeReserver(4)
			m := newDesktopMachine(mode, snaps)
			hit := desktopHit(testDesktopID, testDesktopPath, true)
			assertNoAction(t, m.Handle(desktopEvent(EventDown, 100, 1), hit, res))
			assertNoAction(t, m.Handle(desktopEvent(EventUp, 160, 1), hit, res))
			dDown := m.Handle(desktopEvent(EventDown, 260, 2), hit, res)
			up := desktopEvent(EventUp, 320, 2)
			up.Modifiers = mod
			dUp := m.Handle(up, hit, res)
			if dDown.Action != nil || dUp.Action != nil {
				t.Fatalf("modifier must bypass redirect: down=%+v up=%+v", dDown, dUp)
			}
			if mode == InterceptOnDown && (!dDown.Consume || !dUp.Consume) {
				t.Fatalf("consumed down must pair with consumed up: down=%+v up=%+v", dDown, dUp)
			}
			assertClean(t, m, snaps, res)
		})

		t.Run("drag during second click/"+mode.String(), func(t *testing.T) {
			snaps := newFakeSnapshots()
			res := newFakeReserver(4)
			m := newDesktopMachine(mode, snaps)
			hit := desktopHit(testDesktopID, testDesktopPath, true)
			assertNoAction(t, m.Handle(desktopEvent(EventDown, 100, 1), hit, res))
			assertNoAction(t, m.Handle(desktopEvent(EventUp, 160, 1), hit, res))
			m.Handle(desktopEvent(EventDown, 260, 2), hit, res)
			drag := Event{Kind: EventDragged, Point: Point{X: 220, Y: 180}, ClickCount: 2, TimeNS: ms(290)}
			dDrag := m.Handle(drag, hit, res)
			dUp := m.Handle(Event{Kind: EventUp, Point: Point{X: 220, Y: 180}, ClickCount: 2, TimeNS: ms(320)}, hit, res)
			if dDrag.Action != nil || dUp.Action != nil {
				t.Fatalf("drag must not redirect: drag=%+v up=%+v", dDrag.Action, dUp.Action)
			}
			if mode == InterceptOnUp && (dDrag.Consume || dUp.Consume) {
				t.Fatalf("drag and release must reach Finder natively in up mode: drag=%+v up=%+v", dDrag, dUp)
			}
			assertClean(t, m, snaps, res)
		})
	}

	t.Run("drag within tolerance keeps gesture", func(t *testing.T) {
		snaps := newFakeSnapshots()
		res := newFakeReserver(4)
		m := newDesktopMachine(InterceptOnUp, snaps)
		hit := desktopHit(testDesktopID, testDesktopPath, true)
		firstDown := m.Handle(desktopEvent(EventDown, 100, 1), hit, res)
		assertNoAction(t, firstDown)
		drag := Event{Kind: EventDragged, Point: Point{X: 104, Y: 103}, ClickCount: 1, TimeNS: ms(130)}
		assertNoAction(t, m.Handle(drag, hit, res))
		assertNoAction(t, m.Handle(Event{Kind: EventUp, Point: Point{X: 104, Y: 103}, ClickCount: 1, TimeNS: ms(160)}, hit, res))
		assertNoAction(t, m.Handle(desktopEvent(EventDown, 260, 2), hit, res))
		d := m.Handle(desktopEvent(EventUp, 320, 2), hit, res)
		assertOpenDirectoryAction(t, d, firstDown.GestureID)
		res.commit(firstDown.GestureID)
		assertClean(t, m, snaps, res)
	})

	t.Run("long press is not a click", func(t *testing.T) {
		snaps := newFakeSnapshots()
		res := newFakeReserver(4)
		m := newDesktopMachine(InterceptOnUp, snaps)
		hit := desktopHit(testDesktopID, testDesktopPath, true)
		assertNoAction(t, m.Handle(desktopEvent(EventDown, 100, 1), hit, res))
		assertNoAction(t, m.Handle(desktopEvent(EventUp, 700, 1), hit, res))
		assertClean(t, m, snaps, res)
	})
}

func TestDesktopCapacityAndInterrupt(t *testing.T) {
	t.Run("no capacity on second up passes", func(t *testing.T) {
		snaps := newFakeSnapshots()
		res := newFakeReserver(0)
		m := newDesktopMachine(InterceptOnUp, snaps)
		hit := desktopHit(testDesktopID, testDesktopPath, true)
		assertNoAction(t, m.Handle(desktopEvent(EventDown, 100, 1), hit, res))
		assertNoAction(t, m.Handle(desktopEvent(EventUp, 160, 1), hit, res))
		assertNoAction(t, m.Handle(desktopEvent(EventDown, 260, 2), hit, res))
		assertNoAction(t, m.Handle(desktopEvent(EventUp, 320, 2), hit, res))
		assertClean(t, m, snaps, res)
	})

	t.Run("no capacity on second down passes", func(t *testing.T) {
		snaps := newFakeSnapshots()
		res := newFakeReserver(0)
		m := newDesktopMachine(InterceptOnDown, snaps)
		hit := desktopHit(testDesktopID, testDesktopPath, true)
		assertNoAction(t, m.Handle(desktopEvent(EventDown, 100, 1), hit, res))
		assertNoAction(t, m.Handle(desktopEvent(EventUp, 160, 1), hit, res))
		assertNoAction(t, m.Handle(desktopEvent(EventDown, 260, 2), hit, res))
		assertNoAction(t, m.Handle(desktopEvent(EventUp, 320, 2), hit, res))
		assertClean(t, m, snaps, res)
	})

	t.Run("tap disabled during second click", func(t *testing.T) {
		snaps := newFakeSnapshots()
		res := newFakeReserver(4)
		m := newDesktopMachine(InterceptOnUp, snaps)
		hit := desktopHit(testDesktopID, testDesktopPath, true)
		assertNoAction(t, m.Handle(desktopEvent(EventDown, 100, 1), hit, res))
		assertNoAction(t, m.Handle(desktopEvent(EventUp, 160, 1), hit, res))
		assertNoAction(t, m.Handle(desktopEvent(EventDown, 260, 2), hit, res))
		if interrupted := m.Abort(res); interrupted {
			t.Fatalf("no pending pairing, Abort reported interrupted")
		}
		assertClean(t, m, snaps, res)
	})

	t.Run("tap disabled during consumed desktop down", func(t *testing.T) {
		snaps := newFakeSnapshots()
		res := newFakeReserver(4)
		m := newDesktopMachine(InterceptOnDown, snaps)
		hit := desktopHit(testDesktopID, testDesktopPath, true)
		assertNoAction(t, m.Handle(desktopEvent(EventDown, 100, 1), hit, res))
		assertNoAction(t, m.Handle(desktopEvent(EventUp, 160, 1), hit, res))
		m.Handle(desktopEvent(EventDown, 260, 2), hit, res)
		if interrupted := m.Abort(res); !interrupted {
			t.Fatalf("pending desktop pairing must be reported interrupted by Abort")
		}
		assertClean(t, m, snaps, res)
	})

	t.Run("drain rejects new interception and cancels snapshot", func(t *testing.T) {
		snaps := newFakeSnapshots()
		res := newFakeReserver(4)
		m := newDesktopMachine(InterceptOnUp, snaps)
		hit := desktopHit(testDesktopID, testDesktopPath, true)
		assertNoAction(t, m.Handle(desktopEvent(EventDown, 100, 1), hit, res))
		assertNoAction(t, m.Handle(desktopEvent(EventUp, 160, 1), hit, res))
		m.BeginDrain(res)
		assertClean(t, m, snaps, res)
		assertNoAction(t, m.Handle(desktopEvent(EventDown, 260, 1), hit, res))
		assertNoAction(t, m.Handle(desktopEvent(EventUp, 320, 1), hit, res))
		assertClean(t, m, snaps, res)
	})

	t.Run("drain lets consumed desktop pairing finish", func(t *testing.T) {
		snaps := newFakeSnapshots()
		res := newFakeReserver(4)
		m := newDesktopMachine(InterceptOnDown, snaps)
		hit := desktopHit(testDesktopID, testDesktopPath, true)
		assertNoAction(t, m.Handle(desktopEvent(EventDown, 100, 1), hit, res))
		assertNoAction(t, m.Handle(desktopEvent(EventUp, 160, 1), hit, res))
		dDown := m.Handle(desktopEvent(EventDown, 260, 2), hit, res)
		m.BeginDrain(res)
		if !m.HasPendingPairing() {
			t.Fatalf("drain must keep pending pairing")
		}
		d := m.Handle(desktopEvent(EventUp, 320, 2), hit, res)
		assertOpenDirectoryAction(t, d, dDown.GestureID)
		res.commit(dDown.GestureID)
		assertClean(t, m, snaps, res)
	})
}
