package relay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/XGrin/findrelay/internal/darwin"
	"github.com/XGrin/findrelay/internal/diagnostics"
	"github.com/XGrin/findrelay/internal/policy"
)

const (
	desktopTestDir  = "/Users/test/Desktop/Projects"
	desktopTestID   = "desk:4321:55:6"
	desktopTestDev  = 0x1a
	desktopTestIno  = 0x2b
	finderBundleDir = "/System/Library/CoreServices/Finder.app"
)

var (
	targetAppCall = [2]string{desktopTestDir, "/Applications/Bloom.app"}
	finderDirCall = [2]string{desktopTestDir, finderBundleDir}
)

func desktopObservation() darwin.AXObservation {
	return darwin.AXObservation{
		PID:                  4321,
		ProcessKind:          darwin.ProcessFinder,
		FinderContainer:      darwin.ContainerDesktopIcon,
		Role:                 "AXImage",
		URLKind:              darwin.URLFile,
		FilePath:             desktopTestDir,
		ProcGen:              0x55,
		ElementHash:          6,
		HasSelectionInfo:     true,
		SelectionCount:       1,
		IsSingleSelectedSelf: true,
		ParentChain:          "AXImage>AXGroup>AXScrollArea>AXApplication",
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// syncBuffer lets tests safely read the log while the Recorder's background writer thread is working.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) count(sub string) int { return strings.Count(b.String(), sub) }

// gate lets the test control precisely when the classification worker continues; open can be called repeatedly,
// and when a test fails and returns early it releases the worker before Close, so neither side waits on the other.
type gate struct {
	once sync.Once
	ch   chan struct{}
}

func newGate() *gate { return &gate{ch: make(chan struct{})} }

func (g *gate) wait() { <-g.ch }

func (g *gate) open() { g.once.Do(func() { close(g.ch) }) }

// waitClassified waits for the entry of the given gesture to finish classification; a non-qualifying directory never
// becomes a snapshot, so the entry state is checked directly.
func waitClassified(t *testing.T, c *Classifier, gid uint64) {
	t.Helper()
	waitFor(t, fmt.Sprintf("classification of gesture %d", gid), func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.entries[gid] != nil && c.entries[gid].ready
	})
}

func TestClassifierClassifiesAsynchronouslyAndExpires(t *testing.T) {
	rec := diagnostics.NewRecorder(nil, diagnostics.LogWarn, 64)
	defer rec.Close()

	release := newGate()
	nowNS := uint64(1_000_000_000)
	c := NewClassifier(func(string) (darwin.DirStatus, uint64, uint64) {
		release.wait()
		return darwin.DirEligible, desktopTestDev, desktopTestIno
	}, rec, nil)
	defer c.Close()
	defer release.open()

	expiresNS := nowNS + 1_000_000_000
	c.StartClassification(1, desktopTestDir, desktopTestID, expiresNS)
	if _, ok := c.LookupSnapshot(1, desktopTestID, desktopTestDir, nowNS); ok {
		t.Fatal("snapshot must not be usable before the worker finishes")
	}

	release.open()
	waitClassified(t, c, 1)

	snap, ok := c.LookupSnapshot(1, desktopTestID, desktopTestDir, nowNS)
	if !ok || snap != (policy.DirectorySnapshot{Path: desktopTestDir, Dev: desktopTestDev, Ino: desktopTestIno}) {
		t.Fatalf("snapshot = %+v, %v", snap, ok)
	}

	// A query with mismatched identity or path gets no snapshot and does not delete the entry by mistake.
	for _, probe := range [][2]string{{"desk:4321:55:7", desktopTestDir}, {desktopTestID, "/Users/test/Desktop/Other"}} {
		if _, ok := c.LookupSnapshot(1, probe[0], probe[1], nowNS); ok {
			t.Fatalf("lookup with mismatched %v must fail", probe)
		}
	}
	if _, ok := c.LookupSnapshot(1, desktopTestID, desktopTestDir, nowNS); !ok {
		t.Fatal("mismatched lookups must not evict the entry")
	}

	// After the validity period, the snapshot expires and is removed from the index.
	if _, ok := c.LookupSnapshot(1, desktopTestID, desktopTestDir, expiresNS); ok {
		t.Fatal("expired snapshot must not be returned")
	}
	c.mu.Lock()
	left := len(c.entries)
	c.mu.Unlock()
	if left != 0 {
		t.Fatalf("expired entries left in index: %d", left)
	}
}

func TestClassifierCancelDiscardsLateResult(t *testing.T) {
	rec := diagnostics.NewRecorder(nil, diagnostics.LogWarn, 64)
	defer rec.Close()

	release := newGate()
	c := NewClassifier(func(path string) (darwin.DirStatus, uint64, uint64) {
		if path == desktopTestDir {
			release.wait()
		}
		return darwin.DirEligible, desktopTestDev, desktopTestIno
	}, rec, nil)
	defer c.Close()
	defer release.open()

	c.StartClassification(1, desktopTestDir, desktopTestID, 2_000_000_000)
	c.CancelClassification(1)
	release.open()

	// The worker processes serially: once the second entry finishes classification, the late result of the first entry
	// has been written to the object already detached from the index.
	const otherDir = "/Users/test/Desktop/Other"
	c.StartClassification(2, otherDir, "desk:4321:55:7", 2_000_000_000)
	waitClassified(t, c, 2)
	if _, ok := c.LookupSnapshot(1, desktopTestID, desktopTestDir, 1_000_000_000); ok {
		t.Fatal("cancelled classification must not revive its snapshot")
	}
}

func TestClassifierRejectsIneligibleAndReportsPermissionOnce(t *testing.T) {
	var logs syncBuffer
	rec := diagnostics.NewRecorder(&logs, diagnostics.LogDebug, 128)
	defer rec.Close()

	var mu sync.Mutex
	var unavailable []string
	c := NewClassifier(func(path string) (darwin.DirStatus, uint64, uint64) {
		switch path {
		case "/Users/test/Desktop/Notes.app":
			return darwin.DirRejectPackage, 0, 0
		case "/Users/test/Desktop/Denied":
			return darwin.DirErrPermission, 0, 0
		default:
			return darwin.DirEligible, desktopTestDev, desktopTestIno
		}
	}, rec, func(reason string) {
		mu.Lock()
		unavailable = append(unavailable, reason)
		mu.Unlock()
	})
	defer c.Close()

	requests := []struct {
		id   uint64
		path string
	}{
		{1, "/Users/test/Desktop/Notes.app"},
		{2, "/Users/test/Desktop/Denied"},
		{3, "/Users/test/Desktop/Denied"},
		{4, desktopTestDir},
	}
	for _, req := range requests {
		c.StartClassification(req.id, req.path, fmt.Sprintf("desk:4321:55:%d", req.id), 2_000_000_000)
	}
	waitFor(t, "all classifications to be logged", func() bool { return logs.count("classified") == len(requests) })
	waitFor(t, "permission report", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(unavailable) == 1
	})

	for _, req := range requests[:3] {
		identity := fmt.Sprintf("desk:4321:55:%d", req.id)
		if _, ok := c.LookupSnapshot(req.id, identity, req.path, 1_000_000_000); ok {
			t.Fatalf("%s must not produce a usable snapshot", req.path)
		}
	}
	mu.Lock()
	gotUnavailable := append([]string(nil), unavailable...)
	mu.Unlock()
	if len(gotUnavailable) != 1 || gotUnavailable[0] != "desktop_folder_permission_denied" {
		t.Fatalf("unavailable reports = %v, want one desktop_folder_permission_denied", gotUnavailable)
	}

	waitFor(t, "permission warning", func() bool { return logs.count("desktop_folder_permission_denied") == 1 })
	time.Sleep(20 * time.Millisecond)
	if got := logs.count("desktop_folder_permission_denied"); got != 1 {
		t.Fatalf("permission warning logged %d times, want exactly once", got)
	}
	for _, secret := range []string{"/Users/test", "Notes.app", "Denied"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("logs leaked %q:\n%s", secret, logs.String())
		}
	}
}

