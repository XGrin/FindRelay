package policy

import (
	"errors"
	"strings"
	"testing"
)

type fakeReserver struct {
	capacity  int
	reserved  map[uint64]bool
	committed map[uint64]int
}

func newFakeReserver(capacity int) *fakeReserver {
	return &fakeReserver{
		capacity:  capacity,
		reserved:  make(map[uint64]bool),
		committed: make(map[uint64]int),
	}
}

func (r *fakeReserver) TryReserve(gestureID uint64) bool {
	if r.reserved[gestureID] || len(r.reserved)+len(r.committed) >= r.capacity {
		return false
	}
	r.reserved[gestureID] = true
	return true
}

func (r *fakeReserver) ReleaseReservation(gestureID uint64) {
	delete(r.reserved, gestureID)
}

func (r *fakeReserver) commit(gestureID uint64) {
	if !r.reserved[gestureID] {
		panic("commit without active reservation")
	}
	delete(r.reserved, gestureID)
	r.committed[gestureID]++
}

func ms(v uint64) uint64 {
	return v * 1_000_000
}

func dockFinderHit(identity string) TargetResolver {
	return func(_ Point, _ uint64) HitResult {
		return HitResult{
			Target: Target{
				Kind:     TargetDockFinder,
				Identity: identity,
			},
		}
	}
}

func TestDockPlainLeftClickBothModes(t *testing.T) {
	for _, mode := range []InterceptMode{InterceptOnUp, InterceptOnDown} {
		t.Run(mode.String(), func(t *testing.T) {
			res := newFakeReserver(8)
			calls := 0
			resolver := func(pt Point, deadlineNS uint64) HitResult {
				calls++
				if deadlineNS == 0 {
					t.Errorf("expected positive deadlineNS")
				}
				return HitResult{
					Target: Target{Kind: TargetDockFinder, Identity: "dock:42:finder"},
				}
			}

			m := NewMachine(Config{
				DockEnabled:       true,
				DockInterceptMode: mode,
			})

			d1 := m.Handle(Event{
				Kind:       EventDown,
				Point:      Point{X: 500, Y: 1040},
				ClickCount: 1,
				TimeNS:     ms(100),
			}, resolver, res)

			if d1.Action != nil {
				t.Fatalf("Down must not immediately commit action, got %+v", d1)
			}
			if mode == InterceptOnUp && d1.Consume {
				t.Fatalf("InterceptOnUp must PASS Down")
			}
			if mode == InterceptOnDown && !d1.Consume {
				t.Fatalf("InterceptOnDown must CONSUME Down")
			}

			// Allow 1.5pt of micro-jitter during a click with a high-sensitivity mouse.
			dDrag := m.Handle(Event{
				Kind:       EventDragged,
				Point:      Point{X: 501, Y: 1041},
				ClickCount: 1,
				TimeNS:     ms(130),
			}, resolver, res)
			if mode == InterceptOnUp && dDrag.Consume {
				t.Fatalf("InterceptOnUp must PASS micro-jitter Dragged")
			}
			if mode == InterceptOnDown && !dDrag.Consume {
				t.Fatalf("InterceptOnDown must CONSUME paired Dragged")
			}

			d2 := m.Handle(Event{
				Kind:       EventUp,
				Point:      Point{X: 501, Y: 1041},
				ClickCount: 1,
				TimeNS:     ms(180),
			}, resolver, res)

			if !d2.Consume {
				t.Fatalf("Up must be consumed on qualified Dock Finder click")
			}
			if d2.Action == nil || d2.Action.Kind != ActionActivateApp {
				t.Fatalf("expected ActionActivateApp on Up, got %+v", d2)
			}
			if d2.Action.GestureID == 0 || d2.Action.GestureID != d1.GestureID {
				t.Fatalf("mismatched GestureID: down=%d up=%d", d1.GestureID, d2.Action.GestureID)
			}
			res.commit(d2.Action.GestureID)

			if calls != 2 {
				t.Fatalf("resolver calls = %d, want 2 (down + up)", calls)
			}
			if len(res.reserved) != 0 {
				t.Fatalf("leaked reservations: %+v", res.reserved)
			}
			if m.State() != StateIdle {
				t.Fatalf("final state = %v, want StateIdle", m.State())
			}
		})
	}
}

