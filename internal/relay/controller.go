package relay

import (
	"sync"
	"time"

	"github.com/XGrin/findrelay/internal/darwin"
	"github.com/XGrin/findrelay/internal/diagnostics"
	"github.com/XGrin/findrelay/internal/policy"
)

// HitProbe abstracts the underlying AX hit capability so that test doubles can be injected.
type HitProbe interface {
	// HitTest identifies the target at pt: it makes one hit and resolves attributes.
	HitTest(pt policy.Point, deadlineNS uint64, mode darwin.InspectMode) darwin.AXObservation
	// Recheck confirms that the hit at pt is still the element identified for the current gesture, without rereading
	// attributes; with needSelection it also returns single-selection evidence.
	Recheck(pt policy.Point, deadlineNS uint64, needSelection bool) darwin.AXObservation
	// EndGesture ends the current gesture and releases the element reference retained on successful identification; it
	// costs nothing when no reference is retained.
	EndGesture()
}

// TapEnabler abstracts re-enabling the EventTap so that the timeout recovery logic can be tested.
type TapEnabler interface {
	SetEnabled(enabled bool) bool
}

// ControllerConfig configures the event orchestration controller.
type ControllerConfig struct {
	ObserveOnly       bool
	DockEnabled       bool
	DockInterceptMode policy.InterceptMode
	// DesktopEnabled enables redirection of native desktop folder double-clicks; the entry has no effect in observe mode
	// or when Snapshots is nil.
	DesktopEnabled        bool
	DesktopInterceptMode  policy.InterceptMode
	DoubleClickIntervalNS uint64
	// Snapshots provides background classification of desktop first clicks; when it is nil, the desktop entry has no
	// effect.
	Snapshots policy.SnapshotSource
	Probe     HitProbe
	// Executor being nil means there is no action executor, and then no event is intercepted.
	Executor    *Executor
	Recorder    *diagnostics.Recorder
	NowNS       func() uint64
	IsTrusted   func() bool
	OnFatalStop func(reason string)
}

// Controller connects the native EventTap callbacks, the pure-Go state machine, the AX probe and the asynchronous
// action executor.
type Controller struct {
	observeOnly bool
	inspectMode darwin.InspectMode
	budgetNS    uint64
	probe       HitProbe
	executor    *Executor
	// reserver is the capacity reserver handed to the state machine; it stays a nil interface when the executor is nil,
	// and the state machine then passes events through directly.
	reserver    policy.CapacityReserver
	recorder    *diagnostics.Recorder
	nowNS       func() uint64
	isTrusted   func() bool
	onFatalStop func(reason string)
	machine     *policy.Machine

	stateMu           sync.Mutex
	timeoutRecoveries int
	shuttingDown      bool
	fatalStopped      bool
	pairingDoneCh     chan struct{}

	// Sample correlation state used by the tap thread only in observe mode.
	observeSeq        uint64
	observeActiveID   uint64
	observeDragLogged bool
}

// NewController creates the event orchestration controller.
func NewController(cfg ControllerConfig) *Controller {
	nowFn := cfg.NowNS
	if nowFn == nil {
		nowFn = darwin.MonotonicNowNS
	}
	trustFn := cfg.IsTrusted
	if trustFn == nil {
		trustFn = func() bool { return darwin.IsProcessTrusted(false) }
	}
	desktopEnabled := cfg.DesktopEnabled && !cfg.ObserveOnly && cfg.Snapshots != nil
	machine := policy.NewMachine(policy.Config{
		DockEnabled:           cfg.DockEnabled && !cfg.ObserveOnly,
		DockInterceptMode:     cfg.DockInterceptMode,
		DesktopEnabled:        desktopEnabled,
		DesktopInterceptMode:  cfg.DesktopInterceptMode,
		DoubleClickIntervalNS: cfg.DoubleClickIntervalNS,
		Snapshots:             cfg.Snapshots,
	})
	// When the desktop entry is disabled, no Finder attribute is read, so the ordinary event cost of a Dock-only
	// configuration stays unchanged.
	inspectMode := darwin.InspectDock
	if desktopEnabled {
		inspectMode = darwin.InspectActive
	}

	c := &Controller{
		observeOnly: cfg.ObserveOnly,
		inspectMode: inspectMode,
		budgetNS:    policy.DefaultCandidateBudgetNS,
		probe:       cfg.Probe,
		executor:    cfg.Executor,
		recorder:    cfg.Recorder,
		nowNS:       nowFn,
		isTrusted:   trustFn,
		onFatalStop: cfg.OnFatalStop,
		machine:     machine,
	}
	if cfg.Executor != nil {
		c.reserver = cfg.Executor
	}
	return c
}

