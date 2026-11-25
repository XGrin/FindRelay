package policy

import "math"

const (
	// DefaultCandidateBudgetNS is the hard deadline budget for AX identification of a candidate event: 50 ms.
	// In the steady state after startup warm-up on this machine, the callback p99 for candidate identification and recheck
	// is under 6 ms, with a maximum of 14 ms (measured over three rounds of 500 runs each). The first re-contact after
	// Dock or Finder restarts takes about 20 to 30 ms; the first contact after a cold start is already paid for by the
	// startup warm-up. 50 ms leaves headroom for both; under heavy load, a candidate that exceeds the budget falls back to
	// native Finder rather than making the mouse event wait longer.
	DefaultCandidateBudgetNS uint64 = 50_000_000

	// DefaultMaxClickDurationNS is the maximum press-to-release duration for a plain left click: 500 ms
	// (to avoid accidentally swallowing the Dock long-press menu).
	DefaultMaxClickDurationNS uint64 = 500_000_000

	// DefaultMaxDragDistancePt is the maximum coordinate offset allowed for a plain click: 16.0 pt (tolerates slight
	// movement when sliding onto the Dock to click; the release still rechecks that it is the same icon).
	DefaultMaxDragDistancePt float64 = 16.0

	// DefaultDoubleClickIntervalNS is the default macOS system double-click interval: 500 ms; at runtime it is
	// overridden by the actual system setting.
	DefaultDoubleClickIntervalNS uint64 = 500_000_000

	// defaultMultiClickWindowNS is the Dock Finder multi-click suppression window that immediately follows the first
	// click (1 second).
	defaultMultiClickWindowNS uint64 = 1_000_000_000
)

// State is the current phase of the gesture state machine.
type State uint8

const (
	StateIdle State = iota
	StateCandidate
	StateFirstClickPassed
	StateSecondClickCandidate
	StateConsumedGesture
)

func (s State) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateCandidate:
		return "candidate"
	case StateFirstClickPassed:
		return "first_click_passed"
	case StateSecondClickCandidate:
		return "second_click_candidate"
	case StateConsumedGesture:
		return "consumed_gesture"
	default:
		return "unknown"
	}
}

// Config defines the parameters of the pure Go gesture state machine.
type Config struct {
	DockEnabled           bool
	DockInterceptMode     InterceptMode
	DesktopEnabled        bool
	DesktopInterceptMode  InterceptMode
	CandidateBudgetNS     uint64
	MaxClickDurationNS    uint64
	MaxDragDistancePt     float64
	DoubleClickIntervalNS uint64
	// Snapshots is the background classification and snapshot source for desktop first-click directories; when it is
	// nil, the desktop entry is treated as disabled.
	Snapshots SnapshotSource
}

type activeGesture struct {
	id           uint64
	kind         TargetKind
	downPoint    Point
	downTimeNS   uint64
	identity     string
	path         string // desktop directory candidates only
	expiresNS    uint64 // desktop directory candidates only: monotonic-clock expiry of the first-click snapshot
	reserved     bool
	canceled     bool
	suppressOnly bool              // Dock Finder multi-click (ClickCount >= 2) right after a first-click redirect: only suppresses the native Finder open, does not resubmit the action
	snapshot     DirectorySnapshot // desktop second click only: the verified directory snapshot
}

// Machine is a single-threaded pure Go gesture state machine (owned exclusively by the tap thread).
type Machine struct {
	cfg                Config
	state              State
	nextGestureID      uint64
	draining           bool
	current            activeGesture
	lastDockClickPoint Point
	lastDockClickTime  uint64
	hasRecentDockClick bool
}

// NewMachine creates a gesture state machine initialized in the Idle state.
func NewMachine(cfg Config) *Machine {
	if cfg.CandidateBudgetNS == 0 {
		cfg.CandidateBudgetNS = DefaultCandidateBudgetNS
	}
	if cfg.MaxClickDurationNS == 0 {
		cfg.MaxClickDurationNS = DefaultMaxClickDurationNS
	}
	if cfg.MaxDragDistancePt <= 0 {
		cfg.MaxDragDistancePt = DefaultMaxDragDistancePt
	}
	if cfg.DoubleClickIntervalNS == 0 {
		cfg.DoubleClickIntervalNS = DefaultDoubleClickIntervalNS
	}
	// The desktop entry depends on a snapshot source: without a usable implementation it is treated as disabled.
	cfg.DesktopEnabled = cfg.DesktopEnabled && cfg.Snapshots != nil
	return &Machine{
		cfg:   cfg,
		state: StateIdle,
	}
}

