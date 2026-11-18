package notify

// Snapshot is the contract for the notification_events.snapshot JSONB column. The writer is the db layer's
// finding persistence transaction; the readers are the server layer's delivery engine and filter matching. The definition lives in this package because it is
// the "notification domain" payload: db is only responsible for serialization and does not understand field semantics.
//
// Why store the finding fields redundantly rather than querying back at render time: findings get renamed, re-rated, and re-statused after the fact,
// whereas push content should reflect the conclusion **at the time of the event**—querying back would yield a dangerously misleading
// "later changed to low". Also, fan-out and rendering therefore don't need to JOIN the findings/tasks/assets three tables.
type Snapshot struct {
	// Event types: finding_created / finding_status_changed
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

// Item is a finding to be pushed, for channel rendering.
type Item struct {
	FindingID int64
	Name      string
	VulnClass string
	Severity  string
	Summary   string
	// Assets is the resolved asset display name (e.g., domain/IP). Filled in by the server layer—
	// this package doesn't touch the database and can't get the names.
	Assets []string
	// DetailURL is the finding detail backlink; empty means public_base_url is not configured and it is omitted at render time.
	DetailURL string
	// Specific to status change events; when both are non-empty, rendered as "Pending → Fixed".
	FromStatus string
	ToStatus   string
}

// IsStatusChange reports whether this entry is a status change event.
func (i Item) IsStatusChange() bool { return i.FromStatus != "" || i.ToStatus != "" }

// Title returns the entry's display title: prefer the manually named name, fall back to the finding type vulnclass,
// and when both are empty use a placeholder—never output an empty title.
func (i Item) Title() string {
	if i.Name != "" {
		return i.Name
	}
	if i.VulnClass != "" {
		return i.VulnClass
	}
	return "(Unnamed finding)"
}

// Message is the complete content of a single channel send.
type Message struct {
	// Length is 1 for a single push; for a digest push it is a whole batch.
	// An empty slice is invalid; the caller must ensure at least one entry.
	Items []Item
	// When Batch=true, render as a digest message (different title, with the time window and count).
	Batch bool
	// WindowMinutes is the digest period (in minutes), used only when Batch=true for the "last N minutes" wording.
	// Deliberately passed in explicitly via config rather than computing time.Since at render time: rendering stays deterministic and thus testable.
	WindowMinutes int
	// HomeURL is the platform panel address (global public_base_url); if empty, no panel entry is included.
	HomeURL string
}