func TestNonDockFinderAndIdleFastPass(t *testing.T) {
	res := newFakeReserver(8)
	calls := 0
	unknownResolver := func(_ Point, _ uint64) HitResult {
		calls++
		return HitResult{Target: Target{Kind: TargetUnknown}}
	}

	m := NewMachine(Config{DockEnabled: true, DockInterceptMode: InterceptOnUp})

	// Plain Dragged / Up events in Idle must not trigger any AX query.
	for _, kind := range []EventKind{EventDragged, EventUp} {
		d := m.Handle(Event{Kind: kind, Point: Point{X: 200, Y: 200}, ClickCount: 1, TimeNS: ms(10)}, unknownResolver, res)
		if d.Consume || d.Action != nil {
			t.Fatalf("Idle %v should PASS, got %+v", kind, d)
		}
	}
	if calls != 0 {
		t.Fatalf("Idle Dragged/Up triggered %d AX calls, want 0", calls)
	}

	// Other Dock icons or ordinary Finder windows.
	dDown := m.Handle(Event{Kind: EventDown, Point: Point{X: 600, Y: 1040}, ClickCount: 1, TimeNS: ms(20)}, unknownResolver, res)
	dUp := m.Handle(Event{Kind: EventUp, Point: Point{X: 600, Y: 1040}, ClickCount: 1, TimeNS: ms(80)}, unknownResolver, res)
	if dDown.Consume || dUp.Consume || dDown.Action != nil || dUp.Action != nil {
		t.Fatalf("other target should PASS completely: down=%+v up=%+v", dDown, dUp)
	}
	if calls != 1 {
		t.Fatalf("expected 1 AX call (on Down only), got %d", calls)
	}

	// When Desktop is disabled, the event must pass through even if it hits TargetDesktopDirectory.
	desktopResolver := func(_ Point, _ uint64) HitResult {
		return HitResult{Target: Target{Kind: TargetDesktopDirectory, Path: "/Users/test/Desktop/Work", Identity: "desk:1"}}
	}
	dDeskDown := m.Handle(Event{Kind: EventDown, Point: Point{X: 100, Y: 100}, ClickCount: 1, TimeNS: ms(100)}, desktopResolver, res)
	dDeskUp := m.Handle(Event{Kind: EventUp, Point: Point{X: 100, Y: 100}, ClickCount: 1, TimeNS: ms(160)}, desktopResolver, res)
	if dDeskDown.Consume || dDeskUp.Consume || dDeskUp.Action != nil {
		t.Fatalf("desktop click with DesktopEnabled=false must PASS")
	}
}

func TestDifferentTargetIdentityOnUp(t *testing.T) {
	for _, mode := range []InterceptMode{InterceptOnUp, InterceptOnDown} {
		t.Run(mode.String(), func(t *testing.T) {
			res := newFakeReserver(8)
			m := NewMachine(Config{DockEnabled: true, DockInterceptMode: mode})

			m.Handle(Event{Kind: EventDown, Point: Point{X: 500, Y: 1040}, ClickCount: 1, TimeNS: ms(100)}, dockFinderHit("dock:100:finder"), res)
			// Simulate a Dock restart or a changed hit identity at release time.
			dUp := m.Handle(Event{Kind: EventUp, Point: Point{X: 500, Y: 1040}, ClickCount: 1, TimeNS: ms(160)}, dockFinderHit("dock:205:finder"), res)

			if dUp.Action != nil {
				t.Fatalf("expected nil action when identity changed, got %+v", dUp.Action)
			}
			if mode == InterceptOnUp && dUp.Consume {
				t.Fatalf("InterceptOnUp must PASS when identity mismatched on Up")
			}
			if mode == InterceptOnDown && !dUp.Consume {
				t.Fatalf("InterceptOnDown must still consume paired Up")
			}
			if len(res.reserved) != 0 {
				t.Fatalf("reservation leaked after identity mismatch: %+v", res.reserved)
			}
		})
	}
}

