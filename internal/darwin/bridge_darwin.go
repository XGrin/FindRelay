package darwin

/*
#cgo CFLAGS: -Wall -Wextra -Wno-unused-parameter -Werror
#cgo LDFLAGS: -framework ApplicationServices -framework CoreFoundation -framework CoreGraphics -framework AppKit -framework Foundation -framework Security
#include <stdlib.h>
#include "bridge.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"io"
	"runtime"
	"runtime/cgo"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/XGrin/findrelay/internal/appbundle"
	"github.com/XGrin/findrelay/internal/diagnostics"
	"github.com/XGrin/findrelay/internal/policy"
	"github.com/XGrin/findrelay/internal/service"
)

const (
	cEventLeftMouseDown        = uint32(C.kCGEventLeftMouseDown)
	cEventLeftMouseUp          = uint32(C.kCGEventLeftMouseUp)
	cEventLeftMouseDragged     = uint32(C.kCGEventLeftMouseDragged)
	cEventTapDisabledTimeout   = uint32(C.kCGEventTapDisabledByTimeout)
	cEventTapDisabledUserInput = uint32(C.kCGEventTapDisabledByUserInput)

	cFlagShift   = uint64(C.kCGEventFlagMaskShift)
	cFlagControl = uint64(C.kCGEventFlagMaskControl)
	cFlagOption  = uint64(C.kCGEventFlagMaskAlternate)
	cFlagCommand = uint64(C.kCGEventFlagMaskCommand)
)

var (
	// ErrEventTapCreate means the system-wide CGEventTap could not be created (usually because Accessibility or Input
	// Monitoring permission is missing).
	ErrEventTapCreate = errors.New("cannot create CGEventTap; check Accessibility and Input Monitoring permissions")

	errAXLookupFailed = errors.New("ax lookup failed")
)

// MonotonicNowNS returns the current value of the macOS monotonic clock in nanoseconds (CLOCK_UPTIME_RAW).
func MonotonicNowNS() uint64 {
	return uint64(C.fr_monotonic_now_ns())
}

// DoubleClickIntervalNS returns the system's maximum double-click interval (NSEvent.doubleClickInterval) in
// nanoseconds.
func DoubleClickIntervalNS() uint64 {
	return uint64(C.fr_double_click_interval_ns())
}

// CurrentGUISession queries the graphical session identity of the current process through the public Security.framework
// SessionGetInfo.
func CurrentGUISession() (service.SessionInfo, error) {
	var sid C.uint32_t
	var hasGraphic C.bool
	status := C.fr_current_session_info(&sid, &hasGraphic)
	if status != 0 {
		return service.SessionInfo{}, fmt.Errorf("SessionGetInfo failed with OSStatus %d", int(status))
	}
	return service.SessionInfo{
		ID:               uint32(sid),
		HasGraphicAccess: bool(hasGraphic),
	}, nil
}

// IsProcessTrusted preflights the current process's Accessibility authorization; prompt controls whether the system
// authorization prompt is shown.
func IsProcessTrusted(prompt bool) bool {
	return bool(C.fr_ax_is_trusted(C.bool(prompt)))
}

// NewServiceLogWriter returns a writer that writes whole lines of text to the system unified log, so background
// instances started by a LaunchAgent can emit redacted diagnostics.
// Background instances have no terminal, and launchd discards their standard output and standard error.
func NewServiceLogWriter() io.WriteCloser {
	return diagnostics.NewLineWriter(func(line string) {
		cLine := C.CString(line)
		defer C.free(unsafe.Pointer(cLine))
		C.fr_os_log_line(cLine)
	})
}

// InspectAppBundle uses CoreFoundation CFBundle to validate a .app bundle and read its information (both binary and XML
// Info.plist are supported).
func InspectAppBundle(appPath string) (appbundle.BundleInfo, error) {
	cPath := C.CString(appPath)
	defer C.free(unsafe.Pointer(cPath))

	var bidBuf [256]C.char
	var execBuf [4096]C.char
	ok := C.fr_inspect_bundle(cPath, &bidBuf[0], C.size_t(len(bidBuf)), &execBuf[0], C.size_t(len(execBuf)))
	if !ok {
		return appbundle.BundleInfo{}, errors.New("not a runnable macOS application bundle")
	}
	return appbundle.BundleInfo{
		BundleID:       C.GoString(&bidBuf[0]),
		ExecutablePath: C.GoString(&execBuf[0]),
	}, nil
}

// FinderApplicationPath returns the system Finder.app path queried through NSWorkspace; if the query fails, it returns
// the system default path.
func FinderApplicationPath() string {
	var buf [4096]C.char
	C.fr_finder_app_path(&buf[0], C.size_t(len(buf)))
	return C.GoString(&buf[0])
}

// DisplayRect describes the logical bounds of a screen, used for tests and edge projection.
type DisplayRect struct {
	X      float64
	Y      float64
	Width  float64
	Height float64
}

// ProjectDockEdgePoint projects a point within 1pt of a display's left, right or bottom outer edge 8pt inward, crossing
// the Dock AX edge gap.
func ProjectDockEdgePoint(pt policy.Point, displays []DisplayRect) policy.Point {
	if len(displays) == 0 {
		return pt
	}
	cRects := make([]C.fr_rect_t, len(displays))
	for i, d := range displays {
		cRects[i] = C.fr_rect_t{
			x:      C.double(d.X),
			y:      C.double(d.Y),
			width:  C.double(d.Width),
			height: C.double(d.Height),
		}
	}
	var ox, oy C.double
	C.fr_project_dock_edge_point(
		C.double(pt.X),
		C.double(pt.Y),
		&cRects[0],
		C.uint32_t(len(cRects)),
		&ox,
		&oy,
	)
	return policy.Point{X: float64(ox), Y: float64(oy)}
}

// ProcessKind is the redacted category of the hit process.
type ProcessKind uint8

const (
	ProcessNone ProcessKind = iota
	ProcessDock
	ProcessFinder
	ProcessOther
)

func (k ProcessKind) String() string {
	switch k {
	case ProcessDock:
		return "dock"
	case ProcessFinder:
		return "finder"
	case ProcessOther:
		return "other"
	default:
		return "none"
	}
}

// URLKind is the redacted availability and type of AXURL.
type URLKind uint8

const (
	URLNone URLKind = iota
	URLFinderApp
	URLOtherApp
	URLFile
	URLNonFile
)

func (k URLKind) String() string {
	switch k {
	case URLFinderApp:
		return "finder_app"
	case URLOtherApp:
		return "other_app"
	case URLFile:
		return "file_url"
	case URLNonFile:
		return "non_file_url"
	default:
		return "none"
	}
}

// FinderContainer is the structural container that a hit element in the Finder process belongs to, determined only from
// the AX role chain.
type FinderContainer uint8

const (
	ContainerNone FinderContainer = iota
	ContainerDesktopIcon
	ContainerDesktopBackground
	ContainerFinderWindow
	ContainerFinderOther
)

func (c FinderContainer) String() string {
	switch c {
	case ContainerDesktopIcon:
		return "desktop_icon"
	case ContainerDesktopBackground:
		return "desktop_background"
	case ContainerFinderWindow:
		return "finder_window"
	case ContainerFinderOther:
		return "finder_other"
	default:
		return "none"
	}
}

// InspectMode controls which entries a bounded hit query resolves and how much information it reads.
type InspectMode uint8

const (
	// InspectDock resolves only the Dock process; no attributes are read for Finder hits.
	InspectDock InspectMode = iota
	// InspectActive resolves the Dock and Finder native desktop entries without collecting the parent chain used
	// for observation.
	InspectActive
	// InspectObserve resolves all entries and collects the redacted parent chain; it is used only by observe mode.
	InspectObserve
)

func (m InspectMode) String() string {
	switch m {
	case InspectActive:
		return "active"
	case InspectObserve:
		return "observe"
	default:
		return "dock"
	}
}

// DirStatus is the metadata classification result for a candidate directory.
type DirStatus uint8

const (
	DirEligible DirStatus = iota
	DirErrPermission
	DirErrIO
	DirRejectNotFound
	DirRejectSymlink
	DirRejectNotDirectory
	DirRejectPackage
	DirRejectAlias
	DirRejectVolume
	DirRejectNonLocalVolume
	DirRejectExternalVolume
	DirRejectUbiquitous
	DirRejectAmbiguousMetadata
)

func (s DirStatus) String() string {
	switch s {
	case DirEligible:
		return "eligible"
	case DirErrPermission:
		return "error_permission"
	case DirErrIO:
		return "error_io"
	case DirRejectNotFound:
		return "reject_not_found"
	case DirRejectSymlink:
		return "reject_symlink"
	case DirRejectNotDirectory:
		return "reject_not_directory"
	case DirRejectPackage:
		return "reject_package"
	case DirRejectAlias:
		return "reject_alias"
	case DirRejectVolume:
		return "reject_volume"
	case DirRejectNonLocalVolume:
		return "reject_non_local_volume"
	case DirRejectExternalVolume:
		return "reject_external_volume"
	case DirRejectUbiquitous:
		return "reject_ubiquitous"
	case DirRejectAmbiguousMetadata:
		return "reject_ambiguous_metadata"
	default:
		return "unknown"
	}
}

// HitSource describes how an observation was obtained.
type HitSource uint8

const (
	// SourceSystem: attributes were resolved after a system-wide hit; used for observe mode, the desktop entry
	// and the first recognition in a gesture.
	SourceSystem HitSource = iota
	// SourceOffDock: with only Dock enabled, Dock's own hit test found no element at this point, so no system-wide hit
	// was done.
	SourceOffDock
	// SourceSame: on recheck the hit is still the element held at recognition, so the attributes are carried over from
	// the recognition observation.
	SourceSame
	// SourceChanged: on recheck the hit is no longer the held element.
	SourceChanged
	// SourceCovered: with the desktop entry enabled, there is no element on Dock and the window server reports
	// that a mouse-down would hit a window of an app other than Finder, so no system-wide hit was done.
	SourceCovered
)

func (s HitSource) String() string {
	switch s {
	case SourceOffDock:
		return "off_dock"
	case SourceSame:
		return "same"
	case SourceChanged:
		return "changed"
	case SourceCovered:
		return "covered"
	default:
		return "system"
	}
}

// kAXErrorNoValue: there is no element at the hit position.
const axErrNoValue = -25212

// gateResult is the answer of Dock's in-app hit test for a position.
type gateResult uint8

const (
	// gateOnDock: some Dock element covers this position; a system-wide hit still confirms whether it is topmost.
	gateOnDock gateResult = iota
	// gateMiss: no Dock element covers this position.
	gateMiss
	// gateUnavailable: Dock's application element is unavailable or the query failed; fall back to a system-wide hit.
	gateUnavailable
)

// windowOwner is the window server's answer for a position: the owner of the window that a mouse-down there would hit
// (Dock and WindowServer windows do not count).
type windowOwner uint8

const (
	// windowUnknown: no window was hit, or the owner could not be identified; do a system-wide hit as usual.
	windowUnknown = windowOwner(C.FR_WIN_UNKNOWN)
	windowFinder  = windowOwner(C.FR_WIN_FINDER)
	windowOther   = windowOwner(C.FR_WIN_OTHER)
)

// AXObservation holds the redacted structural information from one bounded AX hit.
// FilePath is the real path resolved from AXURL. It is used only by in-memory classification and the open flow, and is
// never written to logs.
type AXObservation struct {
	AXError              int
	BudgetExceeded       bool
	PID                  int32
	ProcGen              uint64
	ProcessKind          ProcessKind
	FinderContainer      FinderContainer
	IsDockFinder         bool
	ElementHash          uint64
	HasSelectionInfo     bool
	SelectionCount       int32
	IsSingleSelectedSelf bool
	Role                 string
	Subrole              string
	ParentChain          string
	// Roles is the role sequence from the hit element up to the application (index 0 is the hit element); only Dock and
	// Finder hits fill it.
	Roles    []string
	URLKind  URLKind
	FilePath string
	// HitSource describes how this observation was obtained; see the HitSource values.
	HitSource HitSource
}

// axElementRef is the Go-side name of the native AXUIElementRef, so test doubles can write the hit function's type
// without depending on cgo.
type axElementRef = C.AXUIElementRef

// scopedHit performs one bounded hit test on scope: a system-wide hit when scope is the Inspector's ref, otherwise a
// hit on an application element.
// The returned element is a non-nil new reference; when there is no element it returns nil and reports the AX error
// code.
type scopedHit func(scope axElementRef, pt policy.Point, deadlineNS uint64) (el axElementRef, axErr int, overBudget bool)

// AXInspector manages the system-wide AXUIElement and runs bounded queries on the tap thread.
type AXInspector struct {
	ref         C.AXUIElementRef
	cFinderPath *C.char
	// dockApp is Dock's application element; it is 0 while Dock is not running and is rebuilt from the process ID the
	// next time it is needed.
	dockApp C.AXUIElementRef
	// dockPID looks up Dock's process ID by executable path and returns 0 if it is not found; tests can replace it.
	dockPID func() int32
	// dockLookupAfterNS is the monotonic clock time after which another lookup is allowed once the Dock process was not
	// found.
	dockLookupAfterNS uint64
	// hit is the bounded hit implementation; production code binds fr_ax_hit, and tests replace it to assert the call
	// sequence at the native boundary.
	hit scopedHit
	// topWindow queries the owner of the window that a mouse-down at a position would hit; production code binds
	// fr_top_window_owner, and tests can replace it as well.
	topWindow func(pt policy.Point) windowOwner

	// Evidence held when recognition succeeds (the hit becomes a candidate target): the hit element, its parent group
	// (desktop icons only) and the recognition observation.
	// Rechecks within the gesture do only one hit and compare it with held; EndGesture releases them.
	held      C.AXUIElementRef
	heldGroup C.AXUIElementRef
	heldObs   AXObservation
	hasHeld   bool
}

// NewAXInspector creates an AX hit tester bound to the given Finder application path.
func NewAXInspector(finderAppPath string) (*AXInspector, error) {
	ref := C.fr_ax_create_system_wide()
	if ref == 0 {
		return nil, errors.New("AXUIElementCreateSystemWide returned nil")
	}
	a := &AXInspector{
		ref:         ref,
		cFinderPath: C.CString(finderAppPath),
	}
	a.hit = func(scope axElementRef, pt policy.Point, deadlineNS uint64) (axElementRef, int, bool) {
		var axErr C.int
		var over C.bool
		el := C.fr_ax_hit(a.ref, scope, C.double(pt.X), C.double(pt.Y), C.uint64_t(deadlineNS), &axErr, &over)
		return el, int(axErr), bool(over)
	}
	a.dockPID = func() int32 { return int32(C.fr_dock_pid()) }
	a.topWindow = func(pt policy.Point) windowOwner {
		return windowOwner(C.fr_top_window_owner(C.double(pt.X), C.double(pt.Y)))
	}
	return a, nil
}

// Close releases the underlying native AX references (it must be called after the tap has stopped and no callback is
// running).
func (a *AXInspector) Close() {
	if a == nil {
		return
	}
	if a.ref != 0 {
		C.fr_ax_release(a.ref)
		a.ref = 0
	}
	if a.cFinderPath != nil {
		C.free(unsafe.Pointer(a.cFinderPath))
		a.cFinderPath = nil
	}
	a.dropDockApp()
	a.releaseHeld()
}

// hitAt performs one bounded hit test on scope and returns the result as a native reference; el is 0 when nothing was
// hit.
func (a *AXInspector) hitAt(scope C.AXUIElementRef, pt policy.Point, deadlineNS uint64) (el C.AXUIElementRef, axErr int, overBudget bool) {
	return a.hit(scope, pt, deadlineNS)
}

// releaseHeld releases the element references held when recognition succeeded.
func (a *AXInspector) releaseHeld() {
	C.fr_ax_release(a.held)
	C.fr_ax_release(a.heldGroup)
	a.held, a.heldGroup, a.hasHeld = 0, 0, false
}

// retain holds the successfully recognized hit element, its parent group and the recognition observation for rechecks
// within this gesture.
func (a *AXInspector) retain(el, group C.AXUIElementRef, obs AXObservation) {
	a.releaseHeld()
	a.held, a.heldGroup, a.heldObs, a.hasHeld = el, group, obs, true
}

// EndGesture ends the current gesture and releases the element references held when recognition succeeded; it is called
// on gesture end, interruption, drain and close.
// With no held references it makes no native call, so calling it on every event while idle costs nothing.
func (a *AXInspector) EndGesture() {
	if a == nil || !a.hasHeld {
		return
	}
	a.releaseHeld()
}

// Result codes of the startup warm-up steps; negative values are AX error codes.
const (
	warmOK           = int(C.FR_WARM_OK)
	warmNoProcess    = int(C.FR_WARM_NO_PROCESS)
	warmIconNotFound = int(C.FR_WARM_ICON_NOT_FOUND)
	warmBudget       = int(C.FR_WARM_BUDGET)
	warmUnexpected   = int(C.FR_WARM_UNEXPECTED)
	warmNotVisible   = int(C.FR_WARM_NOT_VISIBLE)
)

// WarmupStep is the redacted result of one startup warm-up step: step name, result code and elapsed time.
type WarmupStep struct {
	Name      string
	Status    int
	ElapsedNS uint64
}

// AXWarmupReport is the redacted startup warm-up result. It contains only steps, result codes and elapsed times, with
// no paths, URLs, names or coordinates.
type AXWarmupReport struct {
	Steps []WarmupStep
}

// Complete reports whether every step finished: it either succeeded, or the Dock icon was not visible at the time but
// the query ran to completion.
func (r AXWarmupReport) Complete() bool {
	for _, step := range r.Steps {
		if step.Status != warmOK && step.Status != warmNotVisible {
			return false
		}
	}
	return true
}

// Describe writes the report as one line of text, with each step written as "name=result/elapsed".
func (r AXWarmupReport) Describe() string {
	parts := make([]string, 0, len(r.Steps))
	for _, step := range r.Steps {
		parts = append(parts, fmt.Sprintf("%s=%s/%.1fms", step.Name, warmStatusName(step.Status), float64(step.ElapsedNS)/1e6))
	}
	return strings.Join(parts, " ")
}

func warmStatusName(status int) string {
	switch status {
	case warmOK:
		return "ok"
	case warmNoProcess:
		return "no_process"
	case warmIconNotFound:
		return "icon_not_found"
	case warmBudget:
		return "budget"
	case warmUnexpected:
		return "unexpected"
	case warmNotVisible:
		return "not_visible"
	default:
		return fmt.Sprintf("ax:%d", status)
	}
}

// WarmUp pays the AX cold-start cost before the event tap is created: it reads Dock's application element and walks one
// Dock icon through the hit path for mode; with the desktop entry enabled it does one window hit query (the first
// one in a process must first set up NSApplication, about 27 to 44 ms in total); in observe mode or with the desktop
// entry enabled (mode is not InspectDock) it also reads Finder's application element.
// All steps share one overall deadline; once the total budget is used up, the remaining steps are recorded as timed
// out. Any failure shows up only in the returned report.
func (a *AXInspector) WarmUp(mode InspectMode, budgetNS uint64) AXWarmupReport {
	deadlineNS := MonotonicNowNS() + budgetNS
	var report AXWarmupReport
	run := func(name string, step func() int) {
		startNS := MonotonicNowNS()
		status := warmBudget
		if startNS < deadlineNS {
			status = step()
		}
		report.Steps = append(report.Steps, WarmupStep{Name: name, Status: status, ElapsedNS: MonotonicNowNS() - startNS})
	}

	run("dock_read", func() int {
		if !a.ensureDockApp() {
			return warmNoProcess
		}
		return a.readApplicationRole(a.dockApp, deadlineNS)
	})
	run("dock_hit", func() int {
		center, status := a.dockFinderCenter(deadlineNS)
		if status != warmOK {
			return status
		}
		obs := a.HitTest(center, deadlineNS, mode)
		a.EndGesture()
		switch {
		case obs.BudgetExceeded:
			return warmBudget
		case obs.AXError != 0:
			return obs.AXError
		case obs.IsDockFinder:
			return warmOK
		case obs.HitSource == SourceSystem && obs.PID > 0:
			// The system-wide hit landed on another element: Dock is auto-hidden or in a full-screen Space. The
			// cold-start cost has already been paid.
			return warmNotVisible
		}
		return warmUnexpected
	})
	if mode == InspectActive {
		run("window_hit", func() int {
			a.topWindow(policy.Point{})
			return warmOK
		})
	}
	if mode != InspectDock {
		run("finder_read", func() int {
			finderApp := C.fr_ax_create_application(C.fr_finder_pid())
			if finderApp == 0 {
				return warmNoProcess
			}
			defer C.fr_ax_release(finderApp)
			return a.readApplicationRole(finderApp, deadlineNS)
		})
	}
	return report
}

// dockFinderCenter finds the Finder icon in Dock's AX tree and returns its center; if it is not found, it returns a
// result code (a warm-up status code or an AX error code).
func (a *AXInspector) dockFinderCenter(deadlineNS uint64) (policy.Point, int) {
	if !a.ensureDockApp() {
		return policy.Point{}, warmNoProcess
	}
	var x, y C.double
	var axErr C.int
	var over C.bool
	found := C.fr_ax_dock_item_center(a.ref, a.dockApp, a.cFinderPath, C.uint64_t(deadlineNS), &x, &y, &axErr, &over)
	switch {
	case bool(found):
		return policy.Point{X: float64(x), Y: float64(y)}, warmOK
	case axErr != 0:
		return policy.Point{}, int(axErr)
	case bool(over):
		return policy.Point{}, warmBudget
	}
	return policy.Point{}, warmIconNotFound
}

// dockLookupCooldownNS is how long the process table is not rescanned after the Dock process was not found.
// A scan that finds nothing takes 1 to 3 ms, which must not be repeated for every event on the tap thread.
const dockLookupCooldownNS uint64 = 1_000_000_000

// ensureDockApp ensures that Dock's application element exists; it returns false while Dock is not running and does not
// look again during the cooldown.
func (a *AXInspector) ensureDockApp() bool {
	if a.dockApp != 0 {
		return true
	}
	nowNS := MonotonicNowNS()
	if nowNS < a.dockLookupAfterNS {
		return false
	}
	a.dockApp = C.fr_ax_create_application(C.pid_t(a.dockPID()))
	if a.dockApp == 0 {
		a.dockLookupAfterNS = nowNS + dockLookupCooldownNS
		return false
	}
	return true
}

// readApplicationRole reads an application element's role and reports an unexpected result when it is not
// AXApplication.
func (a *AXInspector) readApplicationRole(app C.AXUIElementRef, deadlineNS uint64) int {
	var role [64]C.char
	var over C.bool
	if code := int(C.fr_ax_copy_role(a.ref, app, C.uint64_t(deadlineNS), &role[0], C.size_t(len(role)), &over)); code != 0 {
		return code
	}
	if bool(over) {
		return warmBudget
	}
	if C.GoString(&role[0]) != "AXApplication" {
		return warmUnexpected
	}
	return warmOK
}

// HitTest queries the given coordinate before the monotonic clock deadline, with mode deciding the resolution scope; it
// must be called before Close.
// In active mode, a successful recognition (a candidate target) holds the hit element for Recheck comparisons within
// this gesture; EndGesture releases it.
//
// With only Dock enabled (InspectDock), Dock itself is asked first: a position with no Dock element directly yields the
// conclusion "not on Dock" without a system-wide hit, so the clicked app is never queried, never pays its AX cold start
// and is not affected by its stalls.
// When Dock has an element there, a system-wide hit is still done, and attributes are resolved only after it confirms
// that the topmost element at the coordinate belongs to Dock; the in-app hit does not check whether other apps cover it
// from above, so no recognition may rest on it alone.
//
// With the desktop entry enabled (InspectActive), a position with no Dock element is then checked with the window
// server: when a mouse-down would hit a window of an app other than Finder, it cannot be a desktop icon, so the event
// passes through directly, again without querying the clicked app. Finder's in-app hit does not check for covering
// windows and cannot be used for this step.
// When the window belongs to Finder, there is no window, or it cannot be determined, a system-wide hit is done as
// usual, so this step can only cause missed redirects, never wrong ones.
func (a *AXInspector) HitTest(pt policy.Point, deadlineNS uint64, mode InspectMode) AXObservation {
	switch mode {
	case InspectDock:
		if a.dockGate(pt, deadlineNS) == gateMiss {
			return AXObservation{HitSource: SourceOffDock}
		}
	case InspectActive:
		if a.dockGate(pt, deadlineNS) == gateMiss && a.topWindow(pt) == windowOther {
			return AXObservation{HitSource: SourceCovered}
		}
	}
	return a.hitSystem(pt, deadlineNS, mode)
}

// dockGate performs one bounded hit test on Dock's application element only; when Dock is not running the application
// element is unavailable and the result is gateUnavailable.
func (a *AXInspector) dockGate(pt policy.Point, deadlineNS uint64) gateResult {
	if !a.ensureDockApp() {
		return gateUnavailable
	}
	el, axErr, _ := a.hitAt(a.dockApp, pt, deadlineNS)
	if el != 0 {
		C.fr_ax_release(el)
		return gateOnDock
	}
	if axErr == axErrNoValue {
		return gateMiss
	}
	// Other results (error or over budget): the application element may have become invalid after a Dock restart. Drop
	// it so the next query rebuilds it from the process ID; this time fall back to a system-wide hit so redirecting is
	// not interrupted.
	a.dropDockApp()
	return gateUnavailable
}

func (a *AXInspector) dropDockApp() {
	C.fr_ax_release(a.dockApp)
	a.dockApp = 0
}

// hitSystem performs one system-wide hit test and resolves the topmost element.
func (a *AXInspector) hitSystem(pt policy.Point, deadlineNS uint64, mode InspectMode) AXObservation {
	el, axErr, overBudget := a.hitAt(a.ref, pt, deadlineNS)
	if el == 0 {
		return AXObservation{AXError: axErr, BudgetExceeded: overBudget, HitSource: SourceSystem}
	}
	retained := false
	defer func() {
		if !retained {
			C.fr_ax_release(el)
		}
	}()

	observe := mode == InspectObserve
	var out C.fr_ax_hit_t
	C.fr_ax_inspect(a.ref, el, C.uint64_t(deadlineNS), a.cFinderPath, C.bool(mode != InspectDock), C.bool(observe), &out)
	obs := observationFromC(&out, SourceSystem)

	// In active mode the second level of the parent chain is an AXGroup: the URL and selection info are read only after
	// confirming it is the desktop icon container.
	group := out.parent_group
	defer func() {
		if group != 0 {
			C.fr_ax_release(group)
		}
	}()
	if group != 0 && obs.FinderContainer == ContainerDesktopIcon {
		C.fr_ax_read_desktop_evidence(a.ref, el, group, C.uint64_t(deadlineNS), a.cFinderPath, &out)
		applyDesktopEvidence(&out, &obs)
	}

	if !observe {
		if _, ok := candidateTarget(obs); ok {
			a.retain(el, group, obs)
			retained, group = true, 0
		}
	}
	return obs
}

// Recheck verifies that the topmost element at pt is still the element held when this gesture was recognized: it does
// only one system-wide hit, compares with CFEqual and does not reread attributes. With needSelection (the second
// desktop click) it also reads the container's selection set once, so the single-selection evidence comes from the
// current state.
// When they are equal it returns the recognition observation (with up-to-date selection info); otherwise the returned
// observation never becomes a candidate target.
func (a *AXInspector) Recheck(pt policy.Point, deadlineNS uint64, needSelection bool) AXObservation {
	if !a.hasHeld {
		return AXObservation{HitSource: SourceChanged}
	}
	el, axErr, overBudget := a.hitAt(a.ref, pt, deadlineNS)
	if el == 0 {
		return AXObservation{AXError: axErr, BudgetExceeded: overBudget, HitSource: SourceSystem}
	}
	same := bool(C.fr_ax_elements_equal(el, a.held))
	C.fr_ax_release(el)
	if !same {
		return AXObservation{HitSource: SourceChanged}
	}

	obs := a.heldObs
	obs.HitSource = SourceSame
	if needSelection && a.heldGroup != 0 {
		var out C.fr_ax_hit_t
		C.fr_ax_read_selection(a.ref, a.heldGroup, a.held, C.uint64_t(deadlineNS), &out)
		obs.HasSelectionInfo = bool(out.has_selection_info)
		obs.SelectionCount = int32(out.selection_count)
		obs.IsSingleSelectedSelf = bool(out.is_single_selected_self)
		if bool(out.budget_exceeded) {
			obs.BudgetExceeded = true
		}
	}
	return obs
}

// observationFromC converts one native inspection result into a redacted observation.
func observationFromC(out *C.fr_ax_hit_t, source HitSource) AXObservation {
	obs := AXObservation{
		AXError:              int(out.ax_error),
		BudgetExceeded:       bool(out.budget_exceeded),
		PID:                  int32(out.pid),
		ProcGen:              uint64(out.proc_gen),
		ProcessKind:          ProcessKind(out.proc_kind),
		IsDockFinder:         bool(out.is_dock_finder),
		ElementHash:          uint64(out.element_hash),
		HasSelectionInfo:     bool(out.has_selection_info),
		SelectionCount:       int32(out.selection_count),
		IsSingleSelectedSelf: bool(out.is_single_selected_self),
		Role:                 C.GoString(&out.role[0]),
		Subrole:              C.GoString(&out.subrole[0]),
		ParentChain:          C.GoString(&out.parent_chain[0]),
		URLKind:              URLKind(out.url_kind),
		FilePath:             C.GoString(&out.file_path[0]),
		HitSource:            source,
	}
	for i := 0; i < int(out.role_count) && i < len(out.roles); i++ {
		obs.Roles = append(obs.Roles, C.GoString(&out.roles[i][0]))
	}
	if obs.ProcessKind == ProcessFinder {
		obs.FinderContainer = classifyFinderContainer(obs.Roles)
	}
	return obs
}

// applyDesktopEvidence merges the URL and selection info read after confirming the container into the observation.
func applyDesktopEvidence(out *C.fr_ax_hit_t, obs *AXObservation) {
	obs.BudgetExceeded = obs.BudgetExceeded || bool(out.budget_exceeded)
	obs.URLKind = URLKind(out.url_kind)
	obs.FilePath = C.GoString(&out.file_path[0])
	obs.HasSelectionInfo = bool(out.has_selection_info)
	obs.SelectionCount = int32(out.selection_count)
	obs.IsSingleSelectedSelf = bool(out.is_single_selected_self)
}

// classifyFinderContainer determines the structural container of a Finder hit from the role sequence going up from the
// hit element (index 0 is the hit element).
// It uses only structural roles, never localized names: the native desktop's scroll area sits directly under
// AXApplication with no AXWindow in the parent chain, and any hit inside a window is classified as the window. A
// structure that matches no known shape is classified as other, and the caller passes it through on the native path.
func classifyFinderContainer(roles []string) FinderContainer {
	for _, role := range roles {
		if role == "AXWindow" {
			return ContainerFinderWindow
		}
	}
	n := len(roles)
	if n < 2 || roles[n-1] != "AXApplication" || roles[n-2] != "AXScrollArea" {
		return ContainerFinderOther
	}
	switch {
	case n == 2:
		// A direct hit on the desktop scroll area itself.
		return ContainerDesktopBackground
	case n == 3 && roles[0] == "AXGroup":
		// Empty desktop space: the group that contains all icons.
		return ContainerDesktopBackground
	case n == 4 && roles[0] == "AXImage" && roles[1] == "AXGroup":
		return ContainerDesktopIcon
	}
	return ContainerFinderOther
}

// candidateTarget reports whether a hit can become a candidate target and gives the target identity used by the state
// machine.
// Only Dock's Finder icon, and icons in the native desktop container that directly provide a local file URL, become
// candidate targets.
func candidateTarget(obs AXObservation) (policy.Target, bool) {
	switch {
	case obs.IsDockFinder && obs.PID > 0:
		return policy.Target{
			Kind:     policy.TargetDockFinder,
			Identity: fmt.Sprintf("dock:%d:finder", obs.PID),
		}, true
	case obs.ProcessKind == ProcessFinder && obs.FinderContainer == ContainerDesktopIcon &&
		obs.URLKind == URLFile && obs.FilePath != "":
		return policy.Target{
			Kind:     policy.TargetDesktopDirectory,
			Path:     obs.FilePath,
			Identity: fmt.Sprintf("desk:%d:%x:%x", obs.PID, obs.ProcGen, obs.ElementHash),
			// The identity includes the Finder process generation and the AX object hash, so it changes when Finder
			// restarts or the icon is replaced by another object.
			SingleSelected: obs.HasSelectionInfo && obs.SelectionCount == 1 && obs.IsSingleSelectedSelf,
		}, true
	}
	return policy.Target{}, false
}

// TargetFromObservation converts a hit observation into a policy.HitResult for the pure Go state machine.
func TargetFromObservation(obs AXObservation) policy.HitResult {
	if obs.BudgetExceeded {
		return policy.HitResult{BudgetExceeded: true}
	}
	if obs.AXError != 0 && !obs.IsDockFinder {
		return policy.HitResult{Err: errAXLookupFailed}
	}
	if target, ok := candidateTarget(obs); ok {
		return policy.HitResult{Target: target}
	}
	return policy.HitResult{Target: policy.Target{Kind: policy.TargetUnknown}}
}

// TapDisableReason distinguishes why the system disabled the CGEventTap.
type TapDisableReason uint8

const (
	TapDisabledByTimeout TapDisableReason = iota + 1
	TapDisabledByUserInput
)

// TapDisposition is the action the underlying EventTap takes on the current mouse event.
type TapDisposition uint8

const (
	// TapPass passes the event through unchanged.
	TapPass TapDisposition = 0
	// TapSwallow swallows the event (returns NULL).
	TapSwallow TapDisposition = 1
	// TapCancelUp rewrites the paired mouse-up of a passed-through mouse-down into an off-screen drag-out release,
	// clearing Dock's pressed highlight and long-press menu.
	TapCancelUp TapDisposition = 2
	// TapCancelDesktopUp rewrites the passed-through mouse-up of the second desktop click into an off-screen release
	// and clears click-state, so Finder does not recognize a double-click.
	TapCancelDesktopUp TapDisposition = 3
)

// TapCallbacks defines the synchronous callback interface that runs on the dedicated tap OS thread.
type TapCallbacks interface {
	OnMouseEvent(ev policy.Event) TapDisposition
	OnTapDisabled(reason TapDisableReason, tap *EventTap)
}

// EventTap manages a CGEventTap and a CFRunLoop running on a separate locked OS thread.
type EventTap struct {
	mu      sync.Mutex
	ptr     *C.fr_tap_t
	doneCh  chan struct{}
	stopped bool
}

// StartEventTap creates and runs a CGEventTap on a separate LockOSThread thread.
// The cgo.Handle is released only after stopping and waiting for Wait() to return, which guarantees that no callback is
// left dangling.
func StartEventTap(listenOnly bool, cb TapCallbacks) (*EventTap, error) {
	if cb == nil {
		return nil, errors.New("nil TapCallbacks")
	}

	et := &EventTap{
		doneCh: make(chan struct{}),
	}
	readyCh := make(chan error, 1)

	go func() {
		// Lock a dedicated OS thread and never call UnlockOSThread on exit, so the Go runtime reclaims the thread
		// directly; this keeps a thread that held native CFRunLoop state from returning to the pool and polluting later
		// work.
		runtime.LockOSThread()

		holder := &tapCallbackHolder{cb: cb, tap: et}
		handle := cgo.NewHandle(holder)
		defer handle.Delete()

		ptr := C.fr_tap_create(C.uintptr_t(handle), C.bool(listenOnly))
		if ptr == nil {
			readyCh <- ErrEventTapCreate
			close(et.doneCh)
			return
		}

		et.mu.Lock()
		et.ptr = ptr
		alreadyStopped := et.stopped
		et.mu.Unlock()

		readyCh <- nil

		if !alreadyStopped {
			C.fr_tap_run(ptr)
		}

		et.mu.Lock()
		et.ptr = nil
		et.mu.Unlock()

		C.fr_tap_destroy(ptr)
		close(et.doneCh)
	}()

	if err := <-readyCh; err != nil {
		<-et.doneCh
		return nil, err
	}
	return et, nil
}

// SetEnabled re-enables or disables the current tap from a tap thread callback or the control path.
func (t *EventTap) SetEnabled(enabled bool) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	ptr := t.ptr
	stopped := t.stopped
	t.mu.Unlock()
	if stopped || ptr == nil {
		return false
	}
	return bool(C.fr_tap_set_enabled(ptr, C.bool(enabled)))
}

// Stop requests that the dedicated tap RunLoop be disabled and exit, and blocks until the current callback has fully
// finished.
func (t *EventTap) Stop() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.stopped = true
	ptr := t.ptr
	t.mu.Unlock()

	if ptr != nil {
		C.fr_tap_stop(ptr)
	}
	<-t.doneCh
}

type tapCallbackHolder struct {
	cb  TapCallbacks
	tap *EventTap
}

//export goOnTapEvent
func goOnTapEvent(
	handle C.uintptr_t,
	eventType C.uint32_t,
	x C.double,
	y C.double,
	clickCount C.int64_t,
	flags C.uint64_t,
	timeNS C.uint64_t,
) C.int {
	h := cgo.Handle(handle)
	holder, ok := h.Value().(*tapCallbackHolder)
	if !ok || holder == nil || holder.cb == nil {
		return C.FR_TAP_PASS
	}

	switch uint32(eventType) {
	case cEventTapDisabledTimeout:
		holder.cb.OnTapDisabled(TapDisabledByTimeout, holder.tap)
		return C.FR_TAP_PASS
	case cEventTapDisabledUserInput:
		holder.cb.OnTapDisabled(TapDisabledByUserInput, holder.tap)
		return C.FR_TAP_PASS
	}

	var kind policy.EventKind
	switch uint32(eventType) {
	case cEventLeftMouseDown:
		kind = policy.EventDown
	case cEventLeftMouseUp:
		kind = policy.EventUp
	case cEventLeftMouseDragged:
		kind = policy.EventDragged
	default:
		return C.FR_TAP_PASS
	}

	var mods policy.Modifiers
	f := uint64(flags)
	if f&cFlagShift != 0 {
		mods |= policy.ModShift
	}
	if f&cFlagControl != 0 {
		mods |= policy.ModControl
	}
	if f&cFlagOption != 0 {
		mods |= policy.ModOption
	}
	if f&cFlagCommand != 0 {
		mods |= policy.ModCommand
	}

	ev := policy.Event{
		Kind:       kind,
		Point:      policy.Point{X: float64(x), Y: float64(y)},
		ClickCount: int(clickCount),
		Modifiers:  mods,
		TimeNS:     uint64(timeNS),
	}
	switch holder.cb.OnMouseEvent(ev) {
	case TapSwallow:
		return C.FR_TAP_SWALLOW
	case TapCancelUp:
		return C.FR_TAP_CANCEL_UP
	case TapCancelDesktopUp:
		return C.FR_TAP_CANCEL_DESKTOP_UP
	default:
		return C.FR_TAP_PASS
	}
}

var (
	wsReqSeq  atomic.Uint64
	wsPending sync.Map // map[uint64]func(error)

	resetMu       sync.Mutex
	resetCallback func()
)

// OpenApplicationAsync schedules NSWorkspace.openApplication(at:configuration:completionHandler:) on the main queue;
// the caller guarantees that onComplete is non-nil, and it is called at most once.
func OpenApplicationAsync(appPath string, onComplete func(error)) {
	reqID := wsReqSeq.Add(1)
	wsPending.Store(reqID, onComplete)
	cPath := C.CString(appPath)
	defer C.free(unsafe.Pointer(cPath))
	C.fr_open_application_async(cPath, C.uint64_t(reqID))
}

// OpenDirectoryAsync schedules NSWorkspace.openURLs:withApplicationAtURL:configuration:completionHandler: on the main
// queue to send a single directory URL to the given app; the caller guarantees that onComplete is non-nil, and it is
// called at most once.
func OpenDirectoryAsync(dirPath, appPath string, onComplete func(error)) {
	reqID := wsReqSeq.Add(1)
	wsPending.Store(reqID, onComplete)
	cDir := C.CString(dirPath)
	defer C.free(unsafe.Pointer(cDir))
	cApp := C.CString(appPath)
	defer C.free(unsafe.Pointer(cApp))
	C.fr_open_directory_async(cDir, cApp, C.uint64_t(reqID))
}

// ClassifyDirectory uses lstat and NSURLResourceKey to decide whether a candidate path is an eligible local regular
// directory, and returns a file identity (device number and inode) that can be re-verified; ineligible paths have
// identity 0.
func ClassifyDirectory(path string) (status DirStatus, dev uint64, ino uint64) {
	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))
	var cDev, cIno C.uint64_t
	st := C.fr_classify_directory(cPath, &cDev, &cIno)
	return DirStatus(st), uint64(cDev), uint64(cIno)
}

//export goOnWorkspaceComplete
func goOnWorkspaceComplete(reqID C.uint64_t, failed C._Bool, errCode C.int, errDomain *C.char) {
	v, ok := wsPending.LoadAndDelete(uint64(reqID))
	if !ok {
		return
	}
	fn, ok := v.(func(error))
	if !ok || fn == nil {
		return
	}
	if !bool(failed) {
		fn(nil)
		return
	}
	domain := ""
	if errDomain != nil {
		domain = C.GoString(errDomain)
	}
	fn(fmt.Errorf("NSWorkspace open failed (domain=%s code=%d)", domain, int(errCode)))
}

// SetSystemResetHandler registers the cleanup callback run on system sleep/wake or display layout changes.
func SetSystemResetHandler(fn func()) {
	resetMu.Lock()
	resetCallback = fn
	resetMu.Unlock()
}

//export goOnSystemReset
func goOnSystemReset() {
	resetMu.Lock()
	fn := resetCallback
	resetMu.Unlock()
	if fn != nil {
		fn()
	}
}

// PrepareMainLoop sets up the main-thread CFRunLoop keep-alive source and system event observers (it must be called on
// the main thread).
func PrepareMainLoop() {
	C.fr_main_loop_prepare()
}

// RunMainLoop runs the CFRunLoop on the main thread, blocking, and serves NSWorkspace main-queue requests without
// polling.
func RunMainLoop() {
	C.fr_main_loop_run()
}

// StopMainLoop wakes and exits the main-thread CFRunLoop (safe to call from any goroutine).
func StopMainLoop() {
	C.fr_main_loop_stop()
}
