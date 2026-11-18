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

// Global setting keys (stored in the settings key-value table; no table creation needed).
const (
	// settingNotifyEnabled is the master push switch. Default is on: it is used for one-click stop-the-bleeding during maintenance,
	// not as the feature's enable condition — the real enable condition is "is any channel configured".
	settingNotifyEnabled = "notify_enabled"
	// settingNotifyPublicBaseURL is the external address used to generate vulnerability detail back links
	// (e.g. https://artex.example.com). If left empty, messages carry no back-link button.
	// The project has no reusable external address configuration, so it is added here.
	settingNotifyPublicBaseURL = "notify_public_base_url"
	// settingNotifyDigestMinutes is the period (in minutes) for digest mode.
	settingNotifyDigestMinutes = "notify_digest_interval_min"
)

const (
	// notifyTick is the polling interval of the delivery engine. 3 seconds is the upper bound of the engine's real-time responsiveness
	// and is also the main source of latency between "vulnerability saved" and "message arrives in IM".
	notifyTick = 3 * time.Second
	// notifyLease is the lease duration when claiming a delivery. It must be significantly longer than the worst-case time of a single delivery
	// (the notify package's HTTP client timeout is 15 seconds), otherwise the same row will be
	// delivered by two dispatchers at once.
	notifyLease = 3 * time.Minute
	// notifyFanOutPerTick limits the number of events dispatched per round, preventing a one-time
	// expansion of the entire historical backlog into delivery tasks when a channel is first enabled.
	notifyFanOutPerTick = 200
	// notifyDefaultDigestMinutes is the default value for the digest period.
	notifyDefaultDigestMinutes = 30
	// notifyUnlimitedBurstPerTick is the per-round delivery cap for channels with no rate limit.
	// Its purpose is to prevent "a channel configured with no rate limit + a scan that produces thousands of vulnerabilities at once" from
	// dragging a single round into a long blocking loop.
	notifyUnlimitedBurstPerTick = 50
	// notifyMaxSendsPerChannelPerTick is the maximum number of deliveries per channel per round.
	//
	// This cap is derived backward from the **lease duration**: when claiming, rows are stamped with a lease (notifyLease = 3 minutes).
	// If serial deliveries in one round are numerous enough that the worst-case time exceeds the lease, the lease of the last few expires before they finish sending.
	// Within a single process this does not matter (Run is a single goroutine running serially, so ticks do not re-enter), but **when two
	// processes connect to the same database**, the peer will re-claim rows whose lease has expired and resend them, and will also
	// attempts is incremented twice, so a delivery is judged failed while the original process is still delivering.
	//
	// Value: 3-minute lease / 30-second single timeout = 6 exactly uses up the lease with zero margin,
	// so it cannot be used; 5 leaves 30 seconds of margin on the 150-second worst case. This relationship is
	// pinned by TestNotifyTickBudgetFitsWithinLease — changing notifyLease,
	// notifySendTimeout, or this value will make that assertion fail.
	notifyMaxSendsPerChannelPerTick = 5
	// notifySendTimeout is the timeout for a single delivery. It also determines the value of the previous constant;
	// their product must not exceed notifyLease, see TestNotifyTickBudgetFitsWithinLease.
	notifySendTimeout = 30 * time.Second
)

// notifyBackoff is the backoff sequence for failed retries, indexed by the number of attempts made.
// 3 chances (including the first) corresponds to db.MaxNotifyAttempts; both must be changed together.
var notifyBackoff = []time.Duration{
	time.Second,
	5 * time.Second,
	30 * time.Second,
}

// Notifier is the delivery engine for vulnerability pushes.
//
// It runs as an independent goroutine alongside Scheduler (see server.New). It deliberately does not reuse
// Scheduler's tick: the push's real-time requirement (3 seconds) differs from the trigger's business cadence,
// and the two fail independently — a stuck push should not affect agent triggering.
type Notifier struct {
	s  *Server
	pg *db.DB

	// mu protects buckets. The number of channels is small and contention is low, so one mutex is enough;
	// it is not worth introducing a finer-grained structure for it.
	mu      sync.Mutex
	buckets map[int64]*notifyBucket
}

