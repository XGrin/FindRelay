package relay

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/XGrin/findrelay/internal/darwin"
	"github.com/XGrin/findrelay/internal/diagnostics"
	"github.com/XGrin/findrelay/internal/policy"
)

const (
	// DefaultActionCapacity is the total capacity limit across reservations, queued actions and callbacks still in
	// flight.
	DefaultActionCapacity = 8

	// DefaultCompletionTimeout is how long a single NSWorkspace request waits for its completion callback (a timeout
	// counts as an unknown result).
	DefaultCompletionTimeout = 5 * time.Second
	// DefaultCommandCompletionTimeout is how long a custom open command waits to complete: it covers a cold start of
	// the target app and UI automation scripts.
	DefaultCommandCompletionTimeout = 15 * time.Second
)

// AppOpener asynchronously launches or activates the given app bundle and calls onComplete when done.
type AppOpener func(appPath string, onComplete func(error))

// DirectoryOpener asynchronously sends a single directory URL to the given app bundle and calls onComplete when done.
type DirectoryOpener func(dirPath, appPath string, onComplete func(error))

// TargetRevalidator revalidates whether the target app bundle is still usable.
type TargetRevalidator func(appPath string) error

// DirectoryRevalidator revalidates that the directory is still the same object the snapshot recorded.
type DirectoryRevalidator func(dirPath string, dev, ino uint64) error

// openRequest issues one system open request and calls back once on completion.
type openRequest func(onComplete func(error))

// slotState is the stage an action slot is in.
type slotState uint8

const (
	// slotReserved means the slot is reserved for a candidate gesture, not yet submitted, and can be revoked.
	slotReserved slotState = iota + 1
	// slotInFlight means the action is queued or dispatched and is waiting for its completion callback.
	slotInFlight
	// slotTimedOutInFlight means the deadline for waiting on the completion callback has passed; the slot is kept until
	// the late callback reclaims it.
	slotTimedOutInFlight
)

type openResult uint8

const (
	openSucceeded openResult = iota
	openTimedOut
	openFailed
)

// ExecutorConfig configures the asynchronous action executor.
type ExecutorConfig struct {
	TargetAppPath       string
	FinderAppPath       string
	Capacity            int
	CompletionTimeout   time.Duration
	Opener              AppOpener
	DirOpener           DirectoryOpener
	RevalidateTarget    TargetRevalidator
	RevalidateDirectory DirectoryRevalidator
	Recorder            *diagnostics.Recorder
	OnTargetInvalid     func(reason string)
}

// Executor submits confirmed open actions serially and manages the total capacity of 8, failure compensation and
// late callback reclamation.
type Executor struct {
	targetAppPath       string
	finderAppPath       string
	capacity            int
	completionTimeout   time.Duration
	opener              AppOpener
	dirOpener           DirectoryOpener
	revalidateTarget    TargetRevalidator
	revalidateDirectory DirectoryRevalidator
	recorder            *diagnostics.Recorder
	onTargetInvalid     func(reason string)

	mu       sync.Mutex
	slots    map[uint64]slotState
	disabled bool
	closed   bool

	queueCh   chan policy.Action
	doneCh    chan struct{}
	closeOnce sync.Once
}

// NewExecutor creates and starts the background serial action executor.
func NewExecutor(cfg ExecutorConfig) *Executor {
	capLimit := cfg.Capacity
	if capLimit <= 0 {
		capLimit = DefaultActionCapacity
	}
	timeout := cfg.CompletionTimeout
	if timeout <= 0 {
		timeout = DefaultCompletionTimeout
	}
	e := &Executor{
		targetAppPath:       cfg.TargetAppPath,
		finderAppPath:       cfg.FinderAppPath,
		capacity:            capLimit,
		completionTimeout:   timeout,
		opener:              cfg.Opener,
		dirOpener:           cfg.DirOpener,
		revalidateTarget:    cfg.RevalidateTarget,
		revalidateDirectory: cfg.RevalidateDirectory,
		recorder:            cfg.Recorder,
		onTargetInvalid:     cfg.OnTargetInvalid,
		slots:               make(map[uint64]slotState, capLimit),
		queueCh:             make(chan policy.Action, capLimit),
		doneCh:              make(chan struct{}),
	}
	go e.workerLoop()
	return e
}

// TryReserve reserves an action slot without blocking, before the event is consumed.
func (e *Executor) TryReserve(gestureID uint64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.closed || e.disabled {
		return false
	}
	if _, exists := e.slots[gestureID]; exists {
		return false
	}
	if len(e.slots) >= e.capacity {
		return false
	}
	e.slots[gestureID] = slotReserved
	return true
}

// ReleaseReservation releases a candidate reservation that has not been submitted yet (idempotent).
func (e *Executor) ReleaseReservation(gestureID uint64) {
	e.mu.Lock()
	if e.slots[gestureID] == slotReserved {
		delete(e.slots, gestureID)
	}
	e.mu.Unlock()
}

// InUseCount returns the total capacity in use (reserved + queued + in flight without a callback yet).
func (e *Executor) InUseCount() int {
	e.mu.Lock()
	n := len(e.slots)
	e.mu.Unlock()
	return n
}

