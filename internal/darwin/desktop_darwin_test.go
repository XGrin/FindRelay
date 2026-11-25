package darwin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"testing"
	"time"

	"github.com/XGrin/findrelay/internal/policy"
)

func TestDoubleClickIntervalMatchesSystemSetting(t *testing.T) {
	interval := DoubleClickIntervalNS()
	if interval < uint64(100*time.Millisecond) || interval > uint64(5*time.Second) {
		t.Fatalf("double click interval = %s, want a plausible system setting", time.Duration(interval))
	}
	if again := DoubleClickIntervalNS(); again != interval {
		t.Fatalf("double click interval changed between calls: %s then %s", time.Duration(interval), time.Duration(again))
	}
}

func lstatIdentity(t *testing.T, path string) (dev, ino uint64) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("lstat result has no stat_t")
	}
	return uint64(st.Dev), uint64(st.Ino)
}

func TestClassifyDirectoryAcceptsPlainDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "工作 目录")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	status, dev, ino := ClassifyDirectory(dir)
	if status != DirEligible {
		t.Fatalf("status = %s, want eligible", status)
	}
	wantDev, wantIno := lstatIdentity(t, dir)
	if dev != wantDev || ino != wantIno {
		t.Fatalf("identity = (%d, %d), want lstat (%d, %d)", dev, ino, wantDev, wantIno)
	}
}

func TestClassifyDirectoryRejectsOutOfScopeObjects(t *testing.T) {
	root := t.TempDir()
	plain := filepath.Join(root, "Plain")
	if err := os.Mkdir(plain, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	file := filepath.Join(root, "note.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Symlink(plain, filepath.Join(root, "Link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "missing"), filepath.Join(root, "Dangling")); err != nil {
		t.Fatalf("dangling symlink: %v", err)
	}
	appBundle := filepath.Join(root, "Notes.app", "Contents")
	if err := os.MkdirAll(appBundle, 0o755); err != nil {
		t.Fatalf("mkdir app: %v", err)
	}
	if err := os.WriteFile(filepath.Join(appBundle, "Info.plist"), []byte("<plist/>"), 0o644); err != nil {
		t.Fatalf("write Info.plist: %v", err)
	}
	rtfd := filepath.Join(root, "Letter.rtfd")
	if err := os.MkdirAll(rtfd, 0o755); err != nil {
		t.Fatalf("mkdir rtfd: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rtfd, "TXT.rtf"), []byte("{\\rtf1}"), 0o644); err != nil {
		t.Fatalf("write rtf: %v", err)
	}

	cases := []struct {
		name string
		path string
		want DirStatus
	}{
		{name: "regular file", path: file, want: DirRejectNotDirectory},
		{name: "symlink to directory", path: filepath.Join(root, "Link"), want: DirRejectSymlink},
		{name: "dangling symlink", path: filepath.Join(root, "Dangling"), want: DirRejectSymlink},
		{name: "application bundle", path: filepath.Join(root, "Notes.app"), want: DirRejectPackage},
		{name: "system application", path: "/System/Applications/Calculator.app", want: DirRejectPackage},
		{name: "rtfd package", path: rtfd, want: DirRejectPackage},
		{name: "volume root", path: "/", want: DirRejectVolume},
		{name: "missing path", path: filepath.Join(root, "Missing"), want: DirRejectNotFound},
		{name: "relative path", path: "relative/dir", want: DirRejectNotDirectory},
		{name: "empty path", path: "", want: DirRejectNotDirectory},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, dev, ino := ClassifyDirectory(tc.path)
			if status != tc.want {
				t.Fatalf("status = %s, want %s", status, tc.want)
			}
			if dev != 0 || ino != 0 {
				t.Fatal("rejected paths must not carry a file identity")
			}
		})
	}
}

func TestClassifyDirectoryReportsPermissionError(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "blocked")
	child := filepath.Join(blocked, "child")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if status, _, _ := ClassifyDirectory(child); status != DirErrPermission {
		t.Fatalf("status = %s, want a permission error", status)
	}
}