func TestClassifierQueueFullDropsNewCandidate(t *testing.T) {
	var logs syncBuffer
	rec := diagnostics.NewRecorder(&logs, diagnostics.LogWarn, 64)
	defer rec.Close()

	release := newGate()
	c := NewClassifier(func(string) (darwin.DirStatus, uint64, uint64) {
		release.wait()
		return darwin.DirEligible, desktopTestDev, desktopTestIno
	}, rec, nil)
	defer c.Close()
	defer release.open()

	identity := func(id uint64) string { return fmt.Sprintf("desk:4321:55:%d", id) }
	// While the worker is stuck it accepts at most "queue capacity + 1 in hand" entries; any extra must be dropped and
	// leave a redacted warning.
	const total = classifierQueueCap * 2
	for id := uint64(1); id <= total; id++ {
		c.StartClassification(id, desktopTestDir, identity(id), 2_000_000_000)
	}
	// The index holds "queue capacity + 1 in the worker's hand" entries; dropped entries never enter the index.
	c.mu.Lock()
	acceptedWhileBlocked := len(c.entries)
	c.mu.Unlock()
	if acceptedWhileBlocked < classifierQueueCap || acceptedWhileBlocked > classifierQueueCap+1 {
		t.Fatalf("index holds %d candidates while the worker is blocked, want %d or %d", acceptedWhileBlocked, classifierQueueCap, classifierQueueCap+1)
	}

	release.open()
	waitFor(t, "accepted candidates classified", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, entry := range c.entries {
			if !entry.ready {
				return false
			}
		}
		return len(c.entries) >= classifierQueueCap
	})
	// The worker may take a request between two registrations, so the accepted entries are not necessarily the
	// lowest-numbered prefix; assert on the entries actually in the index.
	c.mu.Lock()
	acceptedIDs := make(map[uint64]bool, len(c.entries))
	for id := range c.entries {
		acceptedIDs[id] = true
	}
	c.mu.Unlock()
	accepted := len(acceptedIDs)
	if accepted > classifierQueueCap+1 {
		t.Fatalf("accepted %d candidates, want at most %d", accepted, classifierQueueCap+1)
	}
	waitFor(t, "queue-full warnings", func() bool {
		return logs.count("classification_queue_full") == total-accepted
	})
	for id := uint64(1); id <= total; id++ {
		_, ok := c.LookupSnapshot(id, identity(id), desktopTestDir, 1_000_000_000)
		if ok != acceptedIDs[id] {
			t.Fatalf("candidate %d eligible=%v, want %v", id, ok, acceptedIDs[id])
		}
	}
}