// State returns the current gesture state.
func (m *Machine) State() State {
	return m.state
}

// HasPendingPairing reports whether there is a paired gesture whose press was consumed and whose release is still
// pending.
func (m *Machine) HasPendingPairing() bool {
	return m.state == StateConsumedGesture
}

// BeginDrain enters the graceful-shutdown drain phase: it rejects new interceptions and only lets paired gestures
// whose press was already consumed finish.
func (m *Machine) BeginDrain(reserver CapacityReserver) {
	m.draining = true
	if m.state != StateConsumedGesture {
		m.reset(reserver)
	}
}

// Abort immediately terminates any candidate or paired gesture and releases its reservation (used for tap
// invalidation, permission revocation, sleep/wake, or drain timeout).
func (m *Machine) Abort(reserver CapacityReserver) (interrupted bool) {
	interrupted = m.state == StateConsumedGesture
	m.reset(reserver)
	m.hasRecentDockClick = false
	return interrupted
}

// Handle processes a single mouse event and returns the interception decision; for cases such as tap invalidation
// that must interrupt the current gesture, callers use Abort directly.
// Both the initial identification and the in-gesture rechecks use resolver; callers that need to tell them apart
// should see HandleWith.
func (m *Machine) Handle(ev Event, resolver TargetResolver, reserver CapacityReserver) Decision {
	return m.HandleWith(ev, resolver, resolver, resolver, reserver)
}

// HandleWith is the same as Handle, but splits resolution into three kinds: identify is the gesture's initial
// identification; recheck only needs to confirm that the hit is still the same object as at identification;
// recheckSelected additionally provides single-selection evidence (desktop second click). All three share the same
// HitResult semantics.
func (m *Machine) HandleWith(ev Event, identify, recheck, recheckSelected TargetResolver, reserver CapacityReserver) Decision {
	r := hitResolvers{identify: identify, recheck: recheck, recheckSelected: recheckSelected}
	switch ev.Kind {
	case EventDown:
		return m.handleDown(ev, r, reserver)
	case EventDragged:
		return m.handleDragged(ev, reserver)
	case EventUp:
		return m.handleUp(ev, r, reserver)
	default:
		return Decision{}
	}
}

// hitResolvers holds the three resolvers the state machine uses for one event.
type hitResolvers struct {
	identify        TargetResolver
	recheck         TargetResolver
	recheckSelected TargetResolver
}

func (m *Machine) handleDown(ev Event, r hitResolvers, reserver CapacityReserver) Decision {
	// The desktop first click has already passed: a press with system click-state 2 is evaluated under the
	// second-click rules; every other press (including third and later clicks) clears the old snapshot and is
	// handled as a new gesture, so the old snapshot is never reused.
	if m.state == StateFirstClickPassed && ev.ClickCount == 2 && !m.draining {
		return m.handleDesktopSecondDown(ev, r, reserver)
	}

	// When a new press arrives, first clear any leftover old gesture and reservation.
	if m.state != StateIdle {
		m.reset(reserver)
	}

	if (!m.cfg.DockEnabled && !m.cfg.DesktopEnabled) || m.draining {
		return Decision{}
	}
	if ev.Modifiers.HasAny() {
		m.hasRecentDockClick = false
		return Decision{}
	}

	suppressMultiClick := false
	if ev.ClickCount > 1 {
		// Double and triple clicks in ordinary windows make no AX queries (zero-overhead fast pass-through);
		// only when a Dock Finder first-click redirect has just completed at the same position do we verify and suppress the
		// multi-click that follows, to avoid waking native Finder by mistake.
		if !m.isRecentDockFinderFollowUp(ev) {
			return Decision{}
		}
		suppressMultiClick = true
	}

	if r.identify == nil {
		return Decision{}
	}

	deadlineNS := ev.TimeNS + m.cfg.CandidateBudgetNS
	hit := r.identify(ev.Point, deadlineNS)
	if hit.BudgetExceeded {
		return Decision{OverBudget: true}
	}
	if hit.Err != nil {
		m.hasRecentDockClick = false
		return Decision{}
	}

	switch {
	case m.cfg.DockEnabled && hit.Target.Kind == TargetDockFinder && hit.Target.Identity != "":
		return m.beginDockCandidate(ev, hit.Target.Identity, suppressMultiClick, reserver)
	case m.cfg.DesktopEnabled && ev.ClickCount == 1 &&
		hit.Target.Kind == TargetDesktopDirectory && hit.Target.Identity != "" && hit.Target.Path != "":
		return m.beginDesktopFirstClick(ev, hit.Target)
	}
	m.hasRecentDockClick = false
	return Decision{}
}