// notifyBucket is the per-channel token bucket.
//
// A token bucket is used instead of a "count per minute then reset" sliding window because the latter has bad boundary effects:
// sending 20 messages at the end of a window and 20 more the next instant means 40 per second to the platform,
// which gets rate-limited; the token bucket refills at a constant rate, naturally avoiding such bursts.
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
// Any step failure only logs and does not interrupt the loop — a notification system failure must never escalate to a process-level problem.
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
		// The token bucket's unit is **message count** (equivalent to HTTP request count), not vulnerability count.
		// In real-time mode the two are the same (one vulnerability, one message); in digest mode a whole batch of vulnerabilities is combined into
		// one message, so it consumes only one token.
		//
		// Both modes ask the token bucket first, then claim by quota — the order must not be reversed, otherwise deliveries blocked by rate limiting
		// have already consumed retry attempts.
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

// digestTickPlan returns the token consumption and batch size upper bound for digest channels this round.
//
// The two return values are **two different dimensions**, which is exactly why this is a separate function:
//
//   - tokens is the message count. A batch of vulnerabilities is combined into one message and sent as one HTTP request, so it is always 1.
//     rate_per_min therefore still applies to digest (at most this many digest messages per minute).
//   - claimLimit is the maximum number of vulnerabilities in this batch. It is constrained only by the memory upper bound and is unrelated to the request budget.
//
// Previously, to make rate_per_min apply to digest, the per-round request budget
// (notifyMaxSendsPerChannelPerTick, derived from the lease) was passed directly as the batch size.
// The consequence was that a channel with rate_per_min=20 only refilled to 1 token in a 3-second tick, so every digest
// The message contains only 1 finding—digest degenerates into a "realtime push with a summary caption," and what readers receive is a string of
// "1 new finding in the last 30 minutes," while db.MaxDigestBatchSize is never reachable.
//
// This symptom is not easy to spot in end-to-end tests (existing cases all manually pass a sufficiently large limit to
// stepDigest, bypassing the quota calculation in step), so the decision is pinned down here by
// TestDigestTickPlanDecouplesBatchSizeFromSendBudget.
func digestTickPlan() (tokens, claimLimit int) {
	return 1, db.MaxDigestBatchSize
}

// stepRealtime claims and delivers realtime tasks for a channel, one message per finding.
func (n *Notifier) stepRealtime(ctx context.Context, ch *db.NotificationChannel, allow int, baseURL string) {
	deliveries, err := n.pg.ClaimRealtimeDeliveries(ctx, ch.ID, allow, notifyLease)
	if err != nil {
		log.Printf("[notify] failed to claim realtime delivery channel=%d: %v", ch.ID, err)
		return
	}
	if len(deliveries) == 0 {
		return
	}
	channel, cfg, ok := n.adapt(ch)
	if !ok {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("Channel type %q is not registered", ch.Kind))
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

// stepDigest aggregates pending deliveries for a channel into one message when the batch is due.
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
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), fmt.Sprintf("Channel type %q is not registered", ch.Kind))
		return
	}
	msg, included, err := n.renderBatch(ctx, deliveries, baseURL, int(window.Minutes()))
	if err != nil {
		_ = n.pg.FailDeliveries(ctx, deliveryIDs(deliveries), err.Error())
		return
	}
	// Deliveries whose snapshot is broken and could not enter the message must be explicitly marked failed. Otherwise they remain
	// outside included—neither in the message nor in the failure list—and when sending succeeds their status will be
	// missed by the later bulk marking, forever stuck in sending until the lease expires and they are repeatedly claimed.
	if skipped := excludeDeliveries(deliveries, included); len(skipped) > 0 {
		reason := "event snapshot could not be parsed; this finding could not be rendered into a message"
		if fErr := n.pg.FailDeliveries(ctx, deliveryIDs(skipped), reason); fErr != nil {
			log.Printf("[notify] failed to mark broken-snapshot delivery failed channel=%s ids=%v: %v", ch.Kind, deliveryIDs(skipped), fErr)
		}
		log.Printf("[notify] skipped %d deliveries with unparseable snapshots channel=%d", len(skipped), ch.ID)
	}
	// Pass only those that entered the message to send: included[i] strictly corresponds to msg.Items[i],
	// and send relies on this correspondence to apply "the channel reported it fit the first K items" to the correct delivery rows.
	n.send(ctx, channel, cfg, msg, included)
}

