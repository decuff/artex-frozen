package notify

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Filter is the contract for the notification_channels.filter JSONB column: the filter conditions of a channel instance.
// All fields are optional; omitted means "no filtering" — this is exactly the fallback semantics for malformed configs, see ParseFilter.
type Filter struct {
	// MinSeverity is the minimum severity threshold (low/medium/high/critical); empty = no threshold.
	MinSeverity string `json:"min_severity"`
	// An empty TaskIDs / AssetIDs array means unlimited; otherwise the event must intersect with it.
	TaskIDs  []int64 `json:"task_ids"`
	AssetIDs []int64 `json:"asset_ids"`
	// An empty VulnClassInclude means accept all; otherwise vulnclass must hit any one of the keywords.
	// VulnClassExclude excludes if it hits any keyword (exclude takes precedence over include).
	// Matching is case-insensitive substring — safer than regex: a user's bad regex won't silently break the channel.
	VulnClassInclude []string `json:"vulnclass_include"`
	VulnClassExclude []string `json:"vulnclass_exclude"`
	// OnStatusChange determines whether this channel receives vuln status change events (only meaningful in realtime mode).
	OnStatusChange bool `json:"on_status_change"`
}

// ParseFilter parses the channel filter config.
//
// **Never returns error.** This is a deliberate design choice: when filter conditions are malformed, always degrade to a zero-value
// Filter (= no filtering = match everything), because for a vuln notification system, **pushing one extra is far better than
// silently missing one high-severity finding**. Turning parse failures into "don't push" gives the user a channel that looks configured
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
// that is, a single typo in min_severity ("hgih") makes the filter **silently fail** into
// "push everything". This is consistent with the package's "rather over-push than under-push" tradeoff (no missing),
// but the consequence is the user thinks they are doing tiered pushing, when actually all vulns flood into the group,
// with no sign at all telling them they misconfigured. This kind of "silent degradation" should be caught at the entrance.
//
// Note Validate is only for the **write** path. The read path still uses ParseFilter's lenient semantics,
// so bad values already present in historical data won't make the channel completely unreadable.
func (f Filter) Validate() error {
	if !ValidMinSeverity(f.MinSeverity) {
		return fmt.Errorf("Minimum severity %q is invalid; options: low / medium / high / critical, or leave empty for unlimited", f.MinSeverity)
	}
	return nil
}

// Match determines whether an event should be delivered to a channel with this filter.
//
// **Never returns error**, for the same reason as ParseFilter: any internal anomaly is treated as "match".
// Order of judgment: event type → severity threshold → task/asset scope → vuln class keywords.
func Match(f Filter, s Snapshot) bool {
	// Status change events are only received by channels that explicitly enable them. Off by default, because most users
	// expect "push" to mean "new vuln discovered", not tracking every status transition like a ledger.
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
	// Exclude first: hitting any exclude keyword means out, even if the include list is also hit.
	if len(f.VulnClassExclude) > 0 && containsAnyFold(s.VulnClass, f.VulnClassExclude) {
		return false
	}
	if len(f.VulnClassInclude) > 0 && !containsAnyFold(s.VulnClass, f.VulnClassInclude) {
		return false
	}
	return true
}

func intersectsInt(a, b []int64) bool {
	// Linear scan over small sets suffices; both sides are on the order of "a few dozen hand-picked items",
	// so the cost of building a map outweighs the benefit.
	for _, v := range b {
		if slices.Contains(a, v) {
			return true
		}
	}
	return false
}

// containsAnyFold reports whether s contains any keyword in keywords (case-insensitive).
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