func (m *Machine) beginDockCandidate(ev Event, identity string, suppressMultiClick bool, reserver CapacityReserver) Decision {
	m.nextGestureID++
	gid := m.nextGestureID

	if m.cfg.DockInterceptMode == InterceptOnDown {
		if !suppressMultiClick {
			if reserver == nil || !reserver.TryReserve(gid) {
				return Decision{}
			}
		}
		m.state = StateConsumedGesture
		m.current = activeGesture{
			id:           gid,
			kind:         TargetDockFinder,
			downPoint:    ev.Point,
			downTimeNS:   ev.TimeNS,
			identity:     identity,
			reserved:     !suppressMultiClick,
			suppressOnly: suppressMultiClick,
		}
		return Decision{
			GestureID: gid,
			Consume:   true,
		}
	}

	// Default InterceptOnUp: record the candidate and pass the press event through unchanged.
	m.state = StateCandidate
	m.current = activeGesture{
		id:           gid,
		kind:         TargetDockFinder,
		downPoint:    ev.Point,
		downTimeNS:   ev.TimeNS,
		identity:     identity,
		suppressOnly: suppressMultiClick,
	}
	return Decision{
		GestureID: gid,
		Consume:   false,
	}
}

// beginDesktopFirstClick records the desktop first click and passes it through unchanged so that Finder performs
// the native selection; it also starts background directory classification.
func (m *Machine) beginDesktopFirstClick(ev Event, target Target) Decision {
	m.nextGestureID++
	gid := m.nextGestureID
	// The snapshot must cover at least the second-click release after the system double-click interval.
	expiresNS := ev.TimeNS + m.cfg.DoubleClickIntervalNS + m.cfg.MaxClickDurationNS
	m.state = StateCandidate
	m.current = activeGesture{
		id:         gid,
		kind:       TargetDesktopDirectory,
		downPoint:  ev.Point,
		downTimeNS: ev.TimeNS,
		identity:   target.Identity,
		path:       target.Path,
		expiresNS:  expiresNS,
	}
	m.cfg.Snapshots.StartClassification(gid, target.Path, target.Identity, expiresNS)
	return Decision{GestureID: gid}
}

// handleDesktopSecondDown evaluates the desktop second-click press: it intercepts only when the press still hits
// the same object, the position and interval fall within the system double-click conditions, the item is the only
// selected item, and the first-click snapshot is valid; if any condition fails, it clears the snapshot and passes
// the event through on the native path.
func (m *Machine) handleDesktopSecondDown(ev Event, r hitResolvers, reserver CapacityReserver) Decision {
	g := m.current
	if !m.withinDoubleClick(g, ev) {
		return m.abandonDesktop(g, reserver)
	}

	snapshot, overBudget, ok := m.recheckDesktopSecondClick(g, ev, r)
	if !ok {
		dec := m.abandonDesktop(g, reserver)
		dec.OverBudget = overBudget
		return dec
	}

	// Dragging and release for the second click are measured from the second-click press.
	g.snapshot = snapshot
	g.downPoint = ev.Point
	g.downTimeNS = ev.TimeNS

	if m.cfg.DesktopInterceptMode == InterceptOnDown {
		// Press interception: reserve action capacity first, and submit only after the release confirms that the gesture
		// qualifies.
		if reserver == nil || !reserver.TryReserve(g.id) {
			return m.abandonDesktop(g, reserver)
		}
		g.reserved = true
		m.state = StateConsumedGesture
	} else {
		m.state = StateSecondClickCandidate
	}
	m.current = g
	return Decision{
		GestureID: g.id,
		Consume:   g.reserved,
	}
}