func TestClassifierConcurrentStartCancelLookup(t *testing.T) {
	rec := diagnostics.NewRecorder(nil, diagnostics.LogWarn, 64)
	defer rec.Close()
	c := NewClassifier(func(string) (darwin.DirStatus, uint64, uint64) {
		return darwin.DirEligible, desktopTestDev, desktopTestIno
	}, rec, nil)
	defer c.Close()

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(base uint64) {
			defer wg.Done()
			for i := uint64(1); i <= 50; i++ {
				id := base*100 + i
				c.StartClassification(id, desktopTestDir, desktopTestID, ^uint64(0)>>1)
				if i%3 == 0 {
					c.CancelClassification(id)
				}
				c.LookupSnapshot(id, desktopTestID, desktopTestDir, 1)
			}
		}(uint64(g) + 1)
	}
	wg.Wait()
}

// directoryExec builds an executor with an injectable directory opener and revalidator.
type directoryExec struct {
	exec     *Executor
	rec      *diagnostics.Recorder
	logs     syncBuffer
	mu       sync.Mutex
	calls    [][2]string
	rechecks int
}

func newDirectoryExec(t *testing.T, openErr func(dirPath, appPath string) error, revalidate func(call int) error) *directoryExec {
	t.Helper()
	de := &directoryExec{}
	de.rec = diagnostics.NewRecorder(&de.logs, diagnostics.LogWarn, 64)
	de.exec = NewExecutor(ExecutorConfig{
		TargetAppPath:     "/Applications/Bloom.app",
		FinderAppPath:     finderBundleDir,
		Capacity:          8,
		CompletionTimeout: 200 * time.Millisecond,
		Recorder:          de.rec,
		DirOpener: func(dirPath, appPath string, onComplete func(error)) {
			de.mu.Lock()
			de.calls = append(de.calls, [2]string{dirPath, appPath})
			de.mu.Unlock()
			onComplete(openErr(dirPath, appPath))
		},
		RevalidateDirectory: func(dirPath string, dev, ino uint64) error {
			de.mu.Lock()
			de.rechecks++
			call := de.rechecks
			de.mu.Unlock()
			if dirPath != desktopTestDir || dev != desktopTestDev || ino != desktopTestIno {
				return errors.New("unexpected revalidation arguments")
			}
			return revalidate(call)
		},
	})
	t.Cleanup(func() { de.rec.Close() })
	return de
}

