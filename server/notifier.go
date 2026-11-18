package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/notify"
)

// Global settings keys (stored in the settings key-value table, no table creation needed).
const (
	// settingNotifyEnabled is the master push switch. Default on: it is used for one-click stop-the-bleeding during maintenance,
	// rather than being the feature's enable condition—the real enable condition is 'whether a channel is configured'.
	settingNotifyEnabled = "notify_enabled"
	// settingNotifyPublicBaseURL is the external access address used to generate vulnerability detail backlinks
	// (e.g. https://artex.example.com). If left empty, messages carry no backlink button.
	// The project has no reusable external address config, so a new item is added here.
	settingNotifyPublicBaseURL = "notify_public_base_url"
	// settingNotifyDigestMinutes is the digest mode period (in minutes).
	settingNotifyDigestMinutes = "notify_digest_interval_min"
)

const (
	// notifyTick is the delivery engine's polling interval. 3 seconds is the upper bound of this engine's real-time behavior,
	// and is also the main source of latency between 'vulnerability persisted' and 'message reaching IM'.
	notifyTick = 3 * time.Second
	// notifyLease is the lease duration when claiming a delivery. It must be significantly greater than the worst-case time of a single delivery
	// (the notify package's HTTP client timeout is 15 seconds), otherwise the same row could be delivered by two
	// dispatchers simultaneously.
	notifyLease = 3 * time.Minute
	// notifyFanOutPerTick limits the number of events dispatched per round, avoiding expanding the entire historical backlog
	// into delivery tasks all at once when a channel is first enabled.
	notifyFanOutPerTick = 200
	// notifyDefaultDigestMinutes is the default value for the digest period.
	notifyDefaultDigestMinutes = 30
	// notifyUnlimitedBurstPerTick is the per-round delivery cap when a channel has no rate limit set.
	// Its purpose is to prevent 'a channel configured as unlimited + a single scan finding thousands of vulnerabilities' from
	// dragging a single round's loop into a long block.
	notifyUnlimitedBurstPerTick = 50
	// notifyMaxSendsPerChannelPerTick is the max number of deliveries per channel per round.
	//
	// This cap is derived backwards from the **lease duration**: claiming marks the row with a lease (notifyLease = 3 minutes),
	// if the number of serial deliveries in one round is large enough that the worst-case time exceeds the lease, the lease expires before the last few are sent.
	// Within a single process it doesn't matter (Run is a single goroutine running serially, ticks don't re-enter), but with **two
	// processes connected to the same database**, the peer will re-claim rows with expired leases and send them again, and will also
	// double-increment attempts, judging them as failed while the original process is still delivering.
	//
	// Value: 3-minute lease / 30-second single timeout = 6 is **exactly using up the lease**, zero margin,
	// Cannot take; taking 5 leaves 30 seconds of margin on the worst-case 150 seconds. This relationship is pinned down by
	// TestNotifyTickBudgetFitsWithinLease — changing notifyLease,
	// notifySendTimeout, or this value will make that assertion fail.
	notifyMaxSendsPerChannelPerTick = 5
	// notifySendTimeout is the timeout for a single delivery. It also determines the value of the previous constant,
	// and the product of the two must not exceed notifyLease, see TestNotifyTickBudgetFitsWithinLease.
	notifySendTimeout = 30 * time.Second
)

// notifyBackoff is the backoff sequence for retries on failure, indexed by the number of attempts made.
// 3 chances (including the first) corresponds to db.MaxNotifyAttempts; the two must be changed together.
var notifyBackoff = []time.Duration{
	time.Second,
	5 * time.Second,
	30 * time.Second,
}

// Notifier is the delivery engine for vulnerability push notifications.
//
// It runs alongside Scheduler as an independent goroutine (see server.New). Deliberately does not reuse
// Scheduler's tick: the real-time requirement of push (3 seconds) differs from the business rhythm of triggers,
// and their failures are independent — a stuck push should not affect agent triggering.
type Notifier struct {
	s  *Server
	pg *db.DB

	// mu protects buckets. The number of channels is small and contention is low, so a single mutex is enough,
	// and it is not worth introducing a finer-grained structure for it.
	mu      sync.Mutex
	buckets map[int64]*notifyBucket
}