// handleDesktopFirstUp rechecks that the target is unchanged when the desktop first click is released, and keeps
// the snapshot and enters FirstClickPassed; the event is always passed through.
func (m *Machine) handleDesktopFirstUp(g activeGesture, ev Event, r hitResolvers, reserver CapacityReserver) Decision {
	if ev.ClickCount != 1 || !m.withinClickBounds(g, ev) || r.recheck == nil {
		return m.abandonDesktop(g, reserver)
	}

	hit := r.recheck(ev.Point, ev.TimeNS+m.cfg.CandidateBudgetNS)
	if hit.BudgetExceeded {
		dec := m.abandonDesktop(g, reserver)
		dec.OverBudget = true
		return dec
	}
	if hit.Err != nil || !sameDesktopObject(hit.Target, g) {
		return m.abandonDesktop(g, reserver)
	}

	m.state = StateFirstClickPassed
	return Decision{GestureID: g.id}
}

// handleDesktopSecondUp rechecks the desktop second click at release-interception time: when the evidence is
// complete and capacity can be reserved, it rewrites this release and submits the action;
// otherwise it passes the event through for Finder to handle natively.
func (m *Machine) handleDesktopSecondUp(g activeGesture, ev Event, r hitResolvers, reserver CapacityReserver) Decision {
	if ev.ClickCount != 2 || !m.withinClickBounds(g, ev) {
		return m.abandonDesktop(g, reserver)
	}

	snapshot, overBudget, ok := m.recheckDesktopSecondClick(g, ev, r)
	if !ok {
		dec := m.abandonDesktop(g, reserver)
		dec.OverBudget = overBudget
		return dec
	}
	if !g.reserved && (reserver == nil || !reserver.TryReserve(g.id)) {
		return m.abandonDesktop(g, reserver)
	}

	// The gesture ends here: clear the state and snapshot; reserved capacity goes to the executor with the action, and
	// the executor reclaims it when it finishes.
	m.cfg.Snapshots.CancelClassification(g.id)
	m.state = StateIdle
	m.current = activeGesture{}
	return Decision{
		GestureID:       g.id,
		Consume:         true,
		CancelDesktopUp: true,
		Action:          desktopOpenAction(g.id, snapshot),
	}
}

// finishDesktopConsumed finishes a consumed desktop second-click press: the paired release is consumed too, and
// the action is submitted only after the release confirms that the gesture qualifies;
// if the gesture does not qualify, the reservation is released and nothing is submitted, and the consumed press
// cannot be restored.
func (m *Machine) finishDesktopConsumed(g activeGesture, ev Event, r hitResolvers, reserver CapacityReserver) Decision {
	m.cfg.Snapshots.CancelClassification(g.id)
	consumed := Decision{GestureID: g.id, Consume: true}

	qualified := !g.canceled && ev.ClickCount == 2 && m.withinClickBounds(g, ev) && r.recheckSelected != nil
	if qualified {
		// With press interception, the snapshot was already verified and saved on the second-click press, so the release
		// only rechecks the hit identity and the single-selection evidence.
		hit := r.recheckSelected(ev.Point, ev.TimeNS+m.cfg.CandidateBudgetNS)
		consumed.OverBudget = hit.BudgetExceeded
		qualified = !hit.BudgetExceeded && hit.Err == nil && sameDesktopObject(hit.Target, g) && hit.Target.SingleSelected
	}
	if !qualified {
		releaseReservation(g, reserver)
		return consumed
	}
	consumed.Action = desktopOpenAction(g.id, g.snapshot)
	return consumed
}

// recheckDesktopSecondClick rechecks at interception time: the current event still hits the same object as the
// first click, that icon is the only selected item in its container,
// and the first-click snapshot is ready, qualified, and still valid.
func (m *Machine) recheckDesktopSecondClick(g activeGesture, ev Event, r hitResolvers) (snapshot DirectorySnapshot, overBudget bool, ok bool) {
	if r.recheckSelected == nil {
		return DirectorySnapshot{}, false, false
	}
	hit := r.recheckSelected(ev.Point, ev.TimeNS+m.cfg.CandidateBudgetNS)
	if hit.BudgetExceeded {
		return DirectorySnapshot{}, true, false
	}
	if hit.Err != nil || !sameDesktopObject(hit.Target, g) || !hit.Target.SingleSelected {
		return DirectorySnapshot{}, false, false
	}
	snapshot, ok = m.cfg.Snapshots.LookupSnapshot(g.id, g.identity, g.path, ev.TimeNS)
	return snapshot, false, ok
}