// send delivers and transitions status by result.
//
// The same batch of deliveries (possibly dozens in digest mode) shares one send result: either delivered or the whole batch retried.
// No per-item retry—a digest message is one message, and resending part of it would corrupt the batch semantics.
//
// The only exception is **segmentation caused by the channel length limit**: if the channel reports it actually fit only the first K items,
// then item K+1 onward must be left for the next batch rather than marked successful along with the rest. Otherwise the truncated
// findings would be neither in the message nor in the failure list—completely gone.
func (n *Notifier) send(ctx context.Context, channel notify.Channel, cfg map[string]any, msg notify.Message, deliveries []*db.NotificationDelivery) {
	// Set a limit per delivery to avoid one channel getting stuck and holding up all remaining channels in this round.
	sendCtx, cancel := context.WithTimeout(ctx, notifySendTimeout)
	defer cancel()
	delivered, err := channel.Send(sendCtx, cfg, msg)
	if err == nil && delivered > 0 {
		if delivered > len(deliveries) {
			// The number of items reported by the channel cannot exceed the delivery count; if it does, the rendering layer miscalculated—
			// treat all as delivered and record the problem, better than corrupting the records.
			log.Printf("[notify] channel reported %d delivered exceeding delivery count %d channel=%s, treating all as delivered",
				delivered, len(deliveries), channel.Kind())
			delivered = len(deliveries)
		}
		sent, rest := deliveries[:delivered], deliveries[delivered:]
		if err := n.pg.MarkDeliveriesSent(ctx, deliveryIDs(sent)); err != nil {
			log.Printf("[notify] failed to mark delivered failed channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(sent), err)
		}
		if len(rest) > 0 {
			// This message has reached the channel length limit: the rest are immediately requeued and will be sent by the next tick.
			// Use DeferDeliveries rather than RescheduleDeliveries—this is not a failure,
			// and should not consume retry budget (claiming already optimistically +1'd it, which will be subtracted there).
			if err := n.pg.DeferDeliveries(ctx, deliveryIDs(rest),
				fmt.Sprintf("This message has reached the channel length limit; only the first %d delivered, the rest left for the next batch", delivered)); err != nil {
				log.Printf("[notify] failed to enqueue segmented continuation channel=%s ids=%v: %v", channel.Kind(), deliveryIDs(rest), err)
			}
		}
		return
	}
	if err == nil {
		// The channel neither reported an error nor said how many were delivered. Treat as failure (with backoff),
		// so this delivery is not repeatedly claimed yet never markable.
		err = fmt.Errorf("channel did not report delivery count (delivered=%d)", delivered)
	}

	// Handle failures **one by one**, instead of judging by the maximum attempt count of the whole batch.
	//
	// It used to be `if maxAttempts(deliveries) >= MaxNotifyAttempts`, failing the whole batch, but attempts
	// differ per item within a batch: an old delivery that has already retried twice (attempts=2) would drag
	// a brand-new delivery (attempts=1) in the same batch into failed together—a new finding is permanently lost
	// without a single retry, exactly the opposite of the intent of "don't let old rows drag new rows down."
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
	// Reorder by delay group: there are only 3 backoff tiers, so the number of groups is naturally small; no need to issue a separate
	// UPDATE per item (that would cause a 500-item batch to make 500 round trips).
	for delay, group := range byDelay {
		if rErr := n.pg.RescheduleDeliveries(ctx, group, delay, err.Error()); rErr != nil {
			log.Printf("[notify] error reordering deliveries channel=%s ids=%v: %v", channel.Kind(), group, rErr)
		}
	}
	if len(failIDs)+len(exhaustedIDs) > 0 {
		log.Printf("[notify] delivery failed channel=%d kind=%s permanent failures=%d retries exhausted=%d pending retry=%d: %s",
			deliveries[0].ChannelID, channel.Kind(), len(failIDs), len(exhaustedIDs), len(byDelay), err)
	}
}

// excludeDeliveries returns those in all that are not in keep (compared by pointer identity).
// Used to find deliveries that "didn't make it into the message"—they must be explicitly handled, not left in a gray area.
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
		// On config parse failure, give an empty map: the channel's own Validate will report "which field is missing,"
		// and that error guides users to fix the issue better than a JSON parse error.
		_ = json.Unmarshal(ch.Config, &cfg)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return channel, cfg, true
}

// renderSingle renders a single finding message.
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