// notifyBucket is the token bucket for a single channel.
//
// A token bucket is used instead of a 'count per minute then reset' sliding window because the latter has terrible boundary effects:
// sending 20 messages at the end of the window and another 20 the next instant means 40 in one second to the platform,
// which gets rate-limited; a token bucket refills at a constant rate, naturally avoiding this burst.
type notifyBucket struct {
	tokens   float64
	lastFill time.Time
}

func newNotifier(s *Server) *Notifier {
	return &Notifier{s: s, pg: s.m.pg, buckets: map[int64]*notifyBucket{}}
}

// Run loops until ctx ends. Started once by server.New.
func (n *Notifier) Run(ctx context.Context) {
	if n.pg == nil {
		return
	}
	t := time.NewTicker(notifyTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.step(ctx)
		}
	}
}

// step runs one round: first dispatch new events, then deliver due tasks.
//
// Any step failure only logs and does not interrupt the loop — a notification system failure must never escalate into a process-level problem.
// Each tick is independent, and the next round will naturally retry.
func (n *Notifier) step(ctx context.Context) {
	if !n.enabled() {
		return
	}
	if _, _, err := n.pg.FanOutPendingEvents(ctx, notifyFanOutPerTick); err != nil {
		log.Printf("[notify] failed to dispatch events: %v", err)
		return
	}
	channels, err := n.pg.ListNotificationChannels(ctx)
	if err != nil {
		log.Printf("[notify] failed to read channels: %v", err)
		return
	}
	baseURL := n.publicBaseURL()
	for _, ch := range channels {
		if !ch.IsEnabled() {
			continue
		}
		// The unit of measurement for the token bucket is **number of messages** (equivalent to HTTP requests), not number of vulnerabilities.
		// In real-time mode the two are the same (one vulnerability, one message); in summary mode an entire batch of vulnerabilities is combined into
		// One message, so only one token is consumed.
		//
		// Both modes first ask the token bucket, then claim according to the quota — the order must not be reversed, otherwise those blocked by rate limiting
		// have already consumed retry attempts on delivery.
		now := time.Now()
		if ch.Mode == db.NotifyModeDigest {
			tokens, claimLimit := digestTickPlan()
			if n.takeTokens(ch.ID, ch.RatePerMin, tokens, now) <= 0 {
				continue
			}
			n.stepDigest(ctx, ch, claimLimit, baseURL)
			continue
		}
		allow := n.takeTokens(ch.ID, ch.RatePerMin, notifyMaxSendsPerChannelPerTick, now)
		if allow <= 0 {
			continue
		}
		n.stepRealtime(ctx, ch, allow, baseURL)
	}
}

// digestTickPlan returns the token consumption and the upper bound of batch size for the summary channel in this round.
//
// The two return values are **two different dimensions**, which is exactly the reason for making them a separate function:
//
//   - tokens is the number of messages. A batch of vulnerabilities is combined into one message and sent as one HTTP request, so it is always 1.
//     rate_per_min therefore still applies to digest (at most this many summary messages per minute).
//   - claimLimit is the maximum number of vulnerabilities this batch can hold. It is constrained only by the memory upper bound and is unrelated to the request budget.
//
// Previously, in order to make rate_per_min apply to digest, the per-round request budget
// (notifyMaxSendsPerChannelPerTick, derived backwards from the lease) was passed down directly as the batch size.
// The consequence was that a channel with rate_per_min=20 only refilled 1 token in a 3-second tick, so each summary
// message held only 1 vulnerability — digest degenerated into 'real-time push with summary text', and readers received a string of
// '1 new vulnerability in the last 30 minutes', while db.MaxDigestBatchSize was never reachable.
//
// This symptom is not easy to spot in end-to-end tests (existing cases all manually pass a sufficiently large limit to
// stepDigest, bypassing the quota calculation in step), so the decision is consolidated here and pinned directly by
// TestDigestTickPlanDecouplesBatchSizeFromSendBudget.
func digestTickPlan() (tokens, claimLimit int) {
	return 1, db.MaxDigestBatchSize
}