// InspectMode returns the scope the controller uses to resolve hits: observe mode collects the full parent chain, and
// active mode depends on the enabled entries.
func (c *Controller) InspectMode() darwin.InspectMode {
	if c.observeOnly {
		return darwin.InspectObserve
	}
	return c.inspectMode
}

// OnMouseEvent implements darwin.TapCallbacks and synchronously returns a pass, swallow or off-screen cancel decision
// on the dedicated tap thread.
func (c *Controller) OnMouseEvent(ev policy.Event) darwin.TapDisposition {
	if c.observeOnly {
		c.handleObserveEvent(ev)
		return darwin.TapPass
	}
	if ev.TimeNS == 0 {
		ev.TimeNS = c.nowNS()
	}

	var lastObs darwin.AXObservation
	identify := func(pt policy.Point, deadlineNS uint64) policy.HitResult {
		if c.probe == nil {
			return policy.HitResult{}
		}
		lastObs = c.probe.HitTest(pt, deadlineNS, c.inspectMode)
		return darwin.TargetFromObservation(lastObs)
	}
	recheck := func(needSelection bool) policy.TargetResolver {
		return func(pt policy.Point, deadlineNS uint64) policy.HitResult {
			if c.probe == nil {
				return policy.HitResult{}
			}
			lastObs = c.probe.Recheck(pt, deadlineNS, needSelection)
			return darwin.TargetFromObservation(lastObs)
		}
	}

	c.stateMu.Lock()
	if c.fatalStopped {
		c.stateMu.Unlock()
		return darwin.TapPass
	}
	wasPaired := c.machine.HasPendingPairing()
	dec := c.machine.HandleWith(ev, identify, recheck(false), recheck(true), c.reserver)
	// The pair-finish flag is sampled under the lock; waking the drain waiters must wait until the action is enqueued
	// (see below).
	pairingDone := wasPaired && !c.machine.HasPendingPairing()
	// A gesture that returns to idle has ended: release the element reference retained at identification. Like abort and
	// drain, this runs under stateMu, mutually exclusive with resolution on the tap thread.
	if c.machine.State() == policy.StateIdle {
		c.releaseEvidence()
	}
	c.stateMu.Unlock()

	if dec.Action != nil && c.executor != nil && !c.executor.Submit(*dec.Action) && !wasPaired {
		// If the action could not be enqueued when the up was intercepted because the executor had just become invalid,
		// pass the original event through.
		dec.Consume, dec.CancelUp, dec.CancelDesktopUp = false, false, false
	}

	if pairingDone {
		// Wake the shutdown goroutine waiting on drain only after Submit has enqueued the action of the paired up.
		c.stateMu.Lock()
		if c.pairingDoneCh != nil {
			close(c.pairingDoneCh)
			c.pairingDoneCh = nil
		}
		c.stateMu.Unlock()
	}

	isCandidate := wasPaired || dec.GestureID != 0 || dec.OverBudget ||
		lastObs.ProcessKind == darwin.ProcessDock || lastObs.FinderContainer != darwin.ContainerNone
	c.recordGesture(ev, dec, lastObs, isCandidate)
	return dispositionFor(dec)
}