func TestModifiersDragLongPressAndMultiClick(t *testing.T) {
	for _, mode := range []InterceptMode{InterceptOnUp, InterceptOnDown} {
		t.Run(mode.String(), func(t *testing.T) {
			res := newFakeReserver(8)
			m := NewMachine(Config{DockEnabled: true, DockInterceptMode: mode})

			// 1. Modifier key held at press time: no AX call, full pass-through.
			for _, mod := range []Modifiers{ModCommand, ModOption, ModControl, ModShift} {
				calls := 0
				countingResolver := func(_ Point, _ uint64) HitResult {
					calls++
					return HitResult{Target: Target{Kind: TargetDockFinder, Identity: "dock:1:finder"}}
				}
				d1 := m.Handle(Event{Kind: EventDown, Point: Point{X: 500, Y: 1040}, ClickCount: 1, Modifiers: mod, TimeNS: ms(10)}, countingResolver, res)
				d2 := m.Handle(Event{Kind: EventUp, Point: Point{X: 500, Y: 1040}, ClickCount: 1, Modifiers: mod, TimeNS: ms(50)}, countingResolver, res)
				if d1.Consume || d2.Consume || d2.Action != nil || calls != 0 {
					t.Fatalf("modifier %v on Down must fast-PASS with 0 AX calls: d1=%+v d2=%+v calls=%d", mod, d1, d2, calls)
				}
			}

			// 2. Modifier key added midway (no modifier at press, Command held at release).
			m.Handle(Event{Kind: EventDown, Point: Point{X: 500, Y: 1040}, ClickCount: 1, TimeNS: ms(100)}, dockFinderHit("dock:1:finder"), res)
			dMidMod := m.Handle(Event{Kind: EventUp, Point: Point{X: 500, Y: 1040}, ClickCount: 1, Modifiers: ModCommand, TimeNS: ms(150)}, dockFinderHit("dock:1:finder"), res)
			if dMidMod.Action != nil || len(res.reserved) != 0 {
				t.Fatalf("mid-click modifier must cancel action and free reservation: %+v, reserved=%v", dMidMod, res.reserved)
			}
			if mode == InterceptOnUp && dMidMod.Consume {
				t.Fatalf("InterceptOnUp must PASS Up with modifier")
			}
			if mode == InterceptOnDown && !dMidMod.Consume {
				t.Fatalf("InterceptOnDown must pair-consume Up when Down was consumed")
			}

			// 3. Drag exceeds the tolerance (24pt > 16pt).
			m.Handle(Event{Kind: EventDown, Point: Point{X: 500, Y: 1040}, ClickCount: 1, TimeNS: ms(200)}, dockFinderHit("dock:1:finder"), res)
			dDrag := m.Handle(Event{Kind: EventDragged, Point: Point{X: 524, Y: 1040}, ClickCount: 1, TimeNS: ms(240)}, dockFinderHit("dock:1:finder"), res)
			if len(res.reserved) != 0 {
				t.Fatalf("drag beyond threshold must release reservation immediately")
			}
			dAfterDrag := m.Handle(Event{Kind: EventUp, Point: Point{X: 524, Y: 1040}, ClickCount: 1, TimeNS: ms(280)}, dockFinderHit("dock:1:finder"), res)
			if dDrag.Action != nil || dAfterDrag.Action != nil {
				t.Fatalf("drag gesture must not emit action")
			}
			if mode == InterceptOnDown && (!dDrag.Consume || !dAfterDrag.Consume) {
				t.Fatalf("InterceptOnDown must pair-consume Dragged and Up")
			}

			// 4. Long press (600ms >= 500ms default threshold).
			m.Handle(Event{Kind: EventDown, Point: Point{X: 500, Y: 1040}, ClickCount: 1, TimeNS: ms(300)}, dockFinderHit("dock:1:finder"), res)
			dLongUp := m.Handle(Event{Kind: EventUp, Point: Point{X: 500, Y: 1040}, ClickCount: 1, TimeNS: ms(900)}, dockFinderHit("dock:1:finder"), res)
			if dLongUp.Action != nil || len(res.reserved) != 0 {
				t.Fatalf("long-press must not emit action or leak reservation: %+v", dLongUp)
			}

			// 5. Multi-click (double click ClickCount=2, triple click ClickCount=3): the Dock single-click entry does not
			// trigger again.
			for _, cc := range []int{2, 3} {
				calls := 0
				r := func(_ Point, _ uint64) HitResult {
					calls++
					return HitResult{Target: Target{Kind: TargetDockFinder, Identity: "dock:1:finder"}}
				}
				d1 := m.Handle(Event{Kind: EventDown, Point: Point{X: 500, Y: 1040}, ClickCount: cc, TimeNS: ms(1000)}, r, res)
				d2 := m.Handle(Event{Kind: EventUp, Point: Point{X: 500, Y: 1040}, ClickCount: cc, TimeNS: ms(1050)}, r, res)
				if d1.Consume || d2.Consume || d2.Action != nil || calls != 0 {
					t.Fatalf("ClickCount=%d on Dock must PASS with 0 AX calls", cc)
				}
			}
		})
	}
}