// sameDesktopObject reports whether the hit result is the same desktop directory object as the first click.
func sameDesktopObject(target Target, g activeGesture) bool {
	return target.Kind == TargetDesktopDirectory && target.Identity == g.identity && target.Path == g.path
}

func desktopOpenAction(gid uint64, snapshot DirectorySnapshot) *Action {
	return &Action{
		GestureID: gid,
		Kind:      ActionOpenDirectory,
		Path:      snapshot.Path,
		Dev:       snapshot.Dev,
		Ino:       snapshot.Ino,
	}
}

// abandonDesktop clears the desktop candidate, reservation and snapshot, and returns a pass-through decision.
func (m *Machine) abandonDesktop(g activeGesture, reserver CapacityReserver) Decision {
	m.reset(reserver)
	return Decision{GestureID: g.id}
}

func (m *Machine) isRecentDockFinderFollowUp(ev Event) bool {
	if !m.hasRecentDockClick || ev.TimeNS < m.lastDockClickTime {
		return false
	}
	if ev.TimeNS-m.lastDockClickTime > defaultMultiClickWindowNS {
		m.hasRecentDockClick = false
		return false
	}
	dx := ev.Point.X - m.lastDockClickPoint.X
	dy := ev.Point.Y - m.lastDockClickPoint.Y
	return math.Hypot(dx, dy) <= m.cfg.MaxDragDistancePt
}

func (m *Machine) handleDragged(ev Event, reserver CapacityReserver) Decision {
	switch m.state {
	case StateCandidate, StateSecondClickCandidate:
		if !m.withinClickBounds(m.current, ev) {
			id := m.current.id
			m.reset(reserver)
			return Decision{GestureID: id}
		}
		return Decision{GestureID: m.current.id}

	case StateConsumedGesture:
		gid := m.current.id
		if !m.current.canceled && !m.withinClickBounds(m.current, ev) {
			m.current.canceled = true
			m.releaseCurrentReservation(reserver)
		}
		return Decision{
			GestureID: gid,
			Consume:   true,
		}

	default:
		return Decision{}
	}
}

func (m *Machine) handleUp(ev Event, r hitResolvers, reserver CapacityReserver) Decision {
	switch m.state {
	case StateCandidate:
		if m.current.kind == TargetDesktopDirectory {
			return m.handleDesktopFirstUp(m.current, ev, r, reserver)
		}
		g := m.current
		m.state = StateIdle
		m.current = activeGesture{}
		return m.finishDockUp(g, ev, r, reserver)

	case StateSecondClickCandidate:
		return m.handleDesktopSecondUp(m.current, ev, r, reserver)

	case StateConsumedGesture:
		g := m.current
		m.state = StateIdle
		m.current = activeGesture{}
		if g.kind == TargetDesktopDirectory {
			return m.finishDesktopConsumed(g, ev, r, reserver)
		}
		return m.finishDockConsumedUp(g, ev, r, reserver)

	default:
		return Decision{}
	}
}

// finishDockUp rechecks the Dock candidate at release-interception time: the press was already passed through
// unchanged; if the release still hits the same Finder icon, it rewrites this release and submits the action;
// a candidate that only suppresses a multi-click just swallows the release.
func (m *Machine) finishDockUp(g activeGesture, ev Event, r hitResolvers, reserver CapacityReserver) Decision {
	if !m.withinClickBounds(g, ev) || (!g.suppressOnly && ev.ClickCount > 1) || r.recheck == nil {
		return Decision{}
	}
	if !g.suppressOnly && reserver == nil {
		return Decision{}
	}

	unchanged, overBudget := m.dockTargetUnchanged(g, ev, r)
	if overBudget {
		return Decision{GestureID: g.id, OverBudget: true}
	}
	if !unchanged {
		return Decision{}
	}

	if g.suppressOnly {
		m.rememberDockClick(ev)
		return Decision{GestureID: g.id, Consume: true, CancelUp: true}
	}
	if !reserver.TryReserve(g.id) {
		return Decision{}
	}
	m.hasRecentDockClick = true
	m.rememberDockClick(ev)
	return Decision{
		GestureID: g.id,
		Consume:   true,
		CancelUp:  true,
		Action: &Action{
			GestureID: g.id,
			Kind:      ActionActivateApp,
		},
	}
}

