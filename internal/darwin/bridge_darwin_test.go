package darwin

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/XGrin/findrelay/internal/appbundle"
	"github.com/XGrin/findrelay/internal/policy"
)

func TestMonotonicNowNS(t *testing.T) {
	t1 := MonotonicNowNS()
	time.Sleep(1 * time.Millisecond)
	t2 := MonotonicNowNS()
	if t1 == 0 || t2 <= t1 {
		t.Fatalf("monotonic clock did not advance: t1=%d t2=%d", t1, t2)
	}
}

func TestCurrentGUISession(t *testing.T) {
	sess, err := CurrentGUISession()
	if err != nil {
		t.Fatalf("CurrentGUISession() error = %v", err)
	}
	if sess.ID == 0 {
		t.Fatalf("expected non-zero SecuritySessionId, got %+v", sess)
	}
}

func TestFinderAndInspectAppBundle(t *testing.T) {
	finderPath := FinderApplicationPath()
	if finderPath == "" || finderPath[0] != '/' {
		t.Fatalf("unexpected FinderApplicationPath() = %q", finderPath)
	}

	info, err := InspectAppBundle(finderPath)
	if err != nil {
		t.Fatalf("InspectAppBundle(%q) error = %v", finderPath, err)
	}
	if info.BundleID != appbundle.FinderBundleID {
		t.Fatalf("Finder BundleID = %q, want %q", info.BundleID, appbundle.FinderBundleID)
	}
	if info.ExecutablePath == "" {
		t.Fatalf("Finder ExecutablePath is empty")
	}

	// With the native CFBundle Inspector, appbundle.ValidateApplication must reject Finder.
	_, err = appbundle.ValidateApplication(finderPath, appbundle.ValidateOptions{
		Inspector:     InspectAppBundle,
		FinderAppPath: finderPath,
	})
	if err == nil {
		t.Fatalf("expected ValidateApplication to reject Finder.app")
	}

	if _, err := InspectAppBundle("/nonexistent/NoSuch.app"); err == nil {
		t.Fatalf("expected error for nonexistent bundle")
	}
}

func TestIsProcessTrustedNoPrompt(t *testing.T) {
	// Only verify that the no-prompt preflight is safe to call.
	_ = IsProcessTrusted(false)
}