func TestQueueFullAXErrorAndOverBudgetFailOpen(t *testing.T) {
	for _, mode := range []InterceptMode{InterceptOnUp, InterceptOnDown} {
		t.Run(mode.String(), func(t *testing.T) {
			// Queue is full (capacity 0).
			fullRes := newFakeReserver(0)
			m := NewMachine(Config{DockEnabled: true, DockInterceptMode: mode})
			d1 := m.Handle(Event{Kind: EventDown, Point: Point{X: 500, Y: 1040}, ClickCount: 1, TimeNS: ms(100)}, dockFinderHit("dock:1:finder"), fullRes)
			d2 := m.Handle(Event{Kind: EventUp, Point: Point{X: 500, Y: 1040}, ClickCount: 1, TimeNS: ms(150)}, dockFinderHit("dock:1:finder"), fullRes)
			if d1.Consume || d2.Consume || d1.Action != nil || d2.Action != nil {
				t.Fatalf("full queue must PASS all unconsumed events: d1=%+v d2=%+v", d1, d2)
			}

			// AX error passes through.
			res := newFakeReserver(8)
			errResolver := func(_ Point, _ uint64) HitResult {
				return HitResult{Err: errors.New("ax cannot complete")}
			}
			d1 = m.Handle(Event{Kind: EventDown, Point: Point{X: 500, Y: 1040}, ClickCount: 1, TimeNS: ms(200)}, errResolver, res)
			d2 = m.Handle(Event{Kind: EventUp, Point: Point{X: 500, Y: 1040}, ClickCount: 1, TimeNS: ms(250)}, errResolver, res)
			if d1.Consume || d2.Consume || d2.Action != nil {
				t.Fatalf("AX error must PASS")
			}

			// Budget timeout passes through and is marked OverBudget.
			overBudgetResolver := func(_ Point, _ uint64) HitResult {
				return HitResult{
					Target:         Target{Kind: TargetDockFinder, Identity: "dock:1:finder"},
					BudgetExceeded: true,
				}
			}
			d1 = m.Handle(Event{Kind: EventDown, Point: Point{X: 500, Y: 1040}, ClickCount: 1, TimeNS: ms(300)}, overBudgetResolver, res)
			if d1.Consume || !d1.OverBudget || d1.Action != nil {
				t.Fatalf("expected OverBudget PASS on Down, got %+v", d1)
			}
		})
	}
}