// Disabled reports whether further redirects have been turned off because the target app failed revalidation.
func (e *Executor) Disabled() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.disabled
}

// Submit pushes the reserved gesture's action onto the serial execution queue without blocking; each GestureID is
// submitted at most once.
func (e *Executor) Submit(action policy.Action) bool {
	if !supportedAction(action) {
		return false
	}

	e.mu.Lock()
	if e.closed || e.disabled {
		if e.slots[action.GestureID] == slotReserved {
			delete(e.slots, action.GestureID)
		}
		e.mu.Unlock()
		return false
	}
	if e.slots[action.GestureID] != slotReserved {
		e.mu.Unlock()
		return false
	}
	// The invariant len(e.slots) <= e.capacity == cap(e.queueCh) guarantees that enqueueing under the lock here never
	// blocks,
	// and fully eliminates the race between unlocking and Close() closing the channel.
	e.slots[action.GestureID] = slotInFlight
	e.recorder.IncActionSubmitted()
	e.queueCh <- action
	e.mu.Unlock()
	return true
}

func supportedAction(action policy.Action) bool {
	switch action.Kind {
	case policy.ActionActivateApp:
		return true
	case policy.ActionOpenDirectory:
		return action.Path != ""
	default:
		return false
	}
}

// Close stops accepting new requests and waits for the queued actions to finish.
func (e *Executor) Close() {
	e.closeOnce.Do(func() {
		e.mu.Lock()
		e.closed = true
		for id, st := range e.slots {
			if st == slotReserved {
				delete(e.slots, id)
			}
		}
		close(e.queueCh)
		e.mu.Unlock()
		<-e.doneCh
	})
}

func (e *Executor) workerLoop() {
	defer close(e.doneCh)
	for action := range e.queueCh {
		e.executeOne(action)
	}
}

func (e *Executor) executeOne(action policy.Action) {
	switch action.Kind {
	case policy.ActionActivateApp:
		e.executeActivateApp(action.GestureID)
	case policy.ActionOpenDirectory:
		e.executeOpenDirectory(action)
	}
}

// executeActivateApp revalidates the target app and then launches or activates it; on an explicit failure it issues
// one launch or activation compensation to the native Finder.
func (e *Executor) executeActivateApp(gid uint64) {
	// Revalidate that the target app is still valid before submitting to the system.
	if e.revalidateTarget != nil && e.revalidateTarget(e.targetAppPath) != nil {
		e.markTargetInvalid(ReasonTargetInvalidated)
		e.recorder.Error("dock", "target_invalid", "target_app_validation_failed")
		e.compensate(gid, "dock", "finder_activated_once", e.finderAppLaunch())
		return
	}

	if e.opener == nil {
		e.freeSlot(gid)
		return
	}

	switch result, err := e.awaitOpen(gid, "dock", func(cb func(error)) { e.opener(e.targetAppPath, cb) }); result {
	case openSucceeded:
		e.recorder.IncActionSucceeded()
		e.recorder.Debug("dock", "activated", "target_app_opened")
		e.freeSlot(gid)
	case openFailed:
		e.handleOpenFailure("dock", err)
		e.compensate(gid, "dock", "finder_activated_once", e.finderAppLaunch())
	}
}

// executeOpenDirectory revalidates the directory identity and the target app, then sends the single directory URL to
// the target app.
func (e *Executor) executeOpenDirectory(action policy.Action) {
	gid := action.GestureID

	if e.dirOpener == nil || e.revalidateDirectory == nil {
		e.freeSlot(gid)
		return
	}

	// Revalidate the directory identity before submitting to the system: if it was moved, deleted or replaced by a new
	// directory at the same path, drop the stale request;
	// Finder never received the double-click in that case, so there is no native open to compensate.
	if err := e.revalidateDirectory(action.Path, action.Dev, action.Ino); err != nil {
		if errors.Is(err, darwin.ErrDirectoryIdentityChanged) {
			e.recorder.Warn("desktop", "target_invalid", "directory_identity_changed")
		} else {
			e.recorder.Warn("desktop", "target_invalid", "directory_revalidation_failed")
		}
		e.freeSlot(gid)
		return
	}

	if e.revalidateTarget != nil && e.revalidateTarget(e.targetAppPath) != nil {
		e.markTargetInvalid(ReasonTargetInvalidated)
		e.recorder.Error("desktop", "target_invalid", "target_app_validation_failed")
		e.compensateDirectory(gid, action)
		return
	}

	switch result, err := e.awaitOpen(gid, "desktop", func(cb func(error)) { e.dirOpener(action.Path, e.targetAppPath, cb) }); result {
	case openSucceeded:
		e.recorder.IncActionSucceeded()
		e.recorder.Debug("desktop", "opened", "directory_sent_to_target_app")
		e.freeSlot(gid)
	case openFailed:
		e.handleOpenFailure("desktop", err)
		e.compensateDirectory(gid, action)
	}
}

