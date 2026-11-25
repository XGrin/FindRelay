package policy

import (
	"fmt"
	"strings"
)

// EventKind is the category of a mouse gesture event.
type EventKind uint8

const (
	EventDown EventKind = iota + 1
	EventUp
	EventDragged
)

func (k EventKind) String() string {
	switch k {
	case EventDown:
		return "down"
	case EventUp:
		return "up"
	case EventDragged:
		return "dragged"
	default:
		return "unknown"
	}
}

// Point is a global screen coordinate (used only for geometric tolerance comparisons inside the in-memory state
// machine, and never written to logs).
type Point struct {
	X float64
	Y float64
}

// Modifiers is the modifier-key mask carried by a mouse event.
type Modifiers uint8

const (
	ModShift Modifiers = 1 << iota
	ModControl
	ModOption
	ModCommand
)

// HasAny reports whether any bypass modifier key is present.
func (m Modifiers) HasAny() bool {
	const mask = ModShift | ModControl | ModOption | ModCommand
	return m&mask != 0
}

func (m Modifiers) String() string {
	if !m.HasAny() {
		return "none"
	}
	var parts []string
	if m&ModCommand != 0 {
		parts = append(parts, "cmd")
	}
	if m&ModOption != 0 {
		parts = append(parts, "opt")
	}
	if m&ModControl != 0 {
		parts = append(parts, "ctrl")
	}
	if m&ModShift != 0 {
		parts = append(parts, "shift")
	}
	return strings.Join(parts, "+")
}

// Event is the normalized input event passed to the pure Go state machine.
type Event struct {
	Kind       EventKind
	Point      Point
	ClickCount int
	Modifiers  Modifiers
	TimeNS     uint64 // monotonic clock nanoseconds
}

// TargetKind is the target category identified by AX hit-testing.
type TargetKind uint8

const (
	TargetUnknown TargetKind = iota
	TargetDockFinder
	TargetDesktopDirectory
)

func (k TargetKind) String() string {
	switch k {
	case TargetDockFinder:
		return "dock_finder"
	case TargetDesktopDirectory:
		return "desktop_directory"
	default:
		return "unknown"
	}
}

// Target is the target identity within the current gesture (not persisted).
type Target struct {
	Kind     TargetKind
	Path     string // used only by DesktopDirectory
	Identity string // stable identifier within the current gesture
	// SingleSelected is used only for DesktopDirectory: the hit element is the only selected item in its container
	// (multi-selection is excluded).
	SingleSelected bool
}

// ActionKind is the type of application-level action awaiting asynchronous submission.
type ActionKind uint8

const (
	ActionNone ActionKind = iota
	ActionActivateApp
	ActionOpenDirectory
)

func (k ActionKind) String() string {
	switch k {
	case ActionActivateApp:
		return "activate_app"
	case ActionOpenDirectory:
		return "open_directory"
	default:
		return "none"
	}
}

// Action is a single action submitted to the asynchronous executor after interception is confirmed.
type Action struct {
	GestureID uint64
	Kind      ActionKind
	Path      string // used only by OpenDirectory
	Dev       uint64 // used only by OpenDirectory: device number for re-verification before submission
	Ino       uint64 // used only by OpenDirectory: inode for re-verification before submission
}

// InterceptMode controls when a left-click gesture is intercepted; the Dock and desktop entries share the same set
// of values.
type InterceptMode uint8

const (
	// InterceptOnUp rewrites the release into an off-screen release and submits the action after the release recheck
	// passes (default; keeps the native Dock dimming press animation, long-press menu, and dragging).
	InterceptOnUp InterceptMode = iota
	// InterceptOnDown consumes the press and reserves capacity, pairs the drag and release, and submits the action on a
	// qualifying release (comparison strategy).
	InterceptOnDown
)

func (m InterceptMode) String() string {
	switch m {
	case InterceptOnUp:
		return "up"
	case InterceptOnDown:
		return "down"
	default:
		return fmt.Sprintf("unknown(%d)", m)
	}
}

// ParseInterceptMode parses the intercept-timing name passed on the CLI; entry is used only in error messages to
// indicate which entry is meant.
func ParseInterceptMode(s, entry string) (InterceptMode, error) {
	switch s {
	case "", "up":
		return InterceptOnUp, nil
	case "down":
		return InterceptOnDown, nil
	default:
		return InterceptOnUp, fmt.Errorf("invalid %s intercept mode %q (want \"up\" or \"down\")", entry, s)
	}
}

// HitResult wraps the result of one bounded AX hit query.
type HitResult struct {
	Target         Target
	BudgetExceeded bool
	Err            error
}

// TargetResolver resolves the hit target at the given coordinate before a monotonic-clock deadline.
type TargetResolver func(pt Point, deadlineNS uint64) HitResult

// CapacityReserver provides non-blocking reservation and release of action capacity.
type CapacityReserver interface {
	TryReserve(gestureID uint64) bool
	ReleaseReservation(gestureID uint64)
}

// DirectorySnapshot is the file identity, available for re-verification before submission, produced after the
// first-click directory classification passes.
type DirectorySnapshot struct {
	Path string
	Dev  uint64
	Ino  uint64
}

// SnapshotSource provides background classification and short-lived snapshot lookup for desktop first-click
// directories.
// Each method is called synchronously on the tap thread, so implementations must be non-blocking.
type SnapshotSource interface {
	// StartClassification starts directory classification for the first click; expiresNS is the snapshot's
	// monotonic-clock expiry.
	StartClassification(gestureID uint64, path, identity string, expiresNS uint64)
	// LookupSnapshot returns a qualified directory snapshot that matches the target identity and is still valid;
	// it returns false when the snapshot is not ready, canceled, expired, or unqualified.
	LookupSnapshot(gestureID uint64, identity, path string, nowNS uint64) (DirectorySnapshot, bool)
	// CancelClassification invalidates the snapshot; classification results that arrive afterwards are discarded.
	CancelClassification(gestureID uint64)
}

// Decision is the decision output for a single event after it passes through the policy state machine.
type Decision struct {
	GestureID       uint64
	Consume         bool
	CancelUp        bool // true means the press was passed through to Dock, and this release must be rewritten into an off-screen release to clear the press highlight
	CancelDesktopUp bool // true means the desktop second-click press was passed through, and this release must be rewritten into an off-screen release with click-state reset to zero
	Action          *Action
	OverBudget      bool
}
