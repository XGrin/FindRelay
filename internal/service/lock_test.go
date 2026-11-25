package service

import (
	"errors"
	"os"
	"syscall"
	"testing"
)

func TestSingleInstanceLockSameAndDifferentSessions(t *testing.T) {
	runDir := t.TempDir()
	guiSessionA := func() (SessionInfo, error) {
		return SessionInfo{ID: 100015, HasGraphicAccess: true}, nil
	}
	guiSessionB := func() (SessionInfo, error) {
		return SessionInfo{ID: 100099, HasGraphicAccess: true}, nil
	}

	running, err := ProbeInstanceRunning(runDir, 100015)
	if err != nil || running {
		t.Fatalf("ProbeInstanceRunning before acquire = (%v, %v), want (false, nil)", running, err)
	}

	lock1, err := AcquireInstanceLock(runDir, guiSessionA)
	if err != nil {
		t.Fatalf("first AcquireInstanceLock error = %v", err)
	}
	defer lock1.Release()

	stBefore, err := os.Stat(InstanceLockPath(runDir, 100015))
	if err != nil {
		t.Fatalf("stat lock file: %v", err)
	}
	inodeBefore := stBefore.Sys().(*syscall.Stat_t).Ino

	running, err = ProbeInstanceRunning(runDir, 100015)
	if err != nil || !running {
		t.Fatalf("ProbeInstanceRunning while held = (%v, %v), want (true, nil)", running, err)
	}

	// A second instance in the same session must get a conflict error immediately.
	if _, err := AcquireInstanceLock(runDir, guiSessionA); !errors.Is(err, ErrInstanceAlreadyRunning) {
		t.Fatalf("second AcquireInstanceLock same session error = %v, want ErrInstanceAlreadyRunning", err)
	}

	// Different GUI sessions do not block each other.
	lockB, err := AcquireInstanceLock(runDir, guiSessionB)
	if err != nil {
		t.Fatalf("AcquireInstanceLock different session error = %v", err)
	}
	if err := lockB.Release(); err != nil {
		t.Fatalf("Release lockB: %v", err)
	}

	// After release the lock can be taken again, and the inode of the lock file is unchanged (the lock file was not
	// deleted).
	if err := lock1.Release(); err != nil {
		t.Fatalf("Release lock1: %v", err)
	}

	running, err = ProbeInstanceRunning(runDir, 100015)
	if err != nil || running {
		t.Fatalf("ProbeInstanceRunning after release = (%v, %v), want (false, nil)", running, err)
	}

	lock2, err := AcquireInstanceLock(runDir, guiSessionA)
	if err != nil {
		t.Fatalf("re-acquire after release error = %v", err)
	}
	defer lock2.Release()

	stAfter, err := os.Stat(InstanceLockPath(runDir, 100015))
	if err != nil {
		t.Fatalf("stat lock file after re-acquire: %v", err)
	}
	inodeAfter := stAfter.Sys().(*syscall.Stat_t).Ino
	if inodeBefore != inodeAfter {
		t.Fatalf("lock file inode changed (%d -> %d); lock file must not be unlinked", inodeBefore, inodeAfter)
	}
}

func TestInstanceLockRejectsNonGUISession(t *testing.T) {
	runDir := t.TempDir()

	cases := []struct {
		name string
		fn   SessionProvider
	}{
		{name: "provider error", fn: func() (SessionInfo, error) { return SessionInfo{}, errors.New("boom") }},
		{name: "zero session id", fn: func() (SessionInfo, error) { return SessionInfo{ID: 0, HasGraphicAccess: true}, nil }},
		{name: "headless ssh session", fn: func() (SessionInfo, error) { return SessionInfo{ID: 100020, HasGraphicAccess: false}, nil }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := AcquireInstanceLock(runDir, tc.fn)
			if !errors.Is(err, ErrNoGUISession) {
				t.Fatalf("AcquireInstanceLock(%s) error = %v, want ErrNoGUISession", tc.name, err)
			}
		})
	}
}