func TestTapDisabledDrainAndMissingUpRecovery(t *testing.T) {
	res := newFakeReserver(8)
	m := NewMachine(Config{DockEnabled: true, DockInterceptMode: InterceptOnDown})

	// 1. Tap invalidation (Abort) during ConsumedGesture: interrupt the pairing and release the reservation.
	dDown := m.Handle(Event{Kind: EventDown, Point: Point{X: 500, Y: 1040}, ClickCount: 1, TimeNS: ms(100)}, dockFinderHit("dock:1:finder"), res)
	if !dDown.Consume || len(res.reserved) != 1 {
		t.Fatalf("expected consumed Down with 1 reservation")
	}
	if interrupted := m.Abort(res); !interrupted || len(res.reserved) != 0 || m.State() != StateIdle {
		t.Fatalf("Abort must interrupt pairing and release reservation: interrupted=%v state=%v", interrupted, m.State())
	}

	// 2. A lost Up followed immediately by the next normal non-Finder click: it must not pollute later clicks.
	m.Handle(Event{Kind: EventDown, Point: Point{X: 500, Y: 1040}, ClickCount: 1, TimeNS: ms(200)}, dockFinderHit("dock:1:finder"), res)
	if len(res.reserved) != 1 {
		t.Fatalf("expected 1 reservation")
	}
	otherResolver := func(_ Point, _ uint64) HitResult {
		return HitResult{Target: Target{Kind: TargetUnknown}}
	}
	dNextDown := m.Handle(Event{Kind: EventDown, Point: Point{X: 100, Y: 100}, ClickCount: 1, TimeNS: ms(500)}, otherResolver, res)
	dNextUp := m.Handle(Event{Kind: EventUp, Point: Point{X: 100, Y: 100}, ClickCount: 1, TimeNS: ms(550)}, otherResolver, res)
	if dNextDown.Consume || dNextUp.Consume || len(res.reserved) != 0 {
		t.Fatalf("missing Up must clean old reservation and not poison next click: down=%+v up=%+v res=%v", dNextDown, dNextUp, res.reserved)
	}

	// 3. Graceful shutdown BeginDrain: no new presses are accepted, but an in-progress ConsumedGesture may complete
	// its release pairing.
	m.Handle(Event{Kind: EventDown, Point: Point{X: 500, Y: 1040}, ClickCount: 1, TimeNS: ms(600)}, dockFinderHit("dock:1:finder"), res)
	if !m.HasPendingPairing() {
		t.Fatalf("expected pending pairing in ConsumedGesture")
	}
	m.BeginDrain(res)
	if !m.HasPendingPairing() {
		t.Fatalf("BeginDrain should keep ConsumedGesture pairing alive until Up")
	}
	dDrainUp := m.Handle(Event{Kind: EventUp, Point: Point{X: 500, Y: 1040}, ClickCount: 1, TimeNS: ms(660)}, dockFinderHit("dock:1:finder"), res)
	if !dDrainUp.Consume || dDrainUp.Action == nil {
		t.Fatalf("paired Up during drain should finish cleanly, got %+v", dDrainUp)
	}
	res.commit(dDrainUp.Action.GestureID)
	if m.HasPendingPairing() {
		t.Fatalf("expected no pending pairing after Up")
	}

	// A new press in the Drain state passes through directly.
	dBlocked := m.Handle(Event{Kind: EventDown, Point: Point{X: 500, Y: 1040}, ClickCount: 1, TimeNS: ms(700)}, dockFinderHit("dock:1:finder"), res)
	if dBlocked.Consume {
		t.Fatalf("new Down while draining must PASS")
	}
}