// recordGesture records the callback duration and, under --debug, logs redacted samples: candidate or intercepted
// events, plus ordinary downs and ups.
// Drag events in the idle state are not logged one by one, to keep high-frequency movement from filling the bounded
// queue.
func (c *Controller) recordGesture(ev policy.Event, dec policy.Decision, obs darwin.AXObservation, isCandidate bool) {
	elapsedNS := elapsedSince(ev.TimeNS, c.nowNS())
	c.recorder.RecordCallbackDuration(isCandidate, elapsedNS, dec.OverBudget)
	isClick := ev.Kind == policy.EventDown || ev.Kind == policy.EventUp
	if !c.recorder.DebugEnabled() || !(isCandidate || dec.Consume || isClick) {
		return
	}
	sample := newEventSample("debug", ev, dec.GestureID, obs, elapsedNS, !dec.OverBudget)
	if !isCandidate {
		// Ordinary clicks keep only the category and duration: recording the clicked app's process ID every time would
		// amount to a click trail.
		sample.PID = 0
	}
	sample.Decision = decisionLabel(dec)
	c.recorder.EnqueueSample(sample)
}

// dispositionFor maps a state machine decision to the underlying EventTap disposition.
func dispositionFor(dec policy.Decision) darwin.TapDisposition {
	switch {
	case !dec.Consume:
		return darwin.TapPass
	case dec.CancelDesktopUp:
		return darwin.TapCancelDesktopUp
	case dec.CancelUp:
		return darwin.TapCancelUp
	default:
		return darwin.TapSwallow
	}
}

// decisionLabel maps a state machine decision to a redacted debug label.
func decisionLabel(dec policy.Decision) string {
	switch {
	case !dec.Consume:
		return "pass"
	case dec.CancelDesktopUp && dec.Action != nil:
		return "cancel_desktop_up+open_directory"
	case dec.CancelUp && dec.Action != nil:
		return "cancel_up+activate_app"
	case dec.Action != nil && dec.Action.Kind == policy.ActionOpenDirectory:
		return "consume+open_directory"
	case dec.Action != nil:
		return "consume+activate_app"
	default:
		return "consume"
	}
}

func (c *Controller) handleObserveEvent(ev policy.Event) {
	startNS := ev.TimeNS
	if startNS == 0 {
		startNS = c.nowNS()
	}

	c.stateMu.Lock()
	if c.fatalStopped {
		c.stateMu.Unlock()
		return
	}
	var gid uint64
	switch ev.Kind {
	case policy.EventDown:
		c.observeSeq++
		c.observeActiveID = c.observeSeq
		c.observeDragLogged = false
		gid = c.observeActiveID
	case policy.EventDragged:
		// Sample only the first drag event of each pressed gesture, to keep high-frequency movement from filling the AX
		// and log queues.
		if c.observeActiveID == 0 || c.observeDragLogged {
			c.stateMu.Unlock()
			return
		}
		c.observeDragLogged = true
		gid = c.observeActiveID
	case policy.EventUp:
		gid = c.observeActiveID
		c.observeActiveID = 0
		c.observeDragLogged = false
	default:
		c.stateMu.Unlock()
		return
	}
	c.stateMu.Unlock()

	var obs darwin.AXObservation
	if c.probe != nil {
		obs = c.probe.HitTest(ev.Point, startNS+c.budgetNS, darwin.InspectObserve)
	}
	elapsedNS := elapsedSince(startNS, c.nowNS())
	c.recorder.RecordCallbackDuration(true, elapsedNS, obs.BudgetExceeded)

	sample := newEventSample("observe", ev, gid, obs, elapsedNS, !obs.BudgetExceeded)
	sample.ParentChain = obs.ParentChain
	sample.Decision = "observe"
	c.recorder.EnqueueSample(sample)
}

