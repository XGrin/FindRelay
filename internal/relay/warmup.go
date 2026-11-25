package relay

import (
	"github.com/XGrin/findrelay/internal/darwin"
	"github.com/XGrin/findrelay/internal/diagnostics"
)

// DefaultWarmupBudgetNS is the total time limit for startup warm-up: 500 ms.
// Measured on this machine, warm-up completes in 43 to 107 ms; the limit exists only for when the Dock or Finder is
// unresponsive, and once the total time is used up the remaining steps are not run.
const DefaultWarmupBudgetNS uint64 = 500_000_000

// AXWarmer is a probe that can pay the AX cold-start cost before the event tap is created.
type AXWarmer interface {
	WarmUp(mode darwin.InspectMode, budgetNS uint64) darwin.AXWarmupReport
}

// WarmUpAX warms up the probe's AX channel before the event tap is created, taking no longer than
// DefaultWarmupBudgetNS in total.
// Probes that do not support warm-up are skipped; any failed step only logs one redacted line and does not affect
// startup.
func WarmUpAX(probe HitProbe, mode darwin.InspectMode, recorder *diagnostics.Recorder) {
	warmer, ok := probe.(AXWarmer)
	if !ok {
		return
	}
	report := warmer.WarmUp(mode, DefaultWarmupBudgetNS)
	if report.Complete() {
		recorder.Debug("ax", "warmup_complete", report.Describe())
		return
	}
	recorder.Warn("ax", "warmup_incomplete", report.Describe())
}