// submitDirectory submits an open-directory action and waits for the executor to finish processing it.
func (de *directoryExec) submitDirectory(t *testing.T, gid uint64) {
	t.Helper()
	if !de.exec.TryReserve(gid) {
		t.Fatalf("TryReserve(%d) failed", gid)
	}
	if !de.exec.Submit(policy.Action{GestureID: gid, Kind: policy.ActionOpenDirectory, Path: desktopTestDir, Dev: desktopTestDev, Ino: desktopTestIno}) {
		t.Fatalf("Submit(%d) failed", gid)
	}
	de.exec.Close()
}

func (de *directoryExec) assertCalls(t *testing.T, want ...[2]string) {
	t.Helper()
	de.mu.Lock()
	got := append([][2]string(nil), de.calls...)
	rechecks := de.rechecks
	de.mu.Unlock()
	if len(got) != len(want) {
		t.Fatalf("directory open calls = %v, want %v (revalidations=%d)", got, want, rechecks)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("directory open calls = %v, want %v", got, want)
		}
	}
	if de.exec.InUseCount() != 0 {
		t.Fatalf("in-use slots = %d, want 0", de.exec.InUseCount())
	}
}

func (de *directoryExec) assertMetrics(t *testing.T, submitted, succeeded, compensated uint64) {
	t.Helper()
	snap := de.rec.Snapshot()
	if snap.ActionsSubmitted != submitted || snap.ActionsSucceeded != succeeded || snap.ActionsCompensated != compensated {
		t.Fatalf("unexpected metrics: %+v", snap)
	}
}

func TestDirectoryOpenSucceedsWithoutCompensation(t *testing.T) {
	de := newDirectoryExec(t, func(string, string) error { return nil }, func(int) error { return nil })
	de.submitDirectory(t, 1)
	de.assertCalls(t, targetAppCall)
	de.assertMetrics(t, 1, 1, 0)
}

func TestDirectoryOpenFailureTriggersSingleFinderCompensation(t *testing.T) {
	de := newDirectoryExec(t, func(_, appPath string) error {
		if appPath == "/Applications/Bloom.app" {
			return errors.New("launch failed")
		}
		return nil
	}, func(int) error { return nil })
	de.submitDirectory(t, 1)
	de.assertCalls(t, targetAppCall, finderDirCall)
	de.assertMetrics(t, 1, 0, 1)
}

func TestDirectoryIdentityChangedDropsRequestWithoutCompensation(t *testing.T) {
	de := newDirectoryExec(t, func(string, string) error { return errors.New("unexpected open") }, nil)
	de.exec.revalidateDirectory = func(string, uint64, uint64) error { return darwin.ErrDirectoryIdentityChanged }

	de.submitDirectory(t, 1)
	de.assertCalls(t)
	de.assertMetrics(t, 1, 0, 0)
	de.rec.Close()
	if !strings.Contains(de.logs.String(), "directory_identity_changed") {
		t.Fatalf("missing identity-changed warning:\n%s", de.logs.String())
	}
}

func TestDirectoryIdentityChangedAfterFailureSkipsCompensation(t *testing.T) {
	de := newDirectoryExec(t, func(string, string) error { return errors.New("launch failed") }, func(call int) error {
		if call == 1 {
			return nil
		}
		return darwin.ErrDirectoryIdentityChanged
	})
	de.submitDirectory(t, 1)
	de.assertCalls(t, targetAppCall)
	de.assertMetrics(t, 1, 0, 0)
	de.rec.Close()
	if !strings.Contains(de.logs.String(), "compensation_skipped_directory_invalid") {
		t.Fatalf("missing skipped-compensation warning:\n%s", de.logs.String())
	}
}

func TestDirectoryTargetAppInvalidatedCompensatesToFinder(t *testing.T) {
	de := newDirectoryExec(t, func(string, string) error { return nil }, func(int) error { return nil })
	var reason string
	de.exec.onTargetInvalid = func(r string) { reason = r }
	de.exec.revalidateTarget = func(string) error { return errors.New("bundle gone") }

	de.submitDirectory(t, 1)
	de.assertCalls(t, finderDirCall)
	if !de.exec.Disabled() || reason != "target_app_invalidated" {
		t.Fatalf("executor must disable after target invalidation: disabled=%v reason=%q", de.exec.Disabled(), reason)
	}
}