func FuzzMachineInvariants(f *testing.F) {
	f.Add([]byte{0, 1, 0, 10, 2, 0, 15, 1, 1, 20, 3, 0, 5})
	f.Add([]byte{1, 1, 0, 10, 3, 0, 2, 2, 0, 10, 4, 0, 1})
	f.Fuzz(func(t *testing.T, stream []byte) {
		if len(stream) == 0 {
			return
		}
		mode := InterceptOnUp
		if stream[0]&1 == 1 {
			mode = InterceptOnDown
		}
		res := newFakeReserver(2)
		m := NewMachine(Config{DockEnabled: true, DockInterceptMode: mode})

		nowNS := ms(100)
		for i := 1; i+2 < len(stream); i += 3 {
			bKind := stream[i]
			bFlags := stream[i+1]
			bDelta := stream[i+2]
			nowNS += uint64(bDelta) * 5_000_000 // 0..1.275s increments

			// The fourth input kind simulates tap invalidation: call Abort directly, interrupting any candidate or pairing and
			// releasing the reservation.
			kind := EventDragged
			if bKind%4 == 3 {
				m.Abort(res)
			} else {
				kind = EventKind(bKind%4 + 1)
			}

			mods := Modifiers(bFlags & 0x0F)
			clickCount := int((bFlags>>4)&0x03) + 1
			hitMode := (bFlags >> 6) & 0x03

			pt := Point{
				X: 500 + float64(int(bKind>>2)-16),
				Y: 1040,
			}

			resolver := func(_ Point, _ uint64) HitResult {
				switch hitMode {
				case 0:
					return HitResult{Target: Target{Kind: TargetDockFinder, Identity: "dock:1:finder"}}
				case 1:
					return HitResult{Target: Target{Kind: TargetDockFinder, Identity: "dock:2:finder"}}
				case 2:
					return HitResult{Target: Target{Kind: TargetUnknown}}
				default:
					return HitResult{BudgetExceeded: true}
				}
			}

			d := m.Handle(Event{
				Kind:       kind,
				Point:      pt,
				ClickCount: clickCount,
				Modifiers:  mods,
				TimeNS:     nowNS,
			}, resolver, res)

			if mode == InterceptOnUp && (kind == EventDown || kind == EventDragged) && d.Consume {
				t.Fatalf("InterceptOnUp consumed %v", kind)
			}
			if d.Action != nil {
				res.commit(d.Action.GestureID)
				if res.committed[d.Action.GestureID] > 1 {
					t.Fatalf("GestureID %d committed more than once", d.Action.GestureID)
				}
			}
			if m.State() == StateIdle && len(res.reserved) != 0 {
				var ids []string
				for id := range res.reserved {
					ids = append(ids, string(rune(id)))
				}
				t.Fatalf("Idle state leaked %d reservations (%s)", len(res.reserved), strings.Join(ids, ","))
			}
			if len(res.reserved) > 1 {
				t.Fatalf("more than 1 concurrent reservation held by single gesture machine: %d", len(res.reserved))
			}
			// Prevent fakeReserver.committed from exhausting capacity, which would make later new reservations untestable.
			for k := range res.committed {
				delete(res.committed, k)
			}
		}
	})
}

func TestDockFinderRapidMultiClickSuppressesExtraLaunchAndFinder(t *testing.T) {
	for _, mode := range []InterceptMode{InterceptOnUp, InterceptOnDown} {
		t.Run(mode.String(), func(t *testing.T) {
			res := newFakeReserver(8)
			m := NewMachine(Config{DockEnabled: true, DockInterceptMode: mode})

			// Click 1: triggers ActivateApp exactly once as normal.
			d1Down := m.Handle(Event{Kind: EventDown, Point: Point{X: 40, Y: 200}, ClickCount: 1, TimeNS: ms(100)}, dockFinderHit("dock:1:finder"), res)
			d1Up := m.Handle(Event{Kind: EventUp, Point: Point{X: 40, Y: 200}, ClickCount: 1, TimeNS: ms(150)}, dockFinderHit("dock:1:finder"), res)
			if !d1Up.Consume || d1Up.Action == nil {
				t.Fatalf("click 1 expected consume+action, got down=%+v up=%+v", d1Down, d1Up)
			}
			res.commit(d1Up.Action.GestureID)

			// Clicks 2 and 3 that follow immediately: consume/cancel the native click to stop a Finder window from opening, but
			// do not trigger ActivateApp again.
			for cc := 2; cc <= 3; cc++ {
				tBase := ms(uint64(cc * 100))
				dDown := m.Handle(Event{Kind: EventDown, Point: Point{X: 41, Y: 201}, ClickCount: cc, TimeNS: tBase}, dockFinderHit("dock:1:finder"), res)
				dUp := m.Handle(Event{Kind: EventUp, Point: Point{X: 41, Y: 201}, ClickCount: cc, TimeNS: tBase + ms(40)}, dockFinderHit("dock:1:finder"), res)
				if !dUp.Consume || dUp.Action != nil {
					t.Fatalf("follow-up click %d expected suppressed without action, got down=%+v up=%+v", cc, dDown, dUp)
				}
				if mode == InterceptOnUp && !dUp.CancelUp {
					t.Fatalf("follow-up click %d in InterceptOnUp expected CancelUp=true", cc)
				}
			}
			if len(res.reserved) != 0 {
				t.Fatalf("leaked reservations: %+v", res.reserved)
			}
		})
	}
}