// renderBatch renders the digest message. Parse snapshots one by one—if a single one is bad, skip only that one,
// don't let it drag down the whole digest.
//
// The returned included and msg.Items are **strictly one-to-one** (the i-th delivery ↔ the i-th entry).
// This correspondence is a hard requirement: the caller marks the first K deliveries as delivered based on "the channel reported it accepted the first K items."
// delivered. If a bad snapshot is skipped here without removing the skipped delivery from included,
// the indices will be misaligned—the bad entry that should fail would be marked as delivered, and good entries would be misjudged as undelivered.
// The bad ones are explicitly marked failed by the caller; see stepDigest.
func (n *Notifier) renderBatch(ctx context.Context, deliveries []*db.NotificationDelivery, baseURL string, windowMinutes int) (notify.Message, []*db.NotificationDelivery, error) {
	items := make([]notify.Item, 0, len(deliveries))
	included := make([]*db.NotificationDelivery, 0, len(deliveries))
	for _, dl := range deliveries {
		snap, err := parseSnapshot(dl)
		if err != nil {
			// Bad snapshots don't go into the message or included—the caller is responsible for handling them
			// (explicitly marking them failed, rather than sneaking them through as "delivered").
			log.Printf("[notify] skipping unparseable snapshot in digest batch delivery=%d: %v", dl.ID, err)
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
		return notify.Message{}, nil, fmt.Errorf("all %d deliveries in digest batch are unparseable", len(deliveries))
	}
	return notify.Message{
		Items:         items,
		Batch:         true,
		WindowMinutes: windowMinutes,
		HomeURL:       baseURL,
	}, included, nil
}

// itemFor renders the event snapshot into a to-be-pushed entry, also resolving the asset name and detail backlink.
func (n *Notifier) itemFor(ctx context.Context, snap notify.Snapshot, baseURL string) (notify.Item, error) {
	assets, err := n.pg.NotificationAssetNames(ctx, snap.AssetIDs)
	if err != nil {
		// Asset name resolution failure should not block pushing: failing to read the name is far lighter than not receiving the notification,
		// the message just lacks one asset line.
		log.Printf("[notify] error resolving asset name finding=%d: %v", snap.FindingID, err)
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
		// For the detail page route, see web/src/app/(main)/function/findings/detail/page.tsx,
		// which reads the finding id from the query parameter id.
		item.DetailURL = fmt.Sprintf("%s/function/findings/detail?id=%d", baseURL, snap.FindingID)
	}
	return item, nil
}

// takeTokens takes **at most want** tokens from the channel token bucket and returns the number actually taken.
//
// One token = one message (one HTTP request). In realtime mode, the caller passes as many as there are items;
// in digest mode, an entire batch of findings sends only one message, so pass 1.
//
// The bucket capacity is the per-minute limit for this channel, refilled at a constant rate. ratePerMin<=0 means no throttling;
// return a finite but sufficiently large value to prevent a single loop iteration from being stalled by unbounded backlog.
//
// The want limit is required: without it the bucket can only be fully drained, while the caller itself has a per-round limit;
// extra tokens taken are both unused and vanish before the next refill—accumulated burst capacity is never reachable,
// and even "no pending deliveries this round" still deducts a batch.
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
	// Refill based on actual elapsed time, at a rate of ratePerMin/60 per second.
	if elapsed := now.Sub(b.lastFill).Seconds(); elapsed > 0 {
		b.tokens = minF(float64(ratePerMin), b.tokens+elapsed*float64(ratePerMin)/60)
		b.lastFill = now
	}
	// Add a tiny epsilon before truncating: the token count is accumulated in floating point, and when refilling in two steps
	// 0.5 + 0.5 may yield 0.9999999999, which int() would truncate to 0—
	// a mathematically full bucket can yield no tokens. 1e-9 is far smaller than one token, so no real shortfall is let through.
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

// publicBaseURL returns the external address used for backlinks, with the trailing slash removed.
func (n *Notifier) publicBaseURL() string {
	v, ok, err := n.pg.GetSetting(settingNotifyPublicBaseURL)
	if err != nil || !ok {
		return ""
	}
	return trimTrailingSlash(v)
}

// digestInterval returns the digest interval, falling back to the default when invalid or unset.
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

// parseSnapshot parses the event snapshot corresponding to the delivery.
func parseSnapshot(dl *db.NotificationDelivery) (notify.Snapshot, error) {
	var snap notify.Snapshot
	if len(dl.Snapshot) == 0 {
		return snap, fmt.Errorf("event snapshot for delivery %d is empty", dl.ID)
	}
	if err := json.Unmarshal(dl.Snapshot, &snap); err != nil {
		return snap, fmt.Errorf("failed to parse event snapshot for delivery %d: %w", dl.ID, err)
	}
	if snap.Kind == "" {
		// The event type follows the event row; the copy in the snapshot may have been written by an older version.
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