// When a custom open command fails, the redacted reason is recorded by category, the error text (which may contain
// paths) stays out of the log, and one compensation to Finder is made as usual.
func TestDirectoryOpenCommandFailureLogsCategoryAndCompensates(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"run failed", fmt.Errorf("%w: %w", darwin.ErrOpenCommandFailed, errors.New("exit status 1")), "open_command_failed"},
		{"timed out", fmt.Errorf("%w: %w", darwin.ErrOpenCommandFailed, context.DeadlineExceeded), "open_command_timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			de := newDirectoryExec(t, func(_, appPath string) error {
				if appPath == "/Applications/Bloom.app" {
					return fmt.Errorf("%s: %w", "/private/secret/path", tc.err)
				}
				return nil
			}, func(int) error { return nil })
			de.submitDirectory(t, 1)
			de.assertCalls(t, targetAppCall, finderDirCall)
			de.assertMetrics(t, 1, 0, 1)
			de.rec.Close()
			logs := de.logs.String()
			if !strings.Contains(logs, tc.want) {
				t.Fatalf("logs must contain %q:\n%s", tc.want, logs)
			}
			for _, leaked := range []string{"/private/secret", "exit status"} {
				if strings.Contains(logs, leaked) {
					t.Fatalf("logs leaked %q:\n%s", leaked, logs)
				}
			}
		})
	}
}

// Directory version of the completion timeout case: an open completion timeout is an unknown result, so there is no compensation and the
// slot is kept until the late callback reclaims it.
func TestDirectoryOpenTimeoutKeepsSlotUntilLateCallback(t *testing.T) {
	const (
		timeout   = 40 * time.Millisecond
		targetApp = "/Applications/Bloom.app"
	)
	laterError := errors.New("late failure")

	rec := diagnostics.NewRecorder(nil, diagnostics.LogWarn, 64)
	defer rec.Close()

	var (
		mu        sync.Mutex
		calls     []string
		callbacks = map[string]func(error){}
	)
	exec := NewExecutor(ExecutorConfig{
		TargetAppPath:     targetApp,
		FinderAppPath:     finderBundleDir,
		Capacity:          8,
		CompletionTimeout: timeout,
		Recorder:          rec,
		DirOpener: func(_, appPath string, onComplete func(error)) {
			mu.Lock()
			calls = append(calls, appPath)
			callbacks[appPath] = onComplete
			mu.Unlock()
		},
		RevalidateDirectory: func(string, uint64, uint64) error { return nil },
	})
	defer exec.Close()

	if !exec.TryReserve(9) {
		t.Fatalf("TryReserve(9) failed")
	}
	if !exec.Submit(policy.Action{GestureID: 9, Kind: policy.ActionOpenDirectory, Path: desktopTestDir, Dev: desktopTestDev, Ino: desktopTestIno}) {
		t.Fatalf("Submit(9) failed")
	}

	waitFor(t, "target app dispatch", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(callbacks) == 1
	})
	time.Sleep(2 * timeout)
	if exec.InUseCount() != 1 {
		t.Fatalf("InUseCount after timeout = %d, want 1 until the late callback", exec.InUseCount())
	}
	snap := rec.Snapshot()
	if snap.ActionsTimedOut != 1 || snap.ActionsCompensated != 0 {
		t.Fatalf("timeout is an unknown outcome and must not compensate: %+v", snap)
	}

	// A late failure callback only reclaims the slot and must not compensate again.
	mu.Lock()
	targetCB := callbacks[targetApp]
	mu.Unlock()
	targetCB(laterError)
	exec.Close()

	mu.Lock()
	gotCalls := append([]string(nil), calls...)
	mu.Unlock()
	if len(gotCalls) != 1 || gotCalls[0] != targetApp {
		t.Fatalf("directory open calls = %v, want only the target app", gotCalls)
	}
	if exec.InUseCount() != 0 {
		t.Fatalf("InUseCount after late callback = %d, want 0", exec.InUseCount())
	}
	if snap := rec.Snapshot(); snap.ActionsCompensated != 0 {
		t.Fatalf("late failure must not trigger compensation: %+v", snap)
	}
}

// modeProbe records the inspect mode used by each probe call and returns preset observations.
type modeProbe struct {
	obs      darwin.AXObservation
	mu       sync.Mutex
	modes    []darwin.InspectMode
	rechecks []bool
}

func (p *modeProbe) HitTest(_ policy.Point, _ uint64, mode darwin.InspectMode) darwin.AXObservation {
	p.mu.Lock()
	p.modes = append(p.modes, mode)
	p.mu.Unlock()
	return p.obs
}