func TestThirtyMsRecontactWithinDefaultBudget(t *testing.T) {
	res := newFakeReserver(8)
	m := NewMachine(Config{DockEnabled: true, DockInterceptMode: InterceptOnDown})

	// Reproduce an on-device measurement: the first AX re-contact after a Dock restart takes ~30ms and must hit
	// normally rather than be judged over budget.
	resolver := func(_ Point, deadlineNS uint64) HitResult {
		simulatedNow := ms(100) + ms(30)
		if simulatedNow >= deadlineNS {
			return HitResult{BudgetExceeded: true}
		}
		return HitResult{Target: Target{Kind: TargetDockFinder, Identity: "dock:88230:finder"}}
	}

	dDown := m.Handle(Event{Kind: EventDown, Point: Point{X: 60, Y: 1040}, ClickCount: 1, TimeNS: ms(100)}, resolver, res)
	if !dDown.Consume || dDown.OverBudget {
		t.Fatalf("expected 30ms Dock AX hit to succeed within the default budget, got %+v", dDown)
	}
	dUp := m.Handle(Event{Kind: EventUp, Point: Point{X: 64, Y: 1042}, ClickCount: 1, TimeNS: ms(170)}, resolver, res)
	if !dUp.Consume || dUp.Action == nil || dUp.Action.Kind != ActionActivateApp {
		t.Fatalf("expected ActivateApp on Up, got %+v", dUp)
	}
}

func TestCandidateBudgetDefaultRejectsSlowIdentification(t *testing.T) {
	if DefaultCandidateBudgetNS != 50_000_000 {
		t.Fatalf("expected the default candidate budget to be 50ms, got %d", DefaultCandidateBudgetNS)
	}
	res := newFakeReserver(8)
	m := NewMachine(Config{DockEnabled: true, DockInterceptMode: InterceptOnDown})

	// Identification takes 60ms, exceeding the 50 ms default budget: the press must pass through unchanged, and no
	// action may be submitted for this candidate.
	slow := func(_ Point, deadlineNS uint64) HitResult {
		if ms(100)+ms(60) >= deadlineNS {
			return HitResult{BudgetExceeded: true}
		}
		return HitResult{Target: Target{Kind: TargetDockFinder, Identity: "dock:88230:finder"}}
	}

	dDown := m.Handle(Event{Kind: EventDown, Point: Point{X: 60, Y: 1040}, ClickCount: 1, TimeNS: ms(100)}, slow, res)
	if dDown.Consume || !dDown.OverBudget || dDown.Action != nil {
		t.Fatalf("expected a 60ms identification to pass the event through over budget, got %+v", dDown)
	}
	dUp := m.Handle(Event{Kind: EventUp, Point: Point{X: 64, Y: 1042}, ClickCount: 1, TimeNS: ms(170)}, slow, res)
	if dUp.Consume || dUp.Action != nil {
		t.Fatalf("expected the Up after an over-budget identification to pass through, got %+v", dUp)
	}
}

func BenchmarkOrdinaryNonCandidateFilter(b *testing.B) {
	res := newFakeReserver(8)
	m := NewMachine(Config{DockEnabled: true, DockInterceptMode: InterceptOnUp})
	unknownResolver := func(_ Point, _ uint64) HitResult {
		return HitResult{Target: Target{Kind: TargetUnknown}}
	}

	b.ReportAllocs()
	var ts uint64
	for b.Loop() {
		ts += 1_000_000
		m.Handle(Event{Kind: EventDown, Point: Point{X: 300, Y: 300}, ClickCount: 1, TimeNS: ts}, unknownResolver, res)
		m.Handle(Event{Kind: EventDragged, Point: Point{X: 310, Y: 305}, ClickCount: 1, TimeNS: ts + 100_000}, unknownResolver, res)
		m.Handle(Event{Kind: EventUp, Point: Point{X: 310, Y: 305}, ClickCount: 1, TimeNS: ts + 200_000}, unknownResolver, res)
	}
}