// stepRealtime claims and delivers real-time tasks for a channel, one vulnerability per message.
func (n *Notifier) stepRealtime(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	deliveries, err := n.pg.ClaimRealtimeDeliveries(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] failed to claim real-time deliveries channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("channel type %q not registered", ch.Kind))
		return
	}
	for _, dl := range deliveries {
		msg, err := n.renderSingle(ctx, dl, baseURL)
		if err != nil {
			// Render failure is a local data problem; retrying will not make it better.
			_ = n.pg.FailDeliveries(ctx, []int64{dl.ID}, err.Error())
			continue
		}
		n.send(ctx, channel, cfg, msg, []*db.NotificationDelivery{dl})
	}
}

// stepDigest aggregates the pending deliveries of a channel into one message and sends it when the batch is due.
func (n *Notifier) stepDigest(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	window := n.digestInterval()
	due, err := n.pg.DigestBatchDue(ctx, ch.ID, window)
	if err != nil {
		log.Printf("[notify] failed to determine digest batch channel=%d: %v", ch.ID, err)
		return
	}
	if !due {
		return
	}
	deliveries, err := n.pg.ClaimDigestBatch(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] failed to claim digest batch channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("channel type %q not registered", ch.Kind))
		return
	}
	msg, included, err := n.renderBatch(ctx, deliveries, baseURL, int(window.Minutes()))
	if err != nil {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), err.Error())
		return
	}
	// Those deliveries whose snapshot is broken and that failed to make it into the message must be explicitly marked as failed. Otherwise they will remain in
	// Outside of included, they neither enter the message nor the failure list—when sending succeeds, their status will be
	// missed by the subsequent batch marking, forever stuck at sending until the lease expires and they are repeatedly claimed.
	if skipped := excludeDeliveries(deliveries, included); len(skipped) > 0 {
		reason := "event snapshot cannot be parsed, this vulnerability cannot be rendered into a message"
		if fErr := n.pg.FailDeliveries(ctx, deliveryIDs(skipped), reason); fErr != nil {
			log.Printf("[notify] failed to mark bad snapshot delivery channel=%s ids=%v: %v", ch.Kind, deliveryIDs(skipped), fErr)
		}
		log.Printf("[notify] skipped %d deliveries with unparseable snapshots channel=%d", len(skipped), ch.ID)
	}
	// Only hand those that entered the message to send: included[i] strictly corresponds to msg.Items[i],
	// send relies on this correspondence to map 'channel reported it fit the first K items' onto the correct delivery rows.
	n.send(ctx, channel, cfg, msg, included)
}