// rechecks records whether each recheck asked for single-selection evidence.
func (p *modeProbe) Recheck(_ policy.Point, _ uint64, needSelection bool) darwin.AXObservation {
	p.mu.Lock()
	p.rechecks = append(p.rechecks, needSelection)
	p.mu.Unlock()
	return p.obs
}

func (p *modeProbe) EndGesture() {}

func (p *modeProbe) setObservation(obs darwin.AXObservation) {
	p.mu.Lock()
	p.obs = obs
	p.mu.Unlock()
}

func (p *modeProbe) lastMode() darwin.InspectMode {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.modes) == 0 {
		return darwin.InspectDock
	}
	return p.modes[len(p.modes)-1]
}

type desktopHarness struct {
	logs      syncBuffer
	rec       *diagnostics.Recorder
	probe     *modeProbe
	classify  *Classifier
	exec      *Executor
	mu        sync.Mutex
	opened    [][2]string
	nowNS     uint64
	closeOnce sync.Once
}

// newDesktopHarness assembles a controller with both the Dock and desktop entries enabled; event timestamps use the
// real monotonic clock, consistent with the classifier's expiry check.
func newDesktopHarness(t *testing.T, mode policy.InterceptMode, status darwin.DirStatus) (*desktopHarness, *Controller) {
	t.Helper()
	h := &desktopHarness{probe: &modeProbe{obs: desktopObservation()}, nowNS: darwin.MonotonicNowNS()}
	h.rec = diagnostics.NewRecorder(&h.logs, diagnostics.LogDebug, 256)
	h.classify = NewClassifier(func(string) (darwin.DirStatus, uint64, uint64) {
		return status, desktopTestDev, desktopTestIno
	}, h.rec, nil)

	record := func(dirPath, appPath string) {
		h.mu.Lock()
		h.opened = append(h.opened, [2]string{dirPath, appPath})
		h.mu.Unlock()
	}
	h.exec = NewExecutor(ExecutorConfig{
		TargetAppPath:     "/Applications/Bloom.app",
		FinderAppPath:     finderBundleDir,
		Capacity:          8,
		CompletionTimeout: 200 * time.Millisecond,
		Recorder:          h.rec,
		Opener: func(appPath string, onComplete func(error)) {
			record("(app)", appPath)
			onComplete(nil)
		},
		DirOpener: func(dirPath, appPath string, onComplete func(error)) {
			record(dirPath, appPath)
			onComplete(nil)
		},
		RevalidateDirectory: func(string, uint64, uint64) error { return nil },
	})

	ctrl := NewController(ControllerConfig{
		DockEnabled:           true,
		DockInterceptMode:     policy.InterceptOnUp,
		DesktopEnabled:        true,
		DesktopInterceptMode:  mode,
		DoubleClickIntervalNS: uint64(500 * time.Millisecond),
		Snapshots:             h.classify,
		Probe:                 h.probe,
		Executor:              h.exec,
		Recorder:              h.rec,
		NowNS:                 h.next,
		IsTrusted:             func() bool { return true },
	})
	t.Cleanup(h.close)
	return h, ctrl
}

// close waits until the executor has processed the queued actions, the classifier has exited and the log is drained;
// only then can results be read.
func (h *desktopHarness) close() {
	h.closeOnce.Do(func() {
		h.exec.Close()
		h.classify.Close()
		h.rec.Close()
	})
}

func (h *desktopHarness) next() uint64 {
	h.nowNS += 50_000_000
	return h.nowNS
}

func (h *desktopHarness) click(ctrl *Controller, kind policy.EventKind, count int) darwin.TapDisposition {
	return ctrl.OnMouseEvent(policy.Event{
		Kind:       kind,
		Point:      policy.Point{X: 500, Y: 1040},
		ClickCount: count,
		TimeNS:     h.next(),
	})
}

// doubleClick completes one double-click on a desktop icon and returns the dispositions of the second click's down
// and up; the second click is sent only after the first click's classification finishes.
func (h *desktopHarness) doubleClick(t *testing.T, ctrl *Controller) (darwin.TapDisposition, darwin.TapDisposition) {
	t.Helper()
	h.click(ctrl, policy.EventDown, 1)
	h.click(ctrl, policy.EventUp, 1)
	waitClassified(t, h.classify, 1)
	second := h.click(ctrl, policy.EventDown, 2)
	last := h.click(ctrl, policy.EventUp, 2)
	return second, last
}