// compensateDirectory sends the directory to the native Finder through the same interface when the system failed
// explicitly and the directory still revalidates; it runs only once.
func (e *Executor) compensateDirectory(gid uint64, action policy.Action) {
	launch := e.finderDirectoryLaunch(action.Path)
	if launch == nil {
		e.freeSlot(gid)
		return
	}
	if err := e.revalidateDirectory(action.Path, action.Dev, action.Ino); err != nil {
		e.recorder.Warn("desktop", "target_invalid", "compensation_skipped_directory_invalid")
		e.freeSlot(gid)
		return
	}
	e.compensate(gid, "desktop", "finder_opened_directory_once", launch)
}

// finderAppLaunch returns a request that launches or activates the native Finder; it returns nil when unavailable.
func (e *Executor) finderAppLaunch() openRequest {
	if e.opener == nil || e.finderAppPath == "" {
		return nil
	}
	return func(cb func(error)) { e.opener(e.finderAppPath, cb) }
}

// finderDirectoryLaunch returns a request that sends the directory to the native Finder; it returns nil when
// unavailable.
func (e *Executor) finderDirectoryLaunch(dirPath string) openRequest {
	if e.dirOpener == nil || e.finderAppPath == "" {
		return nil
	}
	return func(cb func(error)) { e.dirOpener(dirPath, e.finderAppPath, cb) }
}

// awaitOpen marks the request as dispatched and waits at most completionTimeout;
// a timeout counts as an unknown result, and the slot is kept until the late callback reclaims it. On an
// explicit failure it also returns the failure reason for classification.
func (e *Executor) awaitOpen(gid uint64, feature string, launch openRequest) (openResult, error) {
	outcome, timedOut := e.dispatchAndAwait(gid, launch)
	switch {
	case timedOut:
		e.recorder.IncActionTimedOut()
		e.recorder.Warn(feature, "unknown_outcome", "open_completion_timeout")
		return openTimedOut, nil
	case outcome != nil:
		return openFailed, outcome
	default:
		return openSucceeded, nil
	}
}

// openFailureDetail classifies an open failure into a redacted reason code; the error text may contain paths and is
// not logged.
func openFailureDetail(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "open_command_timeout"
	case errors.Is(err, darwin.ErrOpenCommandFailed):
		return "open_command_failed"
	default:
		return "workspace_open_error"
	}
}

// handleOpenFailure records the category of the open failure and revalidates whether the target app has been
// uninstalled or damaged.
func (e *Executor) handleOpenFailure(feature string, err error) {
	e.recorder.Error(feature, "open_failed", openFailureDetail(err))
	if e.revalidateTarget != nil && e.revalidateTarget(e.targetAppPath) != nil {
		e.markTargetInvalid(ReasonTargetInvalidated)
	}
}

// compensate issues one compensation to the native Finder and reclaims the slot; a compensation timeout also counts
// as an unknown result.
// A nil launch means there is no compensation channel available, and the slot is reclaimed directly.
func (e *Executor) compensate(gid uint64, feature, successDetail string, launch openRequest) {
	if launch == nil {
		e.freeSlot(gid)
		return
	}
	e.recorder.IncActionCompensated()
	switch result, _ := e.awaitOpen(gid, feature, launch); result {
	case openSucceeded:
		e.recorder.Warn(feature, "compensated", successDetail)
		e.freeSlot(gid)
	case openFailed:
		e.recorder.Error(feature, "compensation_failed", "finder_open_error")
		e.freeSlot(gid)
	}
}

// dispatchAndAwait calls launch and waits at most completionTimeout.
// On timeout it marks gid as slotTimedOutInFlight, and the late callback is responsible for the single release of
// capacity.
func (e *Executor) dispatchAndAwait(gid uint64, launch openRequest) (err error, timedOut bool) {
	resCh := make(chan error, 1)
	var once sync.Once

	launch(func(cbErr error) {
		once.Do(func() {
			e.mu.Lock()
			st, exists := e.slots[gid]
			if !exists {
				e.mu.Unlock()
				return
			}
			if st == slotTimedOutInFlight {
				delete(e.slots, gid)
				e.mu.Unlock()
				return
			}
			resCh <- cbErr
			e.mu.Unlock()
		})
	})

	timer := time.NewTimer(e.completionTimeout)
	defer timer.Stop()

	select {
	case cbErr := <-resCh:
		return cbErr, false
	case <-timer.C:
		e.mu.Lock()
		select {
		case cbErr := <-resCh:
			e.mu.Unlock()
			return cbErr, false
		default:
			if _, exists := e.slots[gid]; exists {
				e.slots[gid] = slotTimedOutInFlight
			}
			e.mu.Unlock()
			return nil, true
		}
	}
}

func (e *Executor) freeSlot(gid uint64) {
	e.mu.Lock()
	delete(e.slots, gid)
	e.mu.Unlock()
}

func (e *Executor) markTargetInvalid(reason string) {
	e.mu.Lock()
	already := e.disabled
	e.disabled = true
	e.mu.Unlock()

	if !already && e.onTargetInvalid != nil {
		e.onTargetInvalid(reason)
	}
}