// newEventSample builds one redacted sample: it contains only structured roles and categories, never paths, URLs or
// coordinates.
func newEventSample(mode string, ev policy.Event, gid uint64, obs darwin.AXObservation, elapsedNS uint64, inBudget bool) diagnostics.EventSample {
	return diagnostics.EventSample{
		Mode:           mode,
		GestureID:      gid,
		Stage:          ev.Kind.String(),
		ClickCount:     ev.ClickCount,
		Modifiers:      ev.Modifiers.String(),
		PID:            obs.PID,
		ProcessKind:    obs.ProcessKind.String(),
		AXRole:         obs.Role,
		AXSubrole:      obs.Subrole,
		Container:      containerLabel(obs),
		Selection:      selectionLabel(obs),
		URLKind:        obs.URLKind.String(),
		Classification: classifyObservation(obs),
		Source:         obs.HitSource.String(),
		ElapsedUS:      elapsedNS / 1_000,
		InBudget:       inBudget,
	}
}

func elapsedSince(startNS, endNS uint64) uint64 {
	if endNS < startNS {
		return 0
	}
	return endNS - startNS
}

func classifyObservation(obs darwin.AXObservation) string {
	if obs.BudgetExceeded {
		return "budget_exceeded"
	}
	switch obs.HitSource {
	case darwin.SourceChanged:
		return "target_changed"
	case darwin.SourceOffDock:
		return "off_dock"
	case darwin.SourceCovered:
		return "covered"
	}
	if obs.IsDockFinder {
		return "dock_finder"
	}
	switch obs.ProcessKind {
	case darwin.ProcessDock:
		return "dock_other"
	case darwin.ProcessFinder:
		switch obs.FinderContainer {
		case darwin.ContainerDesktopIcon:
			return "desktop_icon"
		case darwin.ContainerDesktopBackground:
			return "desktop_background"
		case darwin.ContainerFinderWindow:
			return "finder_window"
		default:
			return "finder_element"
		}
	case darwin.ProcessOther:
		return "other_app"
	default:
		if obs.AXError != 0 {
			return "ax_error"
		}
		return "unknown"
	}
}

// containerLabel returns the structural container category of a Finder hit; for other processes it returns an empty
// string.
func containerLabel(obs darwin.AXObservation) string {
	if obs.ProcessKind != darwin.ProcessFinder {
		return ""
	}
	return obs.FinderContainer.String()
}

// selectionLabel returns the selection state of the container holding the Finder hit element; for other processes it
// returns an empty string.
func selectionLabel(obs darwin.AXObservation) string {
	if obs.ProcessKind != darwin.ProcessFinder {
		return ""
	}
	switch {
	case !obs.HasSelectionInfo:
		return "unknown"
	case obs.SelectionCount == 1 && obs.IsSingleSelectedSelf:
		return "single_self"
	case obs.SelectionCount == 1:
		return "single_other"
	case obs.SelectionCount == 0:
		return "none"
	default:
		return "multi"
	}
}

// OnTapDisabled implements darwin.TapCallbacks and handles single timeout recovery and shutdown on permission
// revocation or repeated disable.
func (c *Controller) OnTapDisabled(reason darwin.TapDisableReason, tap *darwin.EventTap) {
	c.HandleTapDisabled(reason, tap)
}

