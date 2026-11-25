package relay

import (
	"sync"

	"github.com/XGrin/findrelay/internal/darwin"
	"github.com/XGrin/findrelay/internal/diagnostics"
	"github.com/XGrin/findrelay/internal/policy"
)

// Redacted reason codes passed across packages.
const (
	// ReasonTargetInvalidated means the target app failed revalidation at runtime and further redirects are turned off.
	ReasonTargetInvalidated = "target_app_invalidated"
	// ReasonDesktopPermissionDenied means the Desktop folder is unreadable and the desktop entry has been degraded.
	ReasonDesktopPermissionDenied = "desktop_folder_permission_denied"
)

// classifierQueueCap is the maximum number of first clicks waiting for classification; when the backlog exceeds it,
// new candidates are dropped and their gestures pass through on the native path.
const classifierQueueCap = 8

// DirectoryClassifier decides whether a path is a qualifying local plain directory and returns a revalidatable file
// identity (device number and inode).
type DirectoryClassifier func(path string) (darwin.DirStatus, uint64, uint64)

type classificationRequest struct {
	entry *directoryEntry
	path  string
}

// directoryEntry is the classification state of one first click; the fields after ready are written by the worker,
// and both reads and writes must hold Classifier.mu.
type directoryEntry struct {
	identity  string
	path      string
	expiresNS uint64

	ready    bool
	status   darwin.DirStatus
	snapshot policy.DirectorySnapshot
}

// Classifier classifies, in the background with a single bounded worker, the desktop directories hit by first clicks,
// and keeps short-lived snapshots.
// It implements policy.SnapshotSource: StartClassification, CancelClassification and LookupSnapshot are all
// non-blocking and can be called directly on the tap thread. The index holds only in-progress gestures: the state
// machine cancels the matching entry at the end of each gesture.
type Classifier struct {
	classify      DirectoryClassifier
	recorder      *diagnostics.Recorder
	onUnavailable func(reason string)

	queue     chan classificationRequest
	doneCh    chan struct{}
	closeOnce sync.Once

	mu                  sync.Mutex
	closed              bool
	entries             map[uint64]*directoryEntry
	unavailableReported bool
}

// NewClassifier creates and starts the background directory classifier.
// onUnavailable is called once, the first time Desktop access permission turns out to be insufficient, to report the
// degradation reason to the diagnostics layer.
func NewClassifier(classify DirectoryClassifier, recorder *diagnostics.Recorder, onUnavailable func(reason string)) *Classifier {
	c := &Classifier{
		classify:      classify,
		recorder:      recorder,
		onUnavailable: onUnavailable,
		queue:         make(chan classificationRequest, classifierQueueCap),
		doneCh:        make(chan struct{}),
		entries:       make(map[uint64]*directoryEntry),
	}
	go c.loop()
	return c
}

// StartClassification registers a snapshot entry for a first click and hands it to the background worker; when the
// classifier is closed or the worker is backlogged, no entry is kept.
func (c *Classifier) StartClassification(gestureID uint64, path, identity string, expiresNS uint64) {
	entry := &directoryEntry{identity: identity, path: path, expiresNS: expiresNS}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.entries[gestureID] = entry
	queued := c.enqueueLocked(classificationRequest{entry: entry, path: path})
	if !queued {
		delete(c.entries, gestureID)
	}
	c.mu.Unlock()

	if !queued {
		c.recorder.Warn("desktop", "classify_dropped", "classification_queue_full")
	}
}

// LookupSnapshot returns a snapshot that matches the target identity, has been classified as qualifying and is still
// valid; otherwise it returns false.
func (c *Classifier) LookupSnapshot(gestureID uint64, identity, path string, nowNS uint64) (policy.DirectorySnapshot, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, exists := c.entries[gestureID]
	if !exists {
		return policy.DirectorySnapshot{}, false
	}
	if nowNS >= entry.expiresNS {
		delete(c.entries, gestureID)
		return policy.DirectorySnapshot{}, false
	}
	if !entry.ready || entry.status != darwin.DirEligible || entry.identity != identity || entry.path != path {
		return policy.DirectorySnapshot{}, false
	}
	return entry.snapshot, true
}

// CancelClassification invalidates the snapshot and discards any classification result that arrives later.
func (c *Classifier) CancelClassification(gestureID uint64) {
	c.mu.Lock()
	delete(c.entries, gestureID)
	c.mu.Unlock()
}

// Close stops accepting new classifications and waits for the worker to exit.
func (c *Classifier) Close() {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		close(c.queue)
		c.entries = nil
		c.mu.Unlock()
		<-c.doneCh
	})
}

// enqueueLocked enqueues without blocking; the caller must hold c.mu, which rules out the race of sending on a closed
// channel.
func (c *Classifier) enqueueLocked(req classificationRequest) bool {
	select {
	case c.queue <- req:
		return true
	default:
		return false
	}
}

func (c *Classifier) loop() {
	defer close(c.doneCh)
	for req := range c.queue {
		c.classifyEntry(req)
	}
}

// classifyEntry runs one directory classification and writes the result into the entry; if the entry was already
// canceled, the result is written only to the entry already detached from the index and never revives it.
func (c *Classifier) classifyEntry(req classificationRequest) {
	status, dev, ino := c.classify(req.path)

	c.mu.Lock()
	req.entry.ready = true
	req.entry.status = status
	if status == darwin.DirEligible {
		req.entry.snapshot = policy.DirectorySnapshot{Path: req.path, Dev: dev, Ino: ino}
	}
	c.mu.Unlock()

	c.recordStatus(status)
}

// recordStatus records the redacted classification category; insufficient Desktop access permission is reported to
// the diagnostics layer only once.
func (c *Classifier) recordStatus(status darwin.DirStatus) {
	c.recorder.Debug("desktop", "classified", status.String())
	if status != darwin.DirErrPermission {
		return
	}
	c.mu.Lock()
	already := c.unavailableReported
	c.unavailableReported = true
	c.mu.Unlock()
	if already {
		return
	}
	c.recorder.Warn("desktop", "unavailable", ReasonDesktopPermissionDenied)
	if c.onUnavailable != nil {
		c.onUnavailable(ReasonDesktopPermissionDenied)
	}
}