func TestRevalidateDirectoryTracksObjectIdentity(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Dir")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	status, dev, ino := ClassifyDirectory(dir)
	if status != DirEligible {
		t.Fatalf("status = %s, want eligible", status)
	}
	if err := RevalidateDirectory(dir, dev, ino); err != nil {
		t.Fatalf("unchanged directory failed revalidation: %v", err)
	}
	if err := RevalidateDirectory(dir, dev+1, ino); !errors.Is(err, ErrDirectoryIdentityChanged) {
		t.Fatalf("device mismatch error = %v, want ErrDirectoryIdentityChanged", err)
	}
	if err := RevalidateDirectory(dir, dev, ino+1); !errors.Is(err, ErrDirectoryIdentityChanged) {
		t.Fatalf("inode mismatch error = %v, want ErrDirectoryIdentityChanged", err)
	}

	// Moved away: the path disappearing counts as an identity change.
	moved := filepath.Join(root, "Moved")
	if err := os.Rename(dir, moved); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := RevalidateDirectory(dir, dev, ino); !errors.Is(err, ErrDirectoryIdentityChanged) {
		t.Fatalf("moved directory error = %v, want ErrDirectoryIdentityChanged", err)
	}

	// A new directory recreated at the same path: the inode differs.
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	if err := RevalidateDirectory(dir, dev, ino); !errors.Is(err, ErrDirectoryIdentityChanged) {
		t.Fatalf("recreated directory error = %v, want ErrDirectoryIdentityChanged", err)
	}

	// The same path replaced by a symlink or a regular file.
	if err := os.Remove(dir); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Symlink(moved, dir); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := RevalidateDirectory(dir, dev, ino); !errors.Is(err, ErrDirectoryIdentityChanged) {
		t.Fatalf("symlink replacement error = %v, want ErrDirectoryIdentityChanged", err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatalf("remove symlink: %v", err)
	}
	if err := os.WriteFile(dir, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := RevalidateDirectory(dir, dev, ino); !errors.Is(err, ErrDirectoryIdentityChanged) {
		t.Fatalf("file replacement error = %v, want ErrDirectoryIdentityChanged", err)
	}
}

func isDesktopFolder(path string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	return path == filepath.Join(home, "Desktop") ||
		path == filepath.Join(home, "Library", "Mobile", "Documents", "com~apple~CloudDocs", "Desktop")
}

// Role sequences come from real-machine observe samples: index 0 is the hit element, going up to AXApplication.
func TestClassifyFinderContainer(t *testing.T) {
	cases := []struct {
		name  string
		roles []string
		want  FinderContainer
	}{
		{name: "desktop icon", roles: []string{"AXImage", "AXGroup", "AXScrollArea", "AXApplication"}, want: ContainerDesktopIcon},
		{name: "unknown element under desktop group", roles: []string{"AXStaticText", "AXGroup", "AXScrollArea", "AXApplication"}, want: ContainerFinderOther},
		{name: "desktop background group", roles: []string{"AXGroup", "AXScrollArea", "AXApplication"}, want: ContainerDesktopBackground},
		{name: "desktop scroll area", roles: []string{"AXScrollArea", "AXApplication"}, want: ContainerDesktopBackground},
		{name: "finder window text", roles: []string{"AXStaticText", "AXWindow", "AXApplication"}, want: ContainerFinderWindow},
		{name: "finder window close button", roles: []string{"AXButton", "AXWindow", "AXApplication"}, want: ContainerFinderWindow},
		{name: "finder window scroll area", roles: []string{"AXScrollArea", "AXWindow", "AXApplication"}, want: ContainerFinderWindow},
		{name: "icon inside finder window", roles: []string{"AXImage", "AXGroup", "AXScrollArea", "AXWindow", "AXApplication"}, want: ContainerFinderWindow},
		{name: "icon in a nested group", roles: []string{"AXImage", "AXGroup", "AXGroup", "AXScrollArea", "AXApplication"}, want: ContainerFinderOther},
		{name: "other finder surface", roles: []string{"AXMenuButton", "AXMenuBar", "AXMenuBar", "AXApplication"}, want: ContainerFinderOther},
		{name: "truncated before application", roles: []string{"AXImage", "AXGroup", "AXScrollArea"}, want: ContainerFinderOther},
		{name: "single element", roles: []string{"AXImage"}, want: ContainerFinderOther},
		{name: "empty", roles: nil, want: ContainerFinderOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyFinderContainer(tc.roles); got != tc.want {
				t.Fatalf("classifyFinderContainer(%v) = %s, want %s", tc.roles, got, tc.want)
			}
		})
	}
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p*float64(len(sorted)-1) + 0.5)
	return sorted[idx]
}