func (h *desktopHarness) assertOpened(t *testing.T, want ...[2]string) {
	t.Helper()
	h.mu.Lock()
	got := append([][2]string(nil), h.opened...)
	h.mu.Unlock()
	if len(got) != len(want) {
		t.Fatalf("opened = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("opened = %v, want %v", got, want)
		}
	}
}

func (h *desktopHarness) assertLogsSanitized(t *testing.T) {
	t.Helper()
	for _, secret := range []string{"/Users/test", "Projects", "1040", "desk:4321", "Bloom.app"} {
		if strings.Contains(h.logs.String(), secret) {
			t.Fatalf("logs leaked %q:\n%s", secret, h.logs.String())
		}
	}
}

func TestControllerDesktopDoubleClickRedirectsOnce(t *testing.T) {
	for _, mode := range []policy.InterceptMode{policy.InterceptOnUp, policy.InterceptOnDown} {
		t.Run(mode.String(), func(t *testing.T) {
			h, ctrl := newDesktopHarness(t, mode, darwin.DirEligible)

			second, last := h.doubleClick(t, ctrl)
			wantSecond, wantLast := darwin.TapPass, darwin.TapCancelDesktopUp
			if mode == policy.InterceptOnDown {
				wantSecond, wantLast = darwin.TapSwallow, darwin.TapSwallow
			}
			if second != wantSecond || last != wantLast {
				t.Fatalf("second click dispositions = %v, %v; want %v, %v", second, last, wantSecond, wantLast)
			}

			// The third and later clicks of a multi-click pass through unchanged.
			if got := h.click(ctrl, policy.EventDown, 3); got != darwin.TapPass {
				t.Fatalf("third click disposition = %v, want TapPass", got)
			}
			if h.probe.lastMode() != darwin.InspectActive {
				t.Fatalf("probe mode = %v, want InspectActive with desktop redirection enabled", h.probe.lastMode())
			}

			h.close()
			h.assertOpened(t, targetAppCall)

			logs := h.logs.String()
			wantDecision := "cancel_desktop_up+open_directory"
			if mode == policy.InterceptOnDown {
				wantDecision = "consume+open_directory"
			}
			for _, want := range []string{wantDecision, "container=desktop_icon", "sel=single_self", "directory_sent_to_target_app"} {
				if !strings.Contains(logs, want) {
					t.Fatalf("logs must contain %q:\n%s", want, logs)
				}
			}
			h.assertLogsSanitized(t)
		})
	}
}

func TestControllerDesktopIneligibleOrUnreadableDirectoryFailsOpen(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status darwin.DirStatus
	}{
		{"package", darwin.DirRejectPackage},
		{"permission denied", darwin.DirErrPermission},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, ctrl := newDesktopHarness(t, policy.InterceptOnUp, tc.status)
			second, last := h.doubleClick(t, ctrl)
			if second != darwin.TapPass || last != darwin.TapPass {
				t.Fatalf("ineligible directory must reach Finder natively, got %v, %v", second, last)
			}
			h.close()
			h.assertOpened(t)
			h.assertLogsSanitized(t)
		})
	}
}

// When the Desktop folder is unreadable, desktop gestures pass through and the Dock entry is unaffected.
func TestControllerDesktopPermissionDeniedLeavesDockWorking(t *testing.T) {
	h, ctrl := newDesktopHarness(t, policy.InterceptOnUp, darwin.DirErrPermission)

	second, last := h.doubleClick(t, ctrl)
	if second != darwin.TapPass || last != darwin.TapPass {
		t.Fatalf("unreadable desktop folder must fail open, got %v, %v", second, last)
	}

	h.probe.setObservation(darwin.AXObservation{
		PID:          777,
		ProcessKind:  darwin.ProcessDock,
		IsDockFinder: true,
		Role:         "AXDockItem",
		Subrole:      "AXApplicationDockItem",
		URLKind:      darwin.URLFinderApp,
	})
	h.click(ctrl, policy.EventDown, 1)
	if got := h.click(ctrl, policy.EventUp, 1); got != darwin.TapCancelUp {
		t.Fatalf("Dock click disposition = %v, want TapCancelUp", got)
	}

	h.close()
	h.assertOpened(t, [2]string{"(app)", "/Applications/Bloom.app"})
	if !strings.Contains(h.logs.String(), "desktop_folder_permission_denied") {
		t.Fatalf("missing permission warning:\n%s", h.logs.String())
	}
}