// HandleTapDisabled performs pair abort, limited recovery or fault shutdown after the tap is disabled.
func (c *Controller) HandleTapDisabled(reason darwin.TapDisableReason, tap TapEnabler) {
	c.stateMu.Lock()
	if c.fatalStopped || c.shuttingDown {
		c.stateMu.Unlock()
		return
	}
	if c.abortGesture() {
		c.recorder.IncPairingInterrupted()
	}
	if c.pairingDoneCh != nil {
		close(c.pairingDoneCh)
		c.pairingDoneCh = nil
	}

	if reason == darwin.TapDisabledByUserInput {
		c.fatalStopped = true
		c.stateMu.Unlock()
		c.recorder.Error("tap", "stopped", "tap_disabled_by_user_input")
		c.notifyFatal("tap_disabled_by_user_input")
		return
	}

	// TapDisabledByTimeout: check whether the permission has been revoked, and auto-recover at most once.
	if !c.isTrusted() {
		c.fatalStopped = true
		c.stateMu.Unlock()
		c.recorder.Error("tap", "stopped", "permission_revoked")
		c.notifyFatal("permission_revoked")
		return
	}

	if c.timeoutRecoveries == 0 {
		c.timeoutRecoveries = 1
		c.stateMu.Unlock()
		if tap != nil && tap.SetEnabled(true) {
			c.recorder.IncTapTimeoutRecovery()
			c.recorder.Warn("tap", "recovered", "tap_disabled_by_timeout_once")
			return
		}
		c.stateMu.Lock()
		c.fatalStopped = true
		c.stateMu.Unlock()
		c.recorder.Error("tap", "stopped", "tap_reenable_failed")
		c.notifyFatal("tap_reenable_failed")
		return
	}

	c.fatalStopped = true
	c.stateMu.Unlock()
	c.recorder.Error("tap", "stopped", "tap_disabled_repeated_timeout")
	c.notifyFatal("tap_disabled_repeated_timeout")
}

// OnSystemReset clears candidates and stale AX identity data on wake from sleep or a display layout change.
func (c *Controller) OnSystemReset() {
	c.stateMu.Lock()
	interrupted := c.abortGesture()
	if c.pairingDoneCh != nil {
		close(c.pairingDoneCh)
		c.pairingDoneCh = nil
	}
	c.stateMu.Unlock()

	if interrupted {
		c.recorder.IncPairingInterrupted()
	}
	c.recorder.Info("system", "reset", "cleared_candidate_state")
}

// abortGesture aborts the current gesture and releases the element reference it retained; the caller must hold
// stateMu.
func (c *Controller) abortGesture() (interrupted bool) {
	interrupted = c.machine.Abort(c.reserver)
	c.releaseEvidence()
	return interrupted
}

// CloseProbe calls closeFn under stateMu to close the probe and drop the reference. The system reset callback on the
// main thread also takes stateMu,
// so it cannot interleave with the close and never touches the closed probe afterwards. It must be called after the
// tap stops.
func (c *Controller) CloseProbe(closeFn func()) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	c.probe = nil
	if closeFn != nil {
		closeFn()
	}
}

// releaseEvidence ends the current gesture's evidence: it releases the element reference the probe retained on
// successful identification; the caller must hold stateMu.
func (c *Controller) releaseEvidence() {
	if c.probe != nil {
		c.probe.EndGesture()
	}
}

// DrainForShutdown stops accepting new intercepts; if a down-consumed pair is in progress, it waits at most
// boundedWait for the paired up to finish it.
func (c *Controller) DrainForShutdown(boundedWait time.Duration) {
	c.stateMu.Lock()
	c.shuttingDown = true
	c.machine.BeginDrain(c.reserver)
	if !c.machine.HasPendingPairing() || boundedWait <= 0 {
		if c.abortGesture() {
			c.recorder.IncPairingInterrupted()
		}
		c.stateMu.Unlock()
		return
	}
	waitCh := make(chan struct{})
	c.pairingDoneCh = waitCh
	c.stateMu.Unlock()

	timer := time.NewTimer(boundedWait)
	defer timer.Stop()

	select {
	case <-waitCh:
	case <-timer.C:
		c.stateMu.Lock()
		if c.abortGesture() {
			c.recorder.IncPairingInterrupted()
		}
		c.pairingDoneCh = nil
		c.stateMu.Unlock()
	}
}

func (c *Controller) notifyFatal(reason string) {
	if c.onFatalStop != nil {
		c.onFatalStop(reason)
	}
}