// TestDesktopIconHitTestIntegration scans the main display on a real machine and verifies the hit evidence for native
// desktop icons: a structural container (no AXWindow ancestor), the local file URL's parent directory is the Desktop
// folder, stable identity, usable directory classification, and hit latency percentiles. It logs only counts and
// timings, never any path or name.
// The scan uses a fixed coarse grid covering common display sizes; coordinates outside the displays only get no hit.
func TestDesktopIconHitTestIntegration(t *testing.T) {
	if os.Getenv("FINDRELAY_AX_INTEGRATION") != "1" {
		t.Skip("set FINDRELAY_AX_INTEGRATION=1 to run the real Accessibility hit test")
	}
	if !IsProcessTrusted(false) {
		t.Skip("Accessibility trust is required")
	}
	ax, err := NewAXInspector("")
	if err != nil {
		t.Fatalf("NewAXInspector: %v", err)
	}
	defer ax.Close()

	const (
		step  = 64.0
		maxX  = 4000.0
		maxY  = 2200.0
		inset = 16.0
	)
	type sample struct {
		obs AXObservation
		at  policy.Point
	}
	var (
		latencies                 []time.Duration
		icons                     []sample
		background, windows, bare int
		overBudget                int
		axErrors                  = map[int]int{}
		procKinds                 = map[ProcessKind]int{}
	)
	// The first cross-process AX call includes connection setup, so it is warmed up separately and excluded from the
	// timings.
	for i := 0; i < 3; i++ {
		ax.HitTest(policy.Point{X: inset + step, Y: inset + step}, MonotonicNowNS()+uint64(250*time.Millisecond), InspectObserve)
	}
	for y := inset; y < maxY; y += step {
		for x := inset; x < maxX; x += step {
			at := policy.Point{X: x, Y: y}
			start := time.Now()
			obs := ax.HitTest(at, MonotonicNowNS()+uint64(50*time.Millisecond), InspectObserve)
			latencies = append(latencies, time.Since(start))
			if obs.BudgetExceeded {
				overBudget++
			}
			axErrors[obs.AXError]++
			procKinds[obs.ProcessKind]++
			if obs.ProcessKind != ProcessFinder {
				continue
			}
			switch obs.FinderContainer {
			case ContainerDesktopBackground:
				background++
			case ContainerFinderWindow:
				windows++
			case ContainerDesktopIcon:
				if obs.URLKind != URLFile {
					bare++
					continue
				}
				icons = append(icons, sample{obs: obs, at: at})
			}
		}
	}

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	if len(latencies) == 0 {
		t.Fatal("scanned an empty display region")
	}
	t.Logf("scan: hits=%d icons=%d background=%d windows=%d icons_without_file_url=%d over_budget=%d latency p50=%s p95=%s max=%s",
		len(latencies), len(icons), background, windows, bare, overBudget,
		percentile(latencies, 0.50), percentile(latencies, 0.95), latencies[len(latencies)-1])
	t.Logf("scan: ax errors=%v process kinds=%v", axErrors, procKinds)
	// kAXErrorAPIDisabled (-25200) while the process is authorized usually means the screen is locked or the display is
	// asleep, so AX is unavailable.
	// The scan asks every app on screen point by point, and occasionally one app takes longer than 50 ms for a single
	// response; only a persistent slowdown counts as failure, so up to 0.5% of points may time out.
	if overBudget*200 > len(latencies) {
		t.Fatalf("%d of %d hit tests exceeded the 50ms deadline, want at most 0.5%%", overBudget, len(latencies))
	}
	if p95 := percentile(latencies, 0.95); p95 > 10*time.Millisecond {
		t.Fatalf("hit test p95 = %s, want <= 10ms", p95)
	}
	if background+windows+len(icons)+bare == 0 {
		if axErrors[-25200] > 0 {
			t.Skip("Accessibility API disabled although the process is trusted: unlock the screen and wake the display")
		}
		t.Fatal("no Finder surface hit anywhere on screen")
	}
	if len(icons) == 0 {
		t.Skip("no desktop icons with a file URL found in the scanned region")
	}

	categories := map[DirStatus]int{}
	for _, s := range icons {
		obs := s.obs
		if obs.PID <= 0 || obs.ProcGen == 0 || obs.ElementHash == 0 {
			t.Fatal("desktop icon hit lacks process generation or element identity")
		}
		if obs.Role != "AXImage" {
			t.Fatalf("desktop icon role = %q, want AXImage", obs.Role)
		}
		// Structural container: the icon sits directly under the desktop scroll area, with no AXWindow ancestor.
		if obs.ParentChain != "AXImage>AXGroup>AXScrollArea>AXApplication" {
			t.Fatalf("desktop icon parent chain = %q", obs.ParentChain)
		}
		if obs.AXError != 0 {
			t.Fatalf("desktop icon hit reported AX error %d", obs.AXError)
		}
		if !isDesktopFolder(filepath.Dir(obs.FilePath)) {
			t.Fatalf("desktop icon URL does not point into a Desktop folder (URL kind %s)", obs.URLKind)
		}

		again := ax.HitTest(s.at, MonotonicNowNS()+uint64(50*time.Millisecond), InspectActive)
		if again.FinderContainer != ContainerDesktopIcon || again.FilePath != obs.FilePath || again.ProcGen != obs.ProcGen || again.ElementHash != obs.ElementHash {
			t.Fatal("repeated hit on the same desktop icon changed its identity")
		}
		if again.ParentChain != "" {
			t.Fatalf("InspectActive must not collect the parent chain, got %q", again.ParentChain)
		}
		if target := TargetFromObservation(again); target.Target.Kind != policy.TargetDesktopDirectory || target.Target.Path != again.FilePath || target.Err != nil {
			t.Fatalf("desktop icon did not resolve to a desktop directory target: %+v", target)
		}

		status, dev, ino := ClassifyDirectory(obs.FilePath)
		categories[status]++
		if status == DirEligible {
			if dev == 0 || ino == 0 {
				t.Fatal("eligible desktop directory lacks a file identity")
			}
			if err := RevalidateDirectory(obs.FilePath, dev, ino); err != nil {
				t.Fatalf("eligible desktop directory failed revalidation: %v", err)
			}
		}
	}
	summary := ""
	for status, count := range categories {
		summary += fmt.Sprintf(" %s=%d", status, count)
	}
	t.Logf("desktop icon classification:%s", summary)
}
