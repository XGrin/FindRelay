package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

var (
	// ErrNoGUISession means the current process is not inside a valid user graphical security session.
	ErrNoGUISession = errors.New("current process does not belong to an active GUI security session")

	// ErrInstanceAlreadyRunning means an agent instance is already running in the same user GUI session.
	ErrInstanceAlreadyRunning = errors.New("another findrelay instance is already running in this GUI session")
)

// SessionInfo describes the macOS SecuritySession that the current process belongs to.
type SessionInfo struct {
	ID               uint32
	HasGraphicAccess bool
}

// SessionProvider queries the security session identity of the current process.
type SessionProvider func() (SessionInfo, error)

// guiSessionInfo queries the security session and confirms that the current process is in a valid user graphical
// security session.
func guiSessionInfo(sessionFn SessionProvider) (SessionInfo, error) {
	info, err := sessionFn()
	if err != nil {
		return SessionInfo{}, fmt.Errorf("%w: %v", ErrNoGUISession, err)
	}
	if info.ID == 0 || !info.HasGraphicAccess {
		return SessionInfo{}, fmt.Errorf("%w: session=%d graphic=%v", ErrNoGUISession, info.ID, info.HasGraphicAccess)
	}
	return info, nil
}

// openLockFile creates the run directory and the lock file, both accessible only by the owner.
func openLockFile(path string) (*os.File, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create run directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("chmod run directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("chmod lock file: %w", err)
	}
	return f, nil
}

// InstanceLock holds an exclusive file lock in the run directory: either the single-instance lock of the current GUI
// session or the operations lock that serializes service commands. The lock file is never deleted
// while running or releasing, so that different processes never lock different inodes.
type InstanceLock struct {
	file *os.File
	once sync.Once
}

// Lock probes (doctor, service status) hold the single-instance lock only momentarily, so a brief conflict during
// acquisition does not mean an instance is running: retry within the window, and report an instance conflict only if
// the lock is still held after the window.
const (
	instanceLockRetryWindow   = 100 * time.Millisecond
	instanceLockRetryInterval = 10 * time.Millisecond
)

// AcquireInstanceLock verifies the GUI session and tries to take the exclusive single-instance lock of that session
// without blocking.
func AcquireInstanceLock(runDir string, sessionFn SessionProvider) (*InstanceLock, error) {
	info, err := guiSessionInfo(sessionFn)
	if err != nil {
		return nil, err
	}
	f, err := openLockFile(InstanceLockPath(runDir, info.ID))
	if err != nil {
		return nil, err
	}

	deadline := time.Now().Add(instanceLockRetryWindow)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &InstanceLock{file: f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = f.Close()
			return nil, fmt.Errorf("flock instance lock: %w", err)
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, ErrInstanceAlreadyRunning
		}
		time.Sleep(instanceLockRetryInterval)
	}
}

// AcquireOperationsLock takes the exclusive lock that serializes service commands, blocking while another service
// command holds it.
func AcquireOperationsLock(runDir string) (*InstanceLock, error) {
	f, err := openLockFile(OperationsLockPath(runDir))
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("flock operations lock: %w", err)
	}
	return &InstanceLock{file: f}, nil
}

// Release releases the file lock and closes the file descriptor, keeping the lock file itself.
func (l *InstanceLock) Release() error {
	if l == nil {
		return nil
	}
	var releaseErr error
	l.once.Do(func() {
		if l.file != nil {
			errUnlock := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
			errClose := l.file.Close()
			if errUnlock != nil {
				releaseErr = errUnlock
			} else if errClose != nil {
				releaseErr = errClose
			}
			l.file = nil
		}
	})
	return releaseErr
}

// ProbeInstanceRunning checks non-destructively whether an instance currently holds the lock of the given GUI session
// (used by doctor and the service commands).
func ProbeInstanceRunning(runDir string, sessionID uint32) (bool, error) {
	if sessionID == 0 {
		return false, nil
	}
	lockPath := InstanceLockPath(runDir, sessionID)
	f, err := os.OpenFile(lockPath, os.O_RDWR, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer f.Close()

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return true, nil
		}
		return false, err
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false, nil
}

// InstanceLockPath returns the path of the single-instance lock file of the given GUI session; the file is never
// deleted while running or releasing, so that two processes never lock different inodes.
func InstanceLockPath(runDir string, sessionID uint32) string {
	return filepath.Join(runDir, fmt.Sprintf("agent-%d.lock", sessionID))
}

// OperationsLockPath returns the path of the operations lock file that serializes service commands; like the
// single-instance lock, it stays in the run directory and is never deleted.
func OperationsLockPath(runDir string) string {
	return filepath.Join(runDir, "service.lock")
}
