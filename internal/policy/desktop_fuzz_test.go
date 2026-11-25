package policy

import "testing"

// FuzzDesktopMachineInvariants checks state machine invariants with arbitrary desktop event streams:
// release-interception mode never consumes a press or drag; one gesture ID produces at most one open-directory action;
// no reservation or snapshot is left behind on returning to idle.
func FuzzDesktopMachineInvariants(f *testing.F) {
	f.Add([]byte{0, 1, 0, 10, 0, 3, 0, 15, 1, 1, 20, 0, 2, 2, 0, 5, 1})
	f.Add([]byte{1, 1, 0, 10, 0, 2, 0, 15, 3, 1, 20, 0, 2, 2, 0, 5, 2})
	f.Add([]byte{0, 3, 0, 10, 0, 1, 1, 15, 2, 0, 20, 7, 2, 0, 2, 30, 0})
	f.Fuzz(func(t *testing.T, stream []byte) {
		if len(stream) == 0 {
			return
		}
		mode := InterceptOnUp
		if stream[0]&1 == 1 {
			mode = InterceptOnDown
		}
		const (
			pathA     = "/Users/test/Desktop/A"
			pathB     = "/Users/test/Desktop/B"
			identityA = "desk:7:1:1"
			identityB = "desk:7:1:2"
		)

		snaps := newFakeSnapshots()
		res := newFakeReserver(2)
		m := NewMachine(Config{DesktopEnabled: true, DesktopInterceptMode: mode, Snapshots: snaps})
		emitted := make(map[uint64]int)

		nowNS := ms(100)
		for i := 1; i+3 < len(stream); i += 4 {
			bKind, bFlags, bDelta, bTarget := stream[i], stream[i+1], stream[i+2], stream[i+3]
			nowNS += uint64(bDelta%64) * 10_000_000

			// The fourth input kind simulates tap invalidation: call Abort directly, interrupting any candidate or pairing and
			// releasing the reservation.
			kind := EventDragged
			if bKind%4 == 3 {
				m.Abort(res)
			} else {
				kind = EventKind(bKind%4 + 1)
			}

			// Occasionally change the snapshot state based on the input, simulating late, unqualified, or expired classification.
			switch bTarget % 4 {
			case 1:
				for _, entry := range snaps.entries {
					entry.ready = false
				}
			case 2:
				for _, entry := range snaps.entries {
					entry.eligible = false
				}
			case 3:
				for _, entry := range snaps.entries {
					entry.expiresNS = nowNS
				}
			}

			target := Target{
				Kind:           TargetDesktopDirectory,
				Path:           pathA,
				Identity:       identityA,
				SingleSelected: bTarget&0x10 == 0,
			}
			var forced HitResult
			switch (bTarget >> 5) & 0x03 {
			case 1:
				target.Path, target.Identity = pathB, identityB
			case 2:
				forced = HitResult{Target: Target{Kind: TargetUnknown}}
			case 3:
				forced = HitResult{BudgetExceeded: true}
			}
			resolver := func(_ Point, _ uint64) HitResult {
				if forced != (HitResult{}) {
					return forced
				}
				return HitResult{Target: target}
			}

			clickCount := int(bFlags&0x03) + 1
			d := m.Handle(Event{
				Kind:       kind,
				Point:      Point{X: 500 + float64(int(bKind>>2)-16), Y: 1040 + float64(bFlags>>2&0x1F)},
				ClickCount: clickCount,
				Modifiers:  Modifiers(bFlags & 0x0F),
				TimeNS:     nowNS,
			}, resolver, res)

			if mode == InterceptOnUp && (kind == EventDown || kind == EventDragged) && d.Consume {
				t.Fatalf("InterceptOnUp consumed %v", kind)
			}
			if m.HasPendingPairing() && kind == EventDown {
				t.Fatalf("a down event cannot leave a pairing pending")
			}
			if d.Action != nil {
				if d.Action.Kind != ActionOpenDirectory || d.Action.GestureID == 0 {
					t.Fatalf("unexpected action %+v", *d.Action)
				}
				if !res.reserved[d.Action.GestureID] {
					t.Fatalf("action %+v submitted without a reservation", *d.Action)
				}
				res.commit(d.Action.GestureID)
				emitted[d.Action.GestureID]++
				if emitted[d.Action.GestureID] > 1 {
					t.Fatalf("GestureID %d produced more than one action", d.Action.GestureID)
				}
			}
			if m.State() == StateIdle && (len(res.reserved) != 0 || len(snaps.entries) != 0) {
				t.Fatalf("Idle state leaked %d reservations and %d snapshots", len(res.reserved), len(snaps.entries))
			}
			// Keep submitted actions from filling up the fake's capacity, which would affect reservation checks for later events.
			for id := range res.committed {
				delete(res.committed, id)
			}
		}

		m.Abort(res)
		if m.State() != StateIdle || len(res.reserved) != 0 || len(snaps.entries) != 0 {
			t.Fatalf("Abort left state=%s reservations=%d snapshots=%d", m.State(), len(res.reserved), len(snaps.entries))
		}
	})
}