func TestProjectDockEdgePoint(t *testing.T) {
	// Multi-display layout:
	// Display 0: [0, 0, 1920, 1080] (outer right edge; bottom partly shared with Display 1; left shared with Display 2)
	// Display 1: [204, 1080, 1512, 982] (y: 1080..2062; left, right and bottom are all outer edges)
	// Display 2: [-1280, 0, 1280, 1024] (x: -1280..0; left edge -1280 is outer, bottom edge 1024 is outer)
	displays := []DisplayRect{
		{X: 0, Y: 0, Width: 1920, Height: 1080},
		{X: 204, Y: 1080, Width: 1512, Height: 982},
		{X: -1280, Y: 0, Width: 1280, Height: 1024},
	}

	cases := []struct {
		name string
		in   policy.Point
		want policy.Point
	}{
		{name: "bottom outer edge", in: policy.Point{X: 521, Y: 2062}, want: policy.Point{X: 521, Y: 2054}},
		{name: "bottom fractional retina edge", in: policy.Point{X: 520.25, Y: 2061.5}, want: policy.Point{X: 520.25, Y: 2054}},
		{name: "right outer edge", in: policy.Point{X: 1920, Y: 500}, want: policy.Point{X: 1912, Y: 500}},
		{name: "left outer edge negative coords", in: policy.Point{X: -1280, Y: 500}, want: policy.Point{X: -1272, Y: 500}},
		{name: "bottom-right corner outer edge", in: policy.Point{X: 1716, Y: 2062}, want: policy.Point{X: 1708, Y: 2054}},
		{name: "shared horizontal boundary stays unchanged", in: policy.Point{X: 521, Y: 1080}, want: policy.Point{X: 521, Y: 1080}},
		{name: "shared vertical boundary stays unchanged", in: policy.Point{X: 0, Y: 500}, want: policy.Point{X: 0, Y: 500}},
		{name: "interior point unchanged", in: policy.Point{X: 845, Y: 654}, want: policy.Point{X: 845, Y: 654}},
		{name: "off-screen point not pulled inward", in: policy.Point{X: 521, Y: 2063}, want: policy.Point{X: 521, Y: 2063}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ProjectDockEdgePoint(tc.in, displays)
			if got != tc.want {
				t.Fatalf("ProjectDockEdgePoint(%+v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestAXInspectorLifecycle(t *testing.T) {
	ax, err := NewAXInspector(FinderApplicationPath())
	if err != nil {
		t.Fatalf("NewAXInspector error = %v", err)
	}
	defer ax.Close()

	// A deadline that has already passed should set BudgetExceeded immediately without blocking.
	obs := ax.HitTest(policy.Point{X: 100, Y: 100}, 1, InspectDock)
	if !obs.BudgetExceeded {
		t.Fatalf("expected BudgetExceeded=true for expired deadline, got %+v", obs)
	}
}

func TestInspectAppBundleNoStaleCacheOnInPlaceUpdate(t *testing.T) {
	appDir := filepath.Join(t.TempDir(), "Mutable.app")
	macosDir := filepath.Join(appDir, "Contents", "MacOS")
	if err := os.MkdirAll(macosDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	writeVersion := func(bundleID, execName string) {
		plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>CFBundleIdentifier</key>
	<string>%s</string>
	<key>CFBundleExecutable</key>
	<string>%s</string>
</dict>
</plist>
`, bundleID, execName)
		if err := os.WriteFile(filepath.Join(appDir, "Contents", "Info.plist"), []byte(plist), 0o644); err != nil {
			t.Fatalf("write plist: %v", err)
		}
		if err := os.WriteFile(filepath.Join(macosDir, execName), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write exec: %v", err)
		}
	}

	writeVersion("com.example.v1", "AppV1")
	info1, err := InspectAppBundle(appDir)
	if err != nil {
		t.Fatalf("InspectAppBundle v1 error = %v", err)
	}
	if info1.BundleID != "com.example.v1" || filepath.Base(info1.ExecutablePath) != "AppV1" {
		t.Fatalf("unexpected v1 info: %+v", info1)
	}

	// Update Info.plist and the executable name in place at the same path, and verify the CFBundle cache does not mask
	// the change.
	_ = os.Remove(filepath.Join(macosDir, "AppV1"))
	writeVersion("com.example.v2", "AppV2")
	info2, err := InspectAppBundle(appDir)
	if err != nil {
		t.Fatalf("InspectAppBundle v2 error = %v", err)
	}
	if info2.BundleID != "com.example.v2" || filepath.Base(info2.ExecutablePath) != "AppV2" {
		t.Fatalf("stale bundle metadata returned: %+v", info2)
	}

	// After Info.plist is deleted, the bundle must be recognized as invalid immediately.
	if err := os.Remove(filepath.Join(appDir, "Contents", "Info.plist")); err != nil {
		t.Fatalf("remove Info.plist: %v", err)
	}
	if _, err := InspectAppBundle(appDir); err == nil {
		t.Fatalf("expected error after Info.plist removal")
	}
}

func TestMainLoopEarlyStopCleanup(t *testing.T) {
	// 1. Simulate a startTap failure: call StopMainLoop right after PrepareMainLoop, without calling RunMainLoop.
	PrepareMainLoop()
	StopMainLoop()

	// 2. Simulate a stop signal received before entering RunMainLoop: RunMainLoop should clean up and return immediately.
	PrepareMainLoop()
	StopMainLoop()
	RunMainLoop()
}

func warmupStepNames(report AXWarmupReport) []string {
	names := make([]string, 0, len(report.Steps))
	for _, step := range report.Steps {
		names = append(names, step.Name)
	}
	return names
}

// The warm-up steps share one overall deadline: when the budget is used up, every step reports a timeout immediately
// without issuing any query.
func TestAXInspectorWarmUpReportsExpiredBudgetWithoutQueries(t *testing.T) {
	ax, err := NewAXInspector(FinderApplicationPath())
	if err != nil {
		t.Fatalf("NewAXInspector error = %v", err)
	}
	defer ax.Close()

	start := time.Now()
	report := ax.WarmUp(InspectActive, 1)
	if elapsed := time.Since(start); elapsed > 20*time.Millisecond {
		t.Fatalf("expired warmup took %s, want an immediate return", elapsed)
	}
	if got, want := warmupStepNames(report), []string{"dock_read", "dock_hit", "window_hit", "finder_read"}; !slices.Equal(got, want) {
		t.Fatalf("warmup steps = %v, want %v", got, want)
	}
	for _, step := range report.Steps {
		if step.Status != warmBudget {
			t.Errorf("step %s status = %d, want budget (%d)", step.Name, step.Status, warmBudget)
		}
	}
	if report.Complete() {
		t.Error("an expired warmup must not report success")
	}
	if !strings.Contains(report.Describe(), "dock_read=budget/0.0ms") {
		t.Errorf("unexpected report text %q", report.Describe())
	}
}

// The real warm-up returns within the total time limit: after the deadline it waits at most one more in-flight message
// timeout. The report contains only steps, result codes and timings.
func TestAXInspectorWarmUpStaysWithinBudget(t *testing.T) {
	ax, err := NewAXInspector(FinderApplicationPath())
	if err != nil {
		t.Fatalf("NewAXInspector error = %v", err)
	}
	defer ax.Close()

	const budget = 2 * time.Second
	start := time.Now()
	report := ax.WarmUp(InspectActive, uint64(budget))
	if elapsed := time.Since(start); elapsed > budget+300*time.Millisecond {
		t.Fatalf("warmup took %s, beyond the %s budget", elapsed, budget)
	}
	if got, want := warmupStepNames(report), []string{"dock_read", "dock_hit", "window_hit", "finder_read"}; !slices.Equal(got, want) {
		t.Fatalf("warmup steps = %v, want %v", got, want)
	}
	text := report.Describe()
	for _, secret := range []string{"/System/", "Finder.app", "Dock.app", "com.apple"} {
		if strings.Contains(text, secret) {
			t.Errorf("warmup report leaked %q: %s", secret, text)
		}
	}
	for _, step := range report.Steps {
		switch name := warmStatusName(step.Status); name {
		case "ok", "no_process", "icon_not_found", "budget", "unexpected", "not_visible":
		default:
			if !strings.HasPrefix(name, "ax:") {
				t.Errorf("step %s has unknown status %q", step.Name, name)
			}
		}
	}
}

// When the Dock icon is not visible at the time (auto-hide, full-screen Space), the warm-up queries have still completed:
// the report records not_visible and counts as done.
func TestWarmupReportTreatsHiddenDockAsComplete(t *testing.T) {
	report := AXWarmupReport{Steps: []WarmupStep{
		{Name: "dock_read", Status: warmOK},
		{Name: "dock_hit", Status: warmNotVisible, ElapsedNS: 2_000_000},
	}}
	if !report.Complete() {
		t.Fatal("a hidden Dock must not make the warmup incomplete")
	}
	if got := report.Describe(); got != "dock_read=ok/0.0ms dock_hit=not_visible/2.0ms" {
		t.Fatalf("Describe() = %q", got)
	}
	report.Steps[1].Status = warmUnexpected
	if report.Complete() {
		t.Fatal("an unexpected result must still make the warmup incomplete")
	}
}
