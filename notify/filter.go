package notify

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Filter is the contract for the notification_channels.filter JSONB column: the filter conditions for a channel instance.
// All fields are optional; absent means 'no filtering' — this is exactly the fallback semantics for malformed configs, see ParseFilter.
type Filter struct {
	// MinSeverity is the minimum severity threshold (low/medium/high/critical); empty = no threshold.
	MinSeverity string `json:"min_severity"`
	// TaskIDs / AssetIDs as empty arrays mean unrestricted; non-empty requires the event to intersect with them.
	TaskIDs  []int64 `json:"task_ids"`
	AssetIDs []int64 `json:"asset_ids"`
	// VulnClassInclude empty means accept all; non-empty requires vulnclass to match any of the keywords.
	// VulnClassExclude excludes on matching any keyword (exclusion takes priority over inclusion).
	// Matching is case-insensitive substring — safer than regex: a user's bad regex won't silently break the channel.
	VulnClassInclude []string `json:"vulnclass_include"`
	VulnClassExclude []string `json:"vulnclass_exclude"`
	// OnStatusChange determines whether this channel receives vulnerability status change events (only meaningful in realtime mode).
	OnStatusChange bool `json:"on_status_change"`
}

// ParseFilter parses the channel filter config.
//
// **Never returns an error.** This is a deliberate design choice: when the filter config is malformed, it always degrades to a zero-value
// Filter (= no filtering = match everything), because for a vulnerability notification system, **pushing one extra is far better than
// silently missing a high-severity one**. Making a parse failure mean 'don't push' would give the user a channel that looks configured
// but actually pushes nothing — the worst failure mode.
func ParseFilter(raw []byte) Filter {
	var f Filter
	if len(raw) == 0 {
		return f
	}
	// On parse failure f stays at zero value, i.e. no filtering.
	_ = json.Unmarshal(raw, &f)
	return f
}

// ValidMinSeverity reports whether s is a valid severity threshold (empty string means no threshold).
func ValidMinSeverity(s string) bool {
	if s == "" {
		return true
	}
	_, ok := severityRank[s]
	return ok
}

// Validate checks the **value-constrained** fields in the filter config, for use when saving a channel.
//
// Why it must be caught at write time: Match's judgment for an unknown threshold is `rank >= 0`, always true —
// In other words, if min_severity has a typo ("hgih"), the filter will **silently fail** and become
// 'push everything'. This is consistent with this package's trade-off direction of 'rather push extra than miss' (won't miss),
// but the consequence is the user thinks they're doing tiered pushing while actually flooding the group with all vulnerabilities,
// and there's no sign at all hinting they misconfigured it. This kind of 'silent degradation' should be caught at the entry point.
//
// Note Validate is only used on the **write** path. The read path still uses ParseFilter's lenient semantics,
// so bad values already present in historical data won't make the channel entirely unreadable.
func (f Filter) Validate() error {
	if !ValidMinSeverity(f.MinSeverity) {
		return fmt.Errorf("minimum severity %q is invalid, options: low / medium / high / critical, or leave empty for unrestricted", f.MinSeverity)
	}
	return nil
}

// Match determines whether an event should be delivered to a channel with this filter config.
//
// **Never returns an error**, for the same reason as ParseFilter: any internal exception is treated as a 'hit'.
// Evaluation order: event type → level threshold → task/asset scope → vulnerability type keywords.
func Match(f Filter, s Snapshot) bool {
	// Status-change events are only received by channels that are explicitly enabled. Off by default, because most users
	// expect 'push' to mean 'new vulnerability discovered', not a running log following every status transition.
	if s.Kind == EventFindingStatusChanged && !f.OnStatusChange {
		return false
	}
	if !AtLeast(s.Severity, f.MinSeverity) {
		return false
	}
	if len(f.TaskIDs) > 0 && !slices.Contains(f.TaskIDs, s.TaskID) {
		return false
	}
	if len(f.AssetIDs) > 0 && !intersectsInt(f.AssetIDs, s.AssetIDs) {
		return false
	}
	// Exclusion takes priority: a hit on any exclusion keyword means it's out, even if it also matches the include list.
	if len(f.VulnClassExclude) > 0 && containsAnyFold(s.VulnClass, f.VulnClassExclude) {
		return false
	}
	if len(f.VulnClassInclude) > 0 && !containsAnyFold(s.VulnClass, f.VulnClassInclude) {
		return false
	}
	return true
}

func intersectsInt(a, b []int64) bool {
	// A small set can just be linearly scanned; both sides are on the order of 'a few dozen hand-picked items',
	// so the overhead of building a map outweighs the benefit.
	for _, v := range b {
		if slices.Contains(a, v) {
			return true
		}
	}
	return false
}

// containsAnyFold reports whether s contains any of the keywords (case-insensitive).
func containsAnyFold(s string, keywords []string) bool {
	lower := strings.ToLower(s)
	for _, kw := range keywords {
		kw = strings.ToLower(strings.TrimSpace(kw))
		if kw != "" && strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}