// send delivers and transitions status based on the result.
//
// The same batch of deliveries (possibly dozens in aggregate mode) shares one send result: either delivered, or the whole batch retries.
// No per-item retry—an aggregate message is one message; resending part of it would corrupt the batch semantics.
//
// The only exception is **segmentation caused by the channel length limit**: the channel reports it actually only fit the first K items,
// then from item K+1 onward they must be left for the next batch, rather than being marked successful along with the rest. Otherwise the truncated
// vulnerabilities are neither in the message nor in the failure list, and vanish completely.
func (n *Notifier) send(ctx context.Context, channel notify.Channel, cfg map[string]any, msg notify.Message, deliveries []*db.NotificationDelivery) {
	// Set a limit per delivery to avoid one channel getting stuck and dragging down all remaining channels in this round.
	sendCtx, cancel := context.WithTimeout(ctx, notifySendTimeout)
	defer cancel()
	delivered, err := channel.Send(sendCtx, cfg, msg)
	if err == nil && delivered > 0 {
		if delivered > len(deliveries) {
			// The channel-reported count cannot exceed the delivery count; if it does, the rendering layer miscalculated,
			// so treat it as all delivered and log the problem—better than corrupting the records.
			log.Printf("[notify] channel reported delivered count %d exceeds delivery count %d channel=%s, treating as all delivered",
				delivered, len(deliveries), channel.Kind())
			delivered = len(deliveries)
		}
		sent, rest := deliveries[:delivered], deliveries[delivered:]
		if err := n.pg.MarkDeliveriesSent(ctx, deliveryIDs(sent)); err != nil {
			log.Printf("[notify] failed to mark as delivered channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(sent), err)
		}
		if len(rest) > 0 {
			// This message has reached the channel length limit: the rest immediately go back to the queue and will be sent by the next tick.
			// Use DeferDeliveries instead of RescheduleDeliveries—this is not a failure,
			// and should not consume retry budget (it was already optimistically +1 on claim, which will be subtracted back there).
			if err := n.pg.DeferDeliveries(ctx, deliveryIDs(rest),
				fmt.Sprintf("this message has reached the channel length limit, only the first %d delivered, the rest left for the next batch", delivered)); err != nil {
				log.Printf("[notify] failed to queue segmented continuation channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(rest), err)
			}
		}
		return
	}
	if err == nil {
		// The channel neither reported an error nor said how many were delivered. Treat as failure (with backoff),
		// so this delivery isn't repeatedly claimed yet never marked.
		err = fmt.Errorf("channel did not report delivered count (delivered=%d)", delivered)
	}

	// Failure handling is decided **per item**, not by taking the max attempts of the whole batch.
	//
	// It used to be `if maxAttempts(deliveries) >= MaxNotifyAttempts` failing the whole batch, but within a batch
	// the attempt counts of each item are not the same: an old delivery that has already retried twice (attempts=2) would drag
	// a brand-new delivery (attempts=1) in the same batch into failed together—the new vulnerability is permanently lost without a single retry,
	// exactly the opposite of the intent of 'don't let old rows drag new rows down'.
	permanent := notify.IsPermanent(err)
	var failIDs, exhaustedIDs []int64
	byDelay := map[time.Duration][]int64{}
	for _, dl := range deliveries {
		switch {
		case permanent:
			failIDs = append(failIDs, dl.ID)
		case dl.Attempts >= db.MaxNotifyAttempts:
			exhaustedIDs = append(exhaustedIDs, dl.ID)
		default:
			delay := notifyBackoff[min(dl.Attempts, len(notifyBackoff)-1)]
			byDelay[delay] = append(byDelay[delay], dl.ID)
		}
	}

	if len(failIDs) > 0 {
		if fErr := n.pg.FailDeliveries(ctx, failIDs, err.Error()); fErr != nil {
			log.Printf("[notify] error marking failed status channel=%s ids=%v: %v", channel.Kind(), failIDs, fErr)
		}
	}
	if len(exhaustedIDs) > 0 {
		reason := fmt.Sprintf("still failed after %d retries: %s", db.MaxNotifyAttempts, err)
		if fErr := n.pg.FailDeliveries(ctx, exhaustedIDs, reason); fErr != nil {
			log.Printf("[notify] error marking failed status channel=%s ids=%v: %v", channel.Kind(), exhaustedIDs, fErr)
		}
	}
	// Regroup by delay: there are only 3 backoff tiers, so the number of groups is naturally small; no need to issue a separate
	// UPDATE for each item (that would cause a 500-item batch to make 500 round trips).
	for delay, group := range byDelay {
		if rErr := n.pg.RescheduleDeliveries(ctx, group, delay, err.Error()); rErr != nil {
			log.Printf("[notify] failed to regroup deliveries channel=%s ids=%v: %v", channel.Kind(), group, rErr)
		}
	}
	if len(failIDs)+len(exhaustedIDs) > 0 {
		log.Printf("[notify] delivery failed channel=%d kind=%s permanent failures=%d retries exhausted=%d pending retry=%d: %s",
			deliveries[0].ChannelID, channel.Kind(), len(failIDs), len(exhaustedIDs), len(byDelay), err)
	}
}

// excludeDeliveries returns those in all that are not in keep (compared by pointer identity).
// Used to find deliveries that 'did not make it into the message'—they must be explicitly handled and cannot be left in limbo.
func excludeDeliveries(all, keep []*db.NotificationDelivery) []*db.NotificationDelivery {
	inKeep := make(map[*db.NotificationDelivery]bool, len(keep))
	for _, dl := range keep {
		inKeep[dl] = true
	}
	var out []*db.NotificationDelivery
	for _, dl := range all {
		if !inKeep[dl] {
			out = append(out, dl)
		}
	}
	return out
}

// adapt fetches the channel implementation and parses its config.
// Returning ok=false means the type is not registered; the delivery should be failed directly rather than retried indefinitely.
func (n *Notifier) adapt(ch *db.NotificationChannel) (notify.Channel, map[string]any, bool) {
	channel, ok := notify.Get(ch.Kind)
	if !ok {
		return nil, nil, false
	}
	var cfg map[string]any
	if len(ch.Config) > 0 {
		// On config parse failure, give an empty map: the channel's own Validate will report 'which field is missing',
		// and that error guides the user to fix it better than a JSON parse error.
		_ = json.Unmarshal(ch.Config, &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return channel, cfg, true
}

// renderSingle renders a single vulnerability message.
func (n *Notifier) renderSingle(ctx context.Context, dl *db.NotificationDelivery, baseURL string) (notify.Message, error) {
	snap, err := parseSnapshot(dl)
	if err != nil {
		return notify.Message{}, err
	}
	item, err := n.itemFor(ctx, snap, baseURL)
	if err != nil {
		return notify.Message{}, err
	}
	return notify.Message{Items: []notify.Item{item}, HomeURL: baseURL}, nil
}

// renderBatch renders a summary message. Snapshots are parsed one by one—if a single one is broken, only that one is skipped,
// so it doesn't take down the whole batch summary.
//
// The returned included and msg.Items are **strictly one-to-one** (the i-th delivery ↔ the i-th entry).
// This correspondence is a hard requirement: the caller decides the first K deliveries based on 'the channel reported it fit the first K items'
// to mark as delivered. If a broken snapshot is skipped here without removing the skipped delivery from included,
// Otherwise the indices would be misaligned—bad entries that should fail would be marked as delivered, while good entries would be misjudged as undelivered.
// The broken ones are explicitly marked as failed by the caller, see stepDigest.
func (n *Notifier) renderBatch(ctx context.Context, deliveries []*db.NotificationDelivery, baseURL string, windowMinutes int) (notify.Message, []*db.NotificationDelivery, error) {
	items := make([]notify.Item, 0, len(deliveries))
	included := make([]*db.NotificationDelivery, 0, len(deliveries))
	for _, dl := range deliveries {
		snap, err := parseSnapshot(dl)
		if err != nil {
			// Bad snapshots don't go into the message, nor into included—their handling is the caller's responsibility
			// (explicitly marked as failed, rather than sneaking through mixed in with 'delivered').
			log.Printf("[notify] skipping unparseable snapshot in summary batch delivery=%d: %v", dl.ID, err)
			continue
		}
		item, err := n.itemFor(ctx, snap, baseURL)
		if err != nil {
			return notify.Message{}, nil, err
		}
		items = append(items, item)
		included = append(included, dl)
	}
	if len(items) == 0 {
		return notify.Message{}, nil, fmt.Errorf("all %d deliveries in summary batch are unparseable", len(deliveries))
	}
	return notify.Message{
		Items:         items,
		Batch:         true,
		WindowMinutes: windowMinutes,
		HomeURL:       baseURL,
	}, included, nil
}

// itemFor renders an event snapshot into a pending push item, along the way resolving the asset name and detail backlink.
func (n *Notifier) itemFor(ctx context.Context, snap notify.Snapshot, baseURL string) (notify.Item, error) {
	assets, err := n.pg.NotificationAssetNames(ctx, snap.AssetIDs)
	if err != nil {
		// Asset name resolution failure shouldn't block the push: failing to read the name is far lighter than not receiving the notification,
		// the message is just missing one line for the asset.
		log.Printf("[notify] failed to resolve asset name finding=%d: %v", snap.FindingID, err)
	}
	item := notify.Item{
		FindingID:  snap.FindingID,
		Name:       snap.Name,
		VulnClass:  snap.VulnClass,
		Severity:   snap.Severity,
		Summary:    snap.Summary,
		Assets:     assets,
		FromStatus: snap.FromStatus,
		ToStatus:   snap.ToStatus,
	}
	if baseURL != "" {
		// For the detail page route see web/src/app/(main)/function/findings/detail/page.tsx,
		// it reads the vulnerability id from the query parameter id.
		item.DetailURL = fmt.Sprintf("%s/function/findings/detail?id=%d", baseURL, snap.FindingID)
	}
	return item, nil
}

// takeTokens takes **at most want** tokens from the channel's token bucket, returning the number actually taken.
//
// One token = one message (one HTTP request). In realtime mode the caller passes however many it needs;
// in summary mode an entire batch of vulnerabilities sends only one message, so pass 1.
//
// The bucket capacity is the channel's per-minute limit, refilled at a constant rate. ratePerMin<=0 means no rate limiting,
// returning a finite but sufficiently large value, to prevent a single round's loop from being stalled by unbounded backlog.
//
// The want cap is essential: without it the bucket could only be drained entirely, while the caller itself has a per-round cap,
// tokens taken in excess are neither usable nor do they vanish into thin air before the next refill—the accumulated burst capacity would forever be unreachable,
// and even 'no pending deliveries this round' would still incur a deduction.
func (n *Notifier) takeTokens(channelID int64, ratePerMin, want int, now time.Time) int {
	if want <= 0 {
		return 0
	}
	if ratePerMin <= 0 {
		return min(want, notifyUnlimitedBurstPerTick)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	b := n.buckets[channelID]
	if b == nil {
		b = &notifyBucket{tokens: float64(ratePerMin), lastFill: now}
		n.buckets[channelID] = b
	}
	// Refill by the actual elapsed time, at a rate of ratePerMin/60 per second.
	if elapsed := now.Sub(b.lastFill).Seconds(); elapsed > 0 {
		b.tokens = minF(float64(ratePerMin), b.tokens+elapsed*float64(ratePerMin)/60)
		b.lastFill = now
	}
	// Add a tiny epsilon before truncating: the token count is accumulated in floating point, and when refilling to full in two steps
	// 0.5 + 0.5 may yield 0.9999999999, which int() would truncate to 0—
	// a mathematically full bucket yet unable to yield a token. 1e-9 is far smaller than one token, so it won't let a real shortfall through.
	take := min(int(b.tokens+1e-9), want)
	if take <= 0 {
		return 0
	}
	b.tokens -= float64(take)
	return take
}

// enabled reads the master switch.
func (n *Notifier) enabled() bool {
	return n.pg.GetBool(settingNotifyEnabled, true)
}

// publicBaseURL returns the external address used for callback links, with the trailing slash removed.
func (n *Notifier) publicBaseURL() string {
	v, ok, err := n.pg.GetSetting(settingNotifyPublicBaseURL)
	if err != nil || !ok {
		return ""
	}
	return trimTrailingSlash(v)
}

// digestInterval returns the aggregation period, falling back to the default value when invalid or unconfigured.
func (n *Notifier) digestInterval() time.Duration {
	v, ok, err := n.pg.GetSetting(settingNotifyDigestMinutes)
	if err != nil || !ok {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	m := 0
	if _, err := fmt.Sscanf(v, "%d", &m); err != nil || m <= 0 {
		return time.Duration(notifyDefaultDigestMinutes) * time.Minute
	}
	return time.Duration(m) * time.Minute
}

// parseSnapshot parses the snapshot of the event corresponding to the delivery.
func parseSnapshot(dl *db.NotificationDelivery) (notify.Snapshot, error) {
	var snap notify.Snapshot
	if len(dl.Snapshot) == 0 {
		return snap, fmt.Errorf("event snapshot for delivery %d is empty", dl.ID)
	}
	if err := json.Unmarshal(dl.Snapshot, &snap); err != nil {
		return snap, fmt.Errorf("failed to parse event snapshot for delivery %d: %w", dl.ID, err)
	}
	if snap.Kind == "" {
		// The event type is determined by the event row; the one in the snapshot may have been written by an older version.
		snap.Kind = dl.EventKind
	}
	return snap, nil
}

func deliveryIDs(deliveries []*db.NotificationDelivery) []int64 {
	out := make([]int64, 0, len(deliveries))
	for _, dl := range deliveries {
		out = append(out, dl.ID)
	}
	return out
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func minF(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
