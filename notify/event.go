package notify

// Snapshot is the contract for the notification_events.snapshot JSONB column. The writer is the db layer's
// vulnerability persistence transaction, and the reader is the server layer's delivery engine and filter matching. The definition lives in this package because it is
// the 'notification domain' payload: db is only responsible for serialization and does not understand field meanings.
//
// Why redundantly store vulnerability fields instead of looking them up at render time: vulnerabilities get renamed, re-leveled, and re-statused after the fact,
// and the pushed content should reflect the conclusion **at the time of the incident**—a lookup would yield the
// dangerous misleading result of 'changed to low after the fact'. Additionally, fan-out and rendering therefore don't need to JOIN the findings/tasks/assets three tables.
type Snapshot struct {
	// Event type: finding_created / finding_status_changed
	Kind      string  `json:"kind"`
	FindingID int64   `json:"finding_id"`
	TaskID    int64   `json:"task_id"`
	VulnClass string  `json:"vulnclass"`
	Name      string  `json:"name"`
	Severity  string  `json:"severity"`
	Summary   string  `json:"summary"`
	AssetIDs  []int64 `json:"asset_ids"`
	// Non-empty only when kind=finding_status_changed.
	FromStatus string `json:"from_status,omitempty"`
	ToStatus   string `json:"to_status,omitempty"`
}

// Item is a vulnerability pending push, for channel rendering.
type Item struct {
	FindingID int64
	Name      string
	VulnClass string
	Severity  string
	Summary   string
	// Assets are the resolved asset display names (e.g. domain/IP). Filled by the server layer—
	// this package doesn't touch the database, so it can't get the names.
	Assets []string
	// DetailURL is the vulnerability detail backlink; empty means public_base_url is not configured, omitted at render time.
	DetailURL string
	// For status change events only; when both are non-empty, rendered as 'pending → fixed'.
	FromStatus string
	ToStatus   string
}

// IsStatusChange reports whether the entry is a status change event.
func (i Item) IsStatusChange() bool { return i.FromStatus != "" || i.ToStatus != "" }

// Title returns the entry's display title: prefer the manually named name, fall back to the vulnerability type vulnclass,
// and when both are empty use a placeholder—never output an empty title.
func (i Item) Title() string {
	if i.Name != "" {
		return i.Name
	}
	if i.VulnClass != "" {
		return i.VulnClass
	}
	return "(unnamed vulnerability)"
}

// Message is the complete content of a single channel send.
type Message struct {
	// Length is 1 for a single push; for a digest push it is an entire batch.
	// An empty slice is invalid; the caller must guarantee at least one entry.
	Items []Item
	// When Batch=true, render as a digest message (different title, include the time window and count).
	Batch bool
	// WindowMinutes is the digest period (minutes), used for the 'last N minutes' wording only when Batch=true.
	// Deliberately passed in explicitly from config rather than computing time.Since at render time: rendering stays deterministic, which makes it testable.
	WindowMinutes int
	// HomeURL is the platform panel address (global public_base_url); if empty, no panel entry is included.
	HomeURL string
}
