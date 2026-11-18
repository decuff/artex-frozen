// Package selfupdate implements one-click page update for ARTEX: pull the new version from GitHub Release
// binary, verify, stage, and atomically swap in on next startup.
//
// Overall division of labor (see start.sh / start.bat):
//
//	startup script = foolproof watchdog loop, only responsible for "after process exits, decide by exit code whether to relaunch"
//	this package    = all error-prone logic (download / SHA256 verify / smoke test / swap / rollback on failure)
//
// The reason the swap is in Go rather than in the script is that sha256 verification and smoke testing on sh and bat
// would need two implementations (sha256sum / shasum / certutil), and this is precisely the step that must not go wrong—swap in a
// binary that won't run, and the watchdog will faithfully keep relaunching it, leaving the user to rescue the machine by hand.
//
// A complete upgrade goes through three process startups:
//
//	① old server receives /api/update/apply → download & verify → stage artex.new → exit 75
//	② script relaunches old version → Bootstrap finds artex.new → verify + smoke test → swap → exit 75
//	③ script relaunches, now the new version → Bootstrap records one attempt → clears marker after successful startup
//
// Any step failing falls back to the old version: ② if verification fails, delete the staged file and keep running the old version; ③ if 3 consecutive
// failures to clear the marker (crashes on startup), automatically swap artex.old back.
package selfupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ExitRestart is the exit code for "please have the watchdog relaunch me" (EX_TEMPFAIL). The startup script sees it
// and reruns immediately, not counting toward crash backoff. 0 means the user stopped normally (script exits loop), everything else is treated as a crash.
const ExitRestart = 75

// maxAttempts is the number of startup attempts allowed after the swap. The new version increments the count by 1 on each startup, and if it survives
// settleDelay the marker is cleared; crashing maxAttempts times in a row means the new version simply won't start, so it auto-rolls back.
const maxAttempts = 3

// Paths are all files involved in one upgrade, uniformly anchored under the **directory containing the executable**.
// Deliberately not using CWD: when running as a service the working directory may be / or any arbitrary path, and using CWD would make staged files land
// elsewhere, rendering the swap logic useless.
type Paths struct {
	Dir     string // directory containing the executable
	Current string // currently running binary        artex      / artex.exe
	New     string // staged new version            artex.new  / artex.new.exe
	Sum     string // sha256 of new version (hex)  artex.new.sha256 / artex.new.exe.sha256
	Old     string // Old version backed up before swap      artex.old  / artex.old.exe
	Marker  string // Upgrade status marker            artex.upgrade.json
}

// ResolvePaths derives all upgrade paths from the current executable.
//
// On Windows, .new/.old must also carry the .exe suffix, otherwise both the smoke test and post-swap execution will fail,
// so strip the suffix first and then concatenate, so the naming is symmetric on both platforms.
func ResolvePaths() (Paths, error) {
	exe, err := os.Executable()
	if err != nil {
		return Paths{}, fmt.Errorf("locating executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)
	name := filepath.Base(exe)
	ext := filepath.Ext(name) // ".exe" on Windows, usually empty on Unix
	stem := strings.TrimSuffix(name, ext)

	join := func(suffix string) string { return filepath.Join(dir, stem+suffix+ext) }
	return Paths{
		Dir:     dir,
		Current: exe,
		New:     join(".new"),
		Sum:     join(".new") + ".sha256",
		Old:     join(".old"),
		Marker:  filepath.Join(dir, stem+".upgrade.json"),
	}, nil
}

// marker records the progress of a swap, used to trigger automatic rollback when the new version fails to start.
type marker struct {
	From     string `json:"from"`     // Version before upgrade
	To       string `json:"to"`       // Target version
	Attempts int    `json:"attempts"` // Number of start attempts after swap
	StagedAt int64  `json:"staged_at"`
}

func readMarker(path string) (marker, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return marker{}, false
	}
	var m marker
	if json.Unmarshal(b, &m) != nil {
		return marker{}, false
	}
	return m, true
}

func writeMarker(path string, m marker) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// cleanStaged clears the staged files. Used on successful swap, verification failure, and user cancellation, to avoid leftover
// artex.new being retried on the next startup.
func cleanStaged(p Paths) {
	_ = os.Remove(p.New)
	_ = os.Remove(p.Sum)
}

// CompareVersions compares two version numbers, returning -1/0/1 (a<b / a==b / a>b).
// ok=false means at least one side is not a comparable version number (e.g. "dev" from a local dev build or
// "0.3.7-2-gabc1234-dirty" produced by git describe), in which case the caller should disable one-click update,
// Otherwise, a development build would be "upgraded" to the release version, overwriting uncommitted changes.
func CompareVersions(a, b string) (int, bool) {
	av, aok := parseVersion(a)
	bv, bok := parseVersion(b)
	if !aok || !bok {
		return 0, false
	}
	for i := range 3 {
		if av[i] != bv[i] {
			if av[i] < bv[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// parseVersion parses version numbers in the form "v0.3.7" / "0.3.7" into [3]int.
//
// Only accepts pure three-segment versions: build.sh uses git describe for non-tag builds to produce
// versions with suffixes like "0.3.7-2-gabc1234", which must be judged as non-comparable rather than treated as
// 0.3.7 — otherwise a development build would be misjudged as 'already up to date' or overwritten by the release version.
func parseVersion(s string) ([3]int, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return [3]int{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// InDocker reports whether the process runs in a container. Under Docker, the swap writes to the container's writable layer,
// and `docker compose up -d` recreating the container reverts to the image's bundled version — this is expected behavior
// (at that point the user is already pulling a new image), but the frontend should be able to explain this clearly.
func InDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	b, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	s := string(b)
	return strings.Contains(s, "docker") || strings.Contains(s, "containerd")
}