// finishDockConsumedUp finishes a consumed Dock press: the paired release is consumed too; if the release still
// hits the same Finder icon, the action is submitted,
// otherwise the reservation is released and nothing is submitted; the consumed press cannot be restored.
func (m *Machine) finishDockConsumedUp(g activeGesture, ev Event, r hitResolvers, reserver CapacityReserver) Decision {
	consumed := Decision{GestureID: g.id, Consume: true}
	if g.suppressOnly {
		m.rememberDockClick(ev)
		return consumed
	}

	if g.canceled || !m.withinClickBounds(g, ev) || ev.ClickCount > 1 || r.recheck == nil {
		releaseReservation(g, reserver)
		return consumed
	}

	unchanged, overBudget := m.dockTargetUnchanged(g, ev, r)
	if overBudget || !unchanged || !g.reserved {
		consumed.OverBudget = overBudget
		releaseReservation(g, reserver)
		return consumed
	}

	m.hasRecentDockClick = true
	m.rememberDockClick(ev)
	consumed.Action = &Action{GestureID: g.id, Kind: ActionActivateApp}
	return consumed
}

// dockTargetUnchanged rechecks at release time that the same Dock Finder icon is still hit; exceeding the budget
// is reported separately.
func (m *Machine) dockTargetUnchanged(g activeGesture, ev Event, r hitResolvers) (unchanged, overBudget bool) {
	hit := r.recheck(ev.Point, ev.TimeNS+m.cfg.CandidateBudgetNS)
	if hit.BudgetExceeded {
		return false, true
	}
	return hit.Err == nil && hit.Target.Kind == TargetDockFinder && hit.Target.Identity == g.identity, false
}

// rememberDockClick records the position and time of the Dock click that was just redirected, as the anchor for
// suppressing the multi-click that follows.
func (m *Machine) rememberDockClick(ev Event) {
	m.lastDockClickPoint = ev.Point
	m.lastDockClickTime = ev.TimeNS
}

// withinClickBounds reports whether the event still belongs to the same plain click: no modifier keys, within the
// click duration, and movement within tolerance.
func (m *Machine) withinClickBounds(g activeGesture, ev Event) bool {
	if ev.Modifiers.HasAny() {
		return false
	}
	if ev.TimeNS < g.downTimeNS {
		return false
	}
	if ev.TimeNS-g.downTimeNS >= m.cfg.MaxClickDurationNS {
		return false
	}
	dx := ev.Point.X - g.downPoint.X
	dy := ev.Point.Y - g.downPoint.Y
	return math.Hypot(dx, dy) <= m.cfg.MaxDragDistancePt
}

// withinDoubleClick reports whether the interval and position of the second-click press relative to the
// first-click press both fall within the system double-click conditions, with no modifier keys present.
func (m *Machine) withinDoubleClick(g activeGesture, ev Event) bool {
	if ev.Modifiers.HasAny() {
		return false
	}
	if ev.TimeNS < g.downTimeNS || ev.TimeNS-g.downTimeNS > m.cfg.DoubleClickIntervalNS {
		return false
	}
	dx := ev.Point.X - g.downPoint.X
	dy := ev.Point.Y - g.downPoint.Y
	return math.Hypot(dx, dy) <= m.cfg.MaxDragDistancePt
}

func releaseReservation(g activeGesture, reserver CapacityReserver) {
	if g.reserved && reserver != nil {
		reserver.ReleaseReservation(g.id)
	}
}

func (m *Machine) releaseCurrentReservation(reserver CapacityReserver) {
	releaseReservation(m.current, reserver)
	m.current.reserved = false
}

func (m *Machine) reset(reserver CapacityReserver) {
	m.releaseCurrentReservation(reserver)
	if m.current.kind == TargetDesktopDirectory {
		m.cfg.Snapshots.CancelClassification(m.current.id)
	}
	m.state = StateIdle
	m.current = activeGesture{}
}
