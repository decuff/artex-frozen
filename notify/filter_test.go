package notify

import "testing"

func TestParseFilterMalformedFallsBackToMatchAll(t *testing.T) {
	// Malformed JSON, empty input, fields with wrong types — all must degrade to a zero-value Filter,
	// i.e. 'no filtering'. This invariant is the anchor for 'better to over-push than to miss a push':
	// once this is changed to error out or half-parse, a user mistyping one character will silently drop all high-severity notifications.
	cases := []struct {
		name string
		raw  string
	}{
		{"empty input", ""},
		{"invalid JSON", `{not json`},
		{"truncated JSON", `{"min_severity":`},
		{"type mismatch", `{"min_severity": 123, "task_ids": "abc"}`},
		{"top level is an array", `[1,2,3]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := ParseFilter([]byte(tc.raw))
			if f.MinSeverity != "" || len(f.TaskIDs) != 0 || len(f.AssetIDs) != 0 {
				t.Fatalf("malformed config should degrade to a zero-value Filter, got %+v", f)
			}
			// A zero-value Filter must match any event.
			ev := Snapshot{Kind: EventFindingCreated, Severity: "low", VulnClass: "XSS"}
			if !Match(f, ev) {
				t.Fatal("a zero-value Filter should match all events")
			}
		})
	}
}

func TestMatchSeverityThreshold(t *testing.T) {
	ev := func(sev string) Snapshot {
		return Snapshot{Kind: EventFindingCreated, Severity: sev}
	}
	cases := []struct {
		min    string
		sev    string
		expect bool
	}{
		{"", "low", true},
		{"", "critical", true},
		{"high", "critical", true},
		{"high", "high", true},
		{"high", "medium", false},
		{"high", "low", false},
		{"critical", "high", false},
		{"critical", "critical", true},
		// Unknown severity ordinal is 0, should be blocked by any non-empty threshold (when in doubt, don't push).
		{"low", "", false},
		{"low", "unknown", false},
		{"", "", true},
	}
	for _, tc := range cases {
		got := Match(Filter{MinSeverity: tc.min}, ev(tc.sev))
		if got != tc.expect {
			t.Errorf("min=%q sev=%q: expected %v got %v", tc.min, tc.sev, tc.expect, got)
		}
	}
}

func TestMatchScopeRestrictions(t *testing.T) {
	ev := Snapshot{
		Kind:      EventFindingCreated,
		Severity:  "high",
		TaskID:    7,
		AssetIDs:  []int64{10, 20},
		VulnClass: "SQL injection",
	}
	cases := []struct {
		name   string
		filter Filter
		expect bool
	}{
		{"empty scope = unrestricted", Filter{}, true},
		{"task match", Filter{TaskIDs: []int64{7}}, true},
		{"task no match", Filter{TaskIDs: []int64{8}}, false},
		{"task multi-select includes match", Filter{TaskIDs: []int64{8, 7}}, true},
		{"assets intersect", Filter{AssetIDs: []int64{20, 99}}, true},
		{"assets no intersection", Filter{AssetIDs: []int64{99}}, false},
		{"task and asset both match", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{10}}, true},
		{"task matches but asset does not", Filter{TaskIDs: []int64{7}, AssetIDs: []int64{99}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev); got != tc.expect {
				t.Errorf("expected %v got %v", tc.expect, got)
			}
		})
	}
}

func TestMatchVulnClassKeywords(t *testing.T) {
	ev := func(class string) Snapshot {
		return Snapshot{Kind: EventFindingCreated, Severity: "high", VulnClass: class}
	}
	cases := []struct {
		name   string
		filter Filter
		class  string
		expect bool
	}{
		{"include empty = accept all", Filter{}, "any type", true},
		{"include match", Filter{VulnClassInclude: []string{"SQL"}}, "SQL injection", true},
		{"include miss", Filter{VulnClassInclude: []string{"Command Execution"}}, "SQL Injection", false},
		{"include any of multiple words hit", Filter{VulnClassInclude: []string{"Command Execution", "SQL"}}, "SQL Injection", true},
		{"case insensitive", Filter{VulnClassInclude: []string{"sql"}}, "SQL Injection", true},
		{"exclude hit means excluded", Filter{VulnClassExclude: []string{"Information Disclosure"}}, "Information Disclosure", false},
		{"exclude miss means allowed", Filter{VulnClassExclude: []string{"Information Disclosure"}}, "SQL Injection", true},
		// Exclusion takes priority over inclusion: when both hit, it should be excluded.
		{"exclusion takes priority over inclusion", Filter{
			VulnClassInclude: []string{"SQL"},
			VulnClassExclude: []string{"Injection"},
		}, "SQL Injection", false},
		// Pure whitespace keywords should be ignored, otherwise it degrades to 'match all strings containing a space'.
		{"whitespace keywords ignored", Filter{VulnClassInclude: []string{"", "  "}}, "SQL Injection", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.filter, ev(tc.class)); got != tc.expect {
				t.Errorf("expected %v got %v", tc.expect, got)
			}
		})
	}
}

func TestMatchStatusChangeRequiresOptIn(t *testing.T) {
	ev := Snapshot{Kind: EventFindingStatusChanged, Severity: "critical", FromStatus: "pending", ToStatus: "fixed"}
	// Default off: what most people mean by 'push vulnerabilities' is discovering new vulnerabilities, not a status changelog.
	if Match(Filter{MinSeverity: "low"}, ev) {
		t.Fatal("status change events should be skipped when not enabled")
	}
	if !Match(Filter{OnStatusChange: true}, ev) {
		t.Fatal("status change events should hit after enabling on_status_change")
	}
	// Create events are not affected by on_status_change.
	created := Snapshot{Kind: EventFindingCreated, Severity: "critical"}
	if !Match(Filter{MinSeverity: "low"}, created) {
		t.Fatal("create events should not depend on on_status_change")
	}
}
