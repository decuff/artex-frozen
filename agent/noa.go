package agent

import (
	"log"
	"path/filepath"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/noaadapter"
)

// noaWarn returns a diagnostics sink tagging non-fatal noa messages with the
// session, routed through the package logger (agents have no per-instance one).
func noaWarn(session string) func(string) {
	return func(msg string) { log.Printf("[noa] %s: %s", session, msg) }
}

// noa is the 'model-driven context compression' mechanism introduced in norma v0.4.0, provided as a platform experimental feature that users toggle in
// system settings. It is mutually exclusive with the built-in compaction: noaadapter.Enable is the only entry point, mounting in one call
// the context taker-over (Compactor), the Compress tool, and the three-part resident prompt; not calling Enable means it is off
// (built-in compaction works as usual). The switch is resolved by the noaEnabledFn injected per agent, read once per run,
// so toggling only affects runs started afterward, with no need to rebuild the agent.

// enableNoa wires noa into opts when the resolver reports it enabled. archiveRoot is the persistence base directory for compressed originals
// (takes the global workDir; all agents uniformly land under <workDir>/noa, not scattered by task/intent directory), sessionID
// names the archive subdirectory beneath it (globally unique, so no conflicts within the same base directory).
//
// noa is an experimental feature: a wiring failure must not interrupt the real task. On error, report via onWarn and fall back to built-in compression.
// On successful enable, clear opts.Compaction to avoid agentcore warning about 'two context managers set at the same time'.
func enableNoa(opts *agentcore.Options, enabled func() bool, archiveRoot, sessionID string, onWarn func(string)) {
	if enabled == nil || !enabled() {
		return
	}
	if opts.OnWarn == nil {
		opts.OnWarn = onWarn
	}
	if err := noaadapter.Enable(opts, noaadapter.Options{
		ArchiveBaseDir: filepath.Join(archiveRoot, "noa"),
		SessionID:      sessionID,
		OnWarn:         onWarn,
	}); err != nil {
		if onWarn != nil {
			onWarn("noa compression enable failed, falling back to built-in compression:" + err.Error())
		}
		return
	}
	// Compactor overrides Compaction, but when both coexist agentcore warns every time; clear it explicitly.
	opts.Compaction = nil
}