func TestControllerDesktopDisabledKeepsFinderClicksNative(t *testing.T) {
	var logs syncBuffer
	probe := &modeProbe{obs: desktopObservation()}
	rec := diagnostics.NewRecorder(&logs, diagnostics.LogWarn, 64)
	defer rec.Close()
	nowNS := uint64(1_000_000_000)
	next := func() uint64 {
		nowNS += 50_000_000
		return nowNS
	}
	var opened int
	exec := NewExecutor(ExecutorConfig{
		TargetAppPath: "/Applications/Bloom.app",
		FinderAppPath: finderBundleDir,
		Capacity:      8,
		Recorder:      rec,
		DirOpener: func(_, _ string, onComplete func(error)) {
			opened++
			onComplete(nil)
		},
		RevalidateDirectory: func(string, uint64, uint64) error { return nil },
	})
	defer exec.Close()

	ctrl := NewController(ControllerConfig{
		DockEnabled:       true,
		DockInterceptMode: policy.InterceptOnUp,
		Probe:             probe,
		Executor:          exec,
		Recorder:          rec,
		NowNS:             next,
		IsTrusted:         func() bool { return true },
	})
	for _, ev := range []struct {
		kind  policy.EventKind
		count int
	}{
		{policy.EventDown, 1}, {policy.EventUp, 1}, {policy.EventDown, 2}, {policy.EventUp, 2},
	} {
		if got := ctrl.OnMouseEvent(policy.Event{Kind: ev.kind, Point: policy.Point{X: 500, Y: 1040}, ClickCount: ev.count, TimeNS: next()}); got != darwin.TapPass {
			t.Fatalf("desktop clicks must stay native when disabled, got %v for %v", got, ev.kind)
		}
	}
	if probe.lastMode() != darwin.InspectDock {
		t.Fatalf("probe mode = %v, want InspectDock while desktop redirection is disabled", probe.lastMode())
	}
	exec.Close()
	rec.Close()
	if opened != 0 || strings.Contains(logs.String(), "desktop_icon") {
		t.Fatalf("disabled entry must not act or classify: opened=%d logs:\n%s", opened, logs.String())
	}
}

// Observe mode distinguishes native desktop icons, the empty desktop and ordinary Finder windows, and it never
// consumes events or records paths.
func TestControllerObserveModeClassifiesDesktopAndFinderWindows(t *testing.T) {
	var logs syncBuffer
	probe := &modeProbe{obs: desktopObservation()}
	rec := diagnostics.NewRecorder(&logs, diagnostics.LogDebug, 64)
	defer rec.Close()
	nowNS := uint64(1_000_000_000)
	next := func() uint64 {
		nowNS += 50_000_000
		return nowNS
	}
	ctrl := NewController(ControllerConfig{
		ObserveOnly:    true,
		DesktopEnabled: true,
		DockEnabled:    true,
		Probe:          probe,
		Recorder:       rec,
		NowNS:          next,
		IsTrusted:      func() bool { return true },
		OnFatalStop:    func(string) {},
	})

	sample := func(kind policy.EventKind, count int) darwin.TapDisposition {
		return ctrl.OnMouseEvent(policy.Event{Kind: kind, Point: policy.Point{X: 500, Y: 1040}, ClickCount: count, TimeNS: next()})
	}

	if got := sample(policy.EventDown, 1); got != darwin.TapPass {
		t.Fatalf("observe mode consumed a desktop click: %v", got)
	}
	if probe.lastMode() != darwin.InspectObserve {
		t.Fatalf("probe mode = %v, want InspectObserve", probe.lastMode())
	}

	background := desktopObservation()
	background.FinderContainer = darwin.ContainerDesktopBackground
	background.FilePath = ""
	background.URLKind = darwin.URLNone
	probe.setObservation(background)
	sample(policy.EventUp, 1)

	window := desktopObservation()
	window.FinderContainer = darwin.ContainerFinderWindow
	probe.setObservation(window)
	sample(policy.EventDown, 1)

	rec.Close()
	for _, want := range []string{"container=desktop_icon", "container=desktop_background", "container=finder_window", "sel=single_self"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("observe logs must contain %q:\n%s", want, logs.String())
		}
	}
	for _, secret := range []string{"/Users/test", "Projects", "desk:4321", "1040"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("observe logs leaked %q:\n%s", secret, logs.String())
		}
	}
}
