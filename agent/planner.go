package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Planner is the event-driven LLM planner (docs §4.3): each time the asset or
// exploration graph changes (debounced), it reads the exploration route, queries
// assets, judges whether the task goal is met, and emits 0..N exploration intents
// into the frontier. It is the sole intent generator.
type Planner struct {
	findingRecorder   FindingRecorder
	prov              llm.Provider
	model             string
	tx                *transcript.Store                      // raw LLM conversation persistence (nil = off)
	window            int                                    // context window in tokens (for compaction)
	windowFn          func() int                             // optional dynamic task-chain minimum
	maxTurns          int                                    // max agent turns per run (0 = unlimited)
	killWork          func(intentID int64) error             // engine callback to terminate a running work (nil = off)
	steerWork         func(intentID int64, msg string) error // engine callback to steer a running work mid-run (nil = off)
	proxyAddr         string                                 // recording proxy for WebFetch (empty = direct)
	proxyCACert       string                                 // recording proxy's CA cert path (HTTPS verify)
	webSearch         WebSearchOpts                          // web_search tool backend selection (off by default)
	workDir           string                                 // shared work dir (surfaced in prompt as artifact-output target)
	injectConstraints func() bool                            // resolver: inject task operation constraints into system prompt? (nil = yes)
	nonStreamingFn    func() bool                            // resolver: use non-streaming (Complete) path? (nil = streaming)
	noaEnabledFn      func() bool                            // resolver: use experimental noa compaction? (nil = off)
	maxTokensFn       func() int                             // resolver: per-reply output cap (nil/0 = send no cap)
	compactor         *Compactor                             // cold-node compaction (§7); nil = disabled

	// todos keeps ONE plan-scratchpad per task (keyed by exploration id) so the
	// planner's multi-step plan survives across wake-ups — each Plan() is a fresh
	// session, but the shared store lets it record a serial exploit chain once and
	// dispatch it step-by-step over rounds instead of front-loading it in parallel.
	todoMu sync.Mutex
	todos  map[int64]*actool.TodoStore
}

func NewPlanner(prov llm.Provider, model, workDir string, tx *transcript.Store, window, maxTurns int) *Planner {
	return &Planner{prov: prov, model: model, workDir: workDir, tx: tx, window: window, maxTurns: maxTurns, todos: map[int64]*actool.TodoStore{}}
}

func (p *Planner) SetCompactionWindowResolver(fn func() int) { p.windowFn = fn }

// SetCompactor wires the cold-node compactor (cold-digest §7). Called each
// planner wake-up to advance the round counter, maintain cold stamps, and
// (off the hot path) fold cold nodes into digests. nil = feature disabled.
func (p *Planner) SetCompactor(c *Compactor) { p.compactor = c }

// SetNonStreaming wires a resolver deciding whether runs use the non-streaming
// model path (true = non-streaming). nil/unset = streaming (default).
func (p *Planner) SetNonStreaming(fn func() bool) { p.nonStreamingFn = fn }

func (p *Planner) nonStreaming() bool { return p.nonStreamingFn != nil && p.nonStreamingFn() }

// SetNoaEnabled wires a resolver deciding whether runs use the experimental noa
// context-compression mechanism. nil/unset = off (built-in compaction). Read per
// run so the settings toggle takes effect without rebuilding the agent.
func (p *Planner) SetNoaEnabled(fn func() bool) { p.noaEnabledFn = fn }

// SetMaxTokens wires a resolver for the per-reply output cap. nil/unset or 0 =
// send no cap and let the endpoint decide. Read per run, like nonStreaming.
func (p *Planner) SetMaxTokens(fn func() int) { p.maxTokensFn = fn }

func (p *Planner) maxTokens() int {
	if p.maxTokensFn == nil {
		return 0
	}
	return p.maxTokensFn()
}

func (p *Planner) compactionWindow() int {
	if p.windowFn != nil {
		return p.windowFn()
	}
	return p.window
}

// SetProxy points the planner's WebFetch at the recording proxy plus the CA cert
// it trusts to verify HTTPS through it (empty addr = direct).
func (p *Planner) SetProxy(addr, caCert string) { p.proxyAddr, p.proxyCACert = addr, caCert }

// SetWebSearch selects the web_search backend for the planner (off by default).
func (p *Planner) SetWebSearch(o WebSearchOpts) { p.webSearch = o }

// SetConstraintInject wires a resolver deciding whether this task's operation
// constraints get injected into the planner system prompt. Read per round so the
// settings toggle takes effect without rebuilding the agent. nil = inject (default).
func (p *Planner) SetConstraintInject(fn func() bool) { p.injectConstraints = fn }

// wantConstraints reports whether constraint injection is enabled (default yes).
func (p *Planner) wantConstraints() bool { return p.injectConstraints == nil || p.injectConstraints() }

// todoFor returns the task's persistent planning todo store, creating it on first
// use. Shared across all of this task's planner wake-ups.
func (p *Planner) todoFor(expID int64) *actool.TodoStore {
	p.todoMu.Lock()
	defer p.todoMu.Unlock()
	s := p.todos[expID]
	if s == nil {
		s = actool.NewTodoStore()
		p.todos[expID] = s
	}
	return s
}

// SetKillWork wires the engine's per-work terminate callback so the planner's
// kill_work tool can stop a single running worker.
func (p *Planner) SetKillWork(fn func(intentID int64) error) { p.killWork = fn }

// SetSteerWork wires the engine's per-work steering callback so the planner's
// steer_work tool can inject a mid-run course-correction into a running worker.
func (p *Planner) SetSteerWork(fn func(intentID int64, msg string) error) { p.steerWork = fn }

// renderPlannerTodos formats the persistent planning todo for injection into the
// wake-up prompt (empty when there are no todos yet — first wake-up).
func renderPlannerTodos(items []actool.Todo) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n[Your planning todos (persisted across wakeups, written by you in the previous round)]:\n")
	for _, it := range items {
		mark := map[actool.TodoStatus]string{actool.TodoPending: "☐", actool.TodoInProgress: "▶", actool.TodoCompleted: "✔"}[it.Status]
		if mark == "" {
			mark = "☐"
		}
		b.WriteString(fmt.Sprintf("  %s %s\n", mark, it.Content))
	}
	b.WriteString("Proceed accordingly: only dispatch intents for the next steps whose [prerequisite steps are complete / their dependent facts already exist]; use TodoWrite to update the list (mark steps already satisfied by facts as completed). Do not re-dispatch steps already pending/in_progress in the list.")
	return b.String()
}

// TriggerEvent describes what concretely caused this planning round to fire, so
// the planner looks first at the actual change instead of re-scanning the whole
// overview. Kind:
//
//	"done"    — a worker finished intent IntentID (its output conclusion is fetched).
//	"finding" — a worker reported a finding on intent IntentID (Detail = summary).
//	"goal"    — the human (via the main agent's set_goals) added one OR MORE goals in a
//	            single call (Goals = the newly added goal texts this time, 1+ items; set_goals supports batch).
//	"goal_deleted" — the human deleted a goal from the overview's goal management (Detail = the deleted goal text).
//	"goal_edited"  — the human edited a goal from the overview's goal management (OldGoal→NewGoal text).
//	"cancelled" — the human deleted intent IntentID (Detail = deletion reason). The intent is
//	            stopped (not deleted) and the reason is attached to it as a fact.
type TriggerEvent struct {
	Kind     string
	IntentID int64
	Detail   string
	Summary  string   // Kind=="cancelled" only: the intent summary captured before deletion (after real deletion the node no longer exists, so it can't be looked up)
	Goals    []string // Kind=="goal" only: the goal texts newly added by this set_goals call (1 or more)
	OldGoal  string   // Kind=="goal_edited" only: the goal text before editing
	NewGoal  string   // Kind=="goal_edited" only: the goal text after editing
	Hints    []string // Kind=="hint" only: the hint texts newly added by this add_hint call (1 or more)
}

// renderTriggers spells out the change(s) that fired this round: for a finished
// worker — which intent + its output conclusion; for a finding — which intent +
// what was found. Empty for time/heartbeat wakes. Reads the store (best-effort;
// a blank field never blocks the round).
func renderTriggers(ts *db.ExplorationStore, evs []TriggerEvent) string {
	if len(evs) == 0 || ts == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n[The actual changes that triggered this round (look here first, then decide whether to add directions)]:")
	for _, ev := range evs {
		switch ev.Kind {
		case "goal":
			if len(ev.Goals) == 1 {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added a goal: %s — a new goal to achieve; please add exploration directions accordingly (if there is no corresponding intent yet).", ev.Goals[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added %d goals: %s — all are new goals to achieve; please add exploration directions one by one for goals that have no corresponding intent yet.", len(ev.Goals), strings.Join(ev.Goals, "; ")))
			}
		case "hint":
			if len(ev.Hints) == 1 {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added a strategic hint: %s — it has been attached to the exploration graph; please adjust/add exploration directions accordingly (if there is no corresponding intent yet).", ev.Hints[0]))
			} else {
				b.WriteString(fmt.Sprintf("\n- The human (main agent) added %d strategic hints: %s — all have been attached to the exploration graph; please adjust/add exploration directions one by one accordingly.", len(ev.Hints), strings.Join(ev.Hints, "; ")))
			}
		case "goal_deleted":
			b.WriteString(fmt.Sprintf("\n- The human deleted this goal: %s — the goal has been removed; please re-evaluate the remaining goals/directions accordingly (no need to dispatch intents for it anymore).", ev.Detail))
		case "goal_edited":
			b.WriteString(fmt.Sprintf("\n- The human edited a goal, from '%s' to '%s' — please adjust exploration directions according to the new goal (stop dispatching the original direction if it no longer applies).", ev.OldGoal, ev.NewGoal))
		case "finding":
			b.WriteString(fmt.Sprintf("\n- The worker for intent #%d (%s) reported a finding: %s", ev.IntentID, intentSummary(ts, ev.IntentID), ev.Detail))
		case "cancelled":
			// For intent content, prefer the Summary captured at deletion time (after real deletion the node no longer exists, so intentSummary can't find it).
			sm := ev.Summary
			if sm == "" {
				sm = intentSummary(ts, ev.IntentID)
			}
			b.WriteString(fmt.Sprintf("\n- Intent #%d was deleted by the user; the intent content is: %s, and the deletion reason is: %s. This intent has been deleted (no longer executed); please re-plan accordingly.", ev.IntentID, sm, ev.Detail))
		default: // "done"
			b.WriteString(fmt.Sprintf("\n- The worker for intent #%d (%s) finished, output conclusion: %s", ev.IntentID, intentSummary(ts, ev.IntentID), workerOutput(ts, ev.IntentID)))
			if fids := factIDsYielded(ts, ev.IntentID); fids != "" {
				b.WriteString(fmt.Sprintf("; fact ids newly produced by this intent: %s ", fids))
			}
		}
	}
	b.WriteString("\n(For full details, query node_detail / get_worker_output / list_findings again.)")
	return b.String()
}

// factIDsYielded lists the fact ids an intent produced this run as "#12、#15", so the
// planner can jump straight to the round's incremental facts. Empty (best-effort) when
// the intent yielded no facts or the lookup fails.
func factIDsYielded(ts *db.ExplorationStore, id int64) string {
	ids, err := ts.FactsYielded(id)
	if err != nil || len(ids) == 0 {
		return ""
	}
	parts := make([]string, len(ids))
	for i, fid := range ids {
		parts[i] = fmt.Sprintf("#%d", fid)
	}
	return strings.Join(parts, "、")
}

// intentSummary reads an intent node's one-line summary (best-effort, "?" on miss).
func intentSummary(ts *db.ExplorationStore, id int64) string {
	n, err := ts.GetNode(id)
	if err != nil || n == nil {
		return "?"
	}
	var p map[string]any
	if json.Unmarshal(n.Payload, &p) == nil {
		if s, ok := p["summary"].(string); ok && s != "" {
			return s
		}
	}
	return "?"
}

// workerOutput returns the finished worker's conclusion for an intent — the last
// 'result' (else 'text') activity's full detail, truncated. Same source get_worker_output uses.
func workerOutput(ts *db.ExplorationStore, id int64) string {
	acts, _, err := ts.ActivityList(&id, 0, 1000)
	if err != nil {
		return "(failed to get output)"
	}
	var pick *db.Activity
	for i := range acts {
		if acts[i].Kind == "result" {
			pick = &acts[i]
		} else if acts[i].Kind == "text" && pick == nil {
			pick = &acts[i]
		}
	}
	if pick == nil {
		return "(this work has no output record yet)"
	}
	out, _ := ts.ActivityDetail(pick.ID)
	if out == "" {
		out = pick.Summary
	}
	return truncOutput(out, 800)
}

// truncOutput caps a worker-output blob so the trigger context doesn't bloat the
// system prompt every round; full text is one get_worker_output call away.
func truncOutput(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + " …(truncated, see get_worker_output for full)"
}

// renderGraphOverview folds the pre-computed graph_overview snapshot into the
// wake-up prompt so the planner starts each round with the full situation in
// hand — saving the round-trip it would otherwise spend calling the tool. It is
// the exact same JSON graph_overview would return; deeper detail is still one
// tool call away (node_detail / list_facts / …).
func renderGraphOverview(data map[string]any) string {
	b, err := json.Marshal(data)
	if err != nil {
		return "" // fall back to the model calling graph_overview itself
	}
	return "\n\n[Current-round situation (graph_overview prefetched, equivalent to the return of you calling that tool; for details call node_detail/list_facts etc. on demand)]:\n" + string(b)
}

// plannerDefaultTmpl is the built-in EDITABLE body (section [A]) of the planner prompt,
// seeded into agent_prompts. Goal is a {{.Goal}} template var; the intermediate-output spec
// tail is code-owned (artifactSpec) and appended by plannerSystem after rendering.
const plannerDefaultTmpl = `You are the "planner" of a network security platform's authorized penetration testing system, and you are woken frequently (woken whenever the graph changes). Responsibilities: read the situation → judge the goals → **only when there is genuinely a new uncovered direction** add exploration intents. You are the planner, not the executor: all outputs this round can only be [generate/clarify intents] or [judge goals]; never do the work inside the plan.

Task goal: {{.Goal}}

**How many intents to produce this round (think this through first)**:
- **Hard bottom line (highest priority)**: as long as [the goal is not met] and [there is currently no open or running intent] (frontier_open=0 and running_intents is empty), this round [must] produce at least one intent that advances toward the goal—when there is no running work to wait for and no queued direction, producing 0 intents = the task stalls; even if all known directions are only in recent_done, still open a new one or dispatch a follow-up based on the done/exhausted/blocked judgment below.
- Beyond the hard floor, **producing 0 intents is a normal result, but it must have a legitimate reason** (not the default of "dispatching fewer is safer"): ①**Already covered**—every direction you thought of is already handled by an intent still open/running (regenerating an existing intent with different wording is a serious error); ②**Waiting on a dependency**—the next step depends on the output of currently running work, and that output isn't out yet (forcing a dispatch here would leave downstream spinning without its prerequisite, so you should wait until the next wake-up graph update before dispatching).
- Conversely: if there really is a [new uncovered direction that does not depend on running work], or the goal is not met and there are still untested surfaces in scope, you should dispatch—don't treat 0 intents as the lazy default.

**Decision flow for each wake-up**:

1. **The full situation is attached below this prompt** (it is the return of graph_overview, no need to call it again): task (original title + goal/root node), asset counts, goals+status, open/running/recent_done intents, sites_without_endpoints (sites without endpoints, hinting at directions possibly yet to explore), facts (exploration fact count, a different category from vulnerabilities), recent_facts ({id,summary,confidence?}).
   - **Scope**: exploration nodes (goals/intents/facts/findings) contain only this task; **the asset graph is globally shared** (one copy across tasks; asset counts are global and in-scope, not unique to this task)—ignore assets unrelated to this task when they appear.
   - **Lineage**: each intent carries parents (upstream: which facts/intents it was derived from) and yields (downstream: which facts/findings it produced), and each recent_facts entry carries from_intent; use this to understand 'which facts come from which direction, and whether new directions can be synthesized'.
   - **Negative/uncertain observations** (such as "port closed/not injectable" in recent_facts) are the worker's observations, not conclusions: before accepting them, first check the evidence via node_detail(id)—only those with solid evidence, confidence=observed, and exhausted means are considered temporarily sealed in that direction; those with missing evidence, merely "looks like/only probed once", or confidence=inferred are treated as [not yet determined], and if within scope and not covered by other intents, by default dispatch a verification intent to confirm or refute (**at most one verification per negative direction**; if still negative after verification and the evidence is reasonable, respect that conclusion and dispatch no more).
   - **Call on demand for deeper details**: list_facts (paginated, newest first, default 20, supports q filter, before paging, with total/has_more), list_findings (all vulnerabilities), node_detail(id) (full evidence/details; lists/recent_facts only give summaries), list_assets (pull: q search, type/company_id/task_id filter, pagination, or direct id/ids lookup), asset_neighbors. Assets are globally shared, don't pull everything by default.

2. **Determine goals (core responsibility)**: The goals field already contains goals and statuses; for an unmet goal that has been proven by some finding/fact, call prove_goal(goal_id, evidence_id, reason) to mark it met. **When the one you mark happens to be the last unfinished goal, the system automatically judges the entire task complete**—the finish is driven only by proving goals one by one via prove_goal; there is no other 'one-click complete' method.
   - ⚠️ **Quantitative acceptance check (strictly forbidden to sign off early)**: When the goal contains quantifiable conditions (coverage reaches X%, capture N flags, obtain a certain permission), before prove_goal you 【must】 check the measured values in the graph_overview above (coverage.pct, findings_total count, etc.): if not up to standard, 【forbidden】 to prove_goal, instead dispatch an intent to close the gap; you must not mark met early on the grounds of 'broadly achieved / core already captured'. Example: requires 100% coverage but measured coverage.pct=40% → not achieved, keep dispatching supplementary-testing intents.

3. **(Optional, only at the start, very lightweight) probing for understanding**: only when the graph has almost no facts yet (recent_facts basically empty, task just started) and the situation alone cannot make the initial intent specific, use Bash etc. to do a very small amount of read-only probing of the target (e.g. 1–2 curls to see the homepage/fingerprint). **The only valid product is one more precise intent description**—absolutely not vulnerability discovery/verification/exploitation, nor endpoint/directory/parameter enumeration results (those are the worker's job, write them as intents and dispatch). Three hard boundaries:
   - If the graph already has facts produced by workers (facts>0 / recent_facts non-empty) → [forbidden] to probe on your own again; base all judgments on existing facts, and this round's output can only be 'dispatch a new intent' or 'end'; to dig deeper into a clue → dispatch an intent for a worker to investigate, not curl yourself.
   - Even at the start, probe at most ≤3 times then stop, just to clarify the initial intent; once you find yourself "investigating deeply" rather than "quickly setting direction" (enumerating endpoints/directories one by one, trying ids one by one, decoding chains, repeatedly probing the same interface, any injection/privilege-escalation/vulnerability testing and verification — all heavy work for the worker), stop immediately and write it down as intent.
   - If you can judge from existing facts/situation, there is no need to probe at all.

4. **Decide which new directions to add**: **'restraint' here only means [do not repeat existing intents], not 'send as few as possible'** — when the goal is not yet achieved, the default follow-up question is 'to get closer to the goal, what deeper, more aggressive, not-yet-covered approaches remain', not 'can we wrap up'. Intents are [open exploration directions] (not fixed types/menus); combine known facts, assets, and goals to self-determine directions, and compare each one against open + running + recent_done:
   - Already covered by open/running → don't generate again (being handled).
   - Appeared in recent_done → **first look at that intent's state (every entry carries it) to tell how it stopped, then decide**:
     · **done (completed normally)**: already covered → do not re-dispatch as-is; whether it is a dead end depends on the fact conclusion it yields, not on state; re-dispatch only when [materially new mechanism] appears (new facts/assets/parameters/clearly different approach), and the summary must clearly state how it differs from last time; rewording, "maybe it'll work if we try again" does not count, retries are forbidden.
     · **exhausted (budget ran out, cut off halfway through exploration, only partial results written back) / blocked (model or network failure, basically no exploration done)**: both mean it ended without a clean finish and with incomplete information — first use get_worker_trace / get_worker_output to see how far it actually got and where it got stuck, then choose from the following: cut off by budget right before a breakthrough → dispatch 'continue from last progress'; pure external failure, never ran (blocked is often this) → directly re-dispatch the same direction; stuck at the same place every time → switch approach/direction. The basis is always the real progress in the trace, not the state itself.
   - A completely new direction with no intent covering it at all → generate.
   - All known directions are covered by intents still in open/running → don't generate, just end (there's running/queued work, wait for them to progress); but if only recent_done covers them, no open/running remain, and the goal isn't achieved → per the hard bottom line at the top, must open new or continue dispatching.
   - **Depth takes priority over coverage**: coverage is a lower bound/acceptance item, not the exploration goal itself; after finding a high-value entry (possibly leading to RCE/privilege escalation/data exfiltration), prioritize dispatching intents to [drive that path deep through], rather than spreading wide to level up coverage and shallow-testing each asset one by one.
   - **Keep routes diverse, don't converge too early**: When the goal is not yet achieved, if the existing intents are all crowded onto the same route/entry point, and there exists an [essentially different] uncovered direction (another entry surface/another asset type/another exploitation chain), prioritize adding that divergent direction rather than adding synonymous intents on the same line (look at substantive differences, not wording); if that divergent direction is already covered by existing intents, still do not generate. Ideally, 2-3 routes with different mechanisms coexist (e.g. 'attack from the upload chain' and 'attack from the auth bypass'), and only after one of them delivers evidence of [approaching the goal] do you concentrate resources there. **But diversity always defers to the [operational constraints] at the top**: entry surfaces/ports/hosts/operations excluded by the constraints are never generated as intents, even if essentially different. 

   **Serial exploitation chains: dispatch step by step, don't split into parallel.** Strongly dependent serial chains (①→②→③, each later step depends on the actual output of the previous): do not dispatch all at once in parallel (downstream without a prerequisite that doesn't exist yet will only duplicate/spin); use TodoWrite to record the whole chain as todos (one per step), and this round dispatch only the step whose 'prerequisite is satisfied' (usually the first step); after it produces facts, on the next wake-up (the prompt will carry the todo list) dispatch the next step and mark the satisfied ones completed. Don't split 'the same thing' into two ('confirm the trigger point' and 'trigger the trigger point' are the same step); only [parallel, mutually independent] dimensions (e.g. enumerating multiple unrelated endpoints) use multiple intents in parallel.

5. **Submit**: use [one] add_intent call to batch-submit the filtered new directions (intents array, at most 4 highest-value ones, don't call multiple times one by one):
   - **summary**: one sentence in natural language describing the direction (full address of the test target + what to do + why), no fixed categorization; dedup mainly relies on comparing it with existing intents.
   - **asset_ids**: the target asset ids to test/attack in this direction (pass as many as possible, 0/1/multiple, from list_assets) — as long as the direction revolves around a specific asset (site/endpoint/param/host), be sure to pass them, used for coverage dedup and linking into the asset chain; if it spans multiple assets, pass them all; leave empty only for pure global reconnaissance with no specific asset.
   - **parent_ids**: which upstream nodes this direction was synthesized from (optional, 0/1/multiple) — if multiple facts combine to produce one intent, pass them all; if derived from some upstream intent/finding, also pass its id; leave empty for a top-level brand-new direction.

Don't repeat, don't force it; but when the goal isn't achieved and there are uncovered, deeper approaches, dispatch them. Concise, focused, efficient.`

func plannerSystem(goal, dataDir, workDir string) string {
	body := renderSystem("planner", plannerDefaultTmpl, PlannerVars{Goal: goal, DataDir: dataDir, Now: nowStr()})
	return body + artifactSpec(workDir)
}

// Plan runs one planning round. emit, if non-nil, receives the planner's execution
// steps (so users can see how it reads the situation and judges goals — the
// planner is the intent generator and was previously a black box). Returns whether
// the planner judged the goal met.
// triggers carries the concrete change(s) that fired this round — worker(s) done
// and/or finding(s) reported (may be several — the engine debounces a burst; empty
// for time/heartbeat wakes). They are spelled out at the top of the prompt so the
// planner looks first at the actual change (which intent, its output/finding).
func (p *Planner) Plan(ctx context.Context, taskID int64, as *db.AssetStore, ts *db.ExplorationStore, goal string, triggers []TriggerEvent, emit func(db.Activity)) (met bool, reason string, err error) {
	// cold-digest §2.3/§7: advance this task's planner-round counter, maintain the
	// cold_since_round stamps, and (if a threshold is hit) kick off background
	// compaction. Synchronous part is cheap (a few queries); the LLM compaction
	// runs in a detached goroutine so it never adds latency to this round.
	p.compactor.OnPlannerRound(ctx, ts)
	tsx := NewToolSet(ts, "planner")
	tsx.SetFindingRecorder(p.findingRecorder)
	if as != nil {
		tsx.SetAssetStore(as, as.Companies())
	}
	tsx.SetTaskID(taskID)
	tsx.SetCoverageEnabled(as == nil || as.CoverageEnabled(taskID))
	tsx.killWork = p.killWork   // enable kill_work tool (nil = unavailable)
	tsx.steerWork = p.steerWork // enable steer_work tool (nil = unavailable)
	if origin, _ := ts.OriginFactID(); origin > 0 {
		tsx.SetOwnerNode(origin) // planner-side anchors default to the task root (origin fact)
	}
	// Domain tools + basic default toolset (Read/Write/Edit/MultiEdit/LS/Glob/Grep/Bash)
	// When the asset coverage feature is off, remove add_task_scope/list_untested_assets (not included in prompt).
	base := append(tsx.DropCoverageTools(tsx.PlannerTools()), actool.DefaultTools()...)
	ctx = WithRunInfo(ctx, RunInfo{TaskID: taskID, ExplorationID: explorationID(ts)})
	tools, def, cleanup := AugmentTools(ctx, "planner", base)
	defer cleanup()
	// Key situational info (just-completed intents + prefetched full graph) moved to [this round's user input] (see input below), system
	// keeps only the static planning body. move-out keeps system stable each round, better for caching; the cost is if a single round gets long, situational info may
	// be compressed by compaction (planner rounds are usually short, low risk). situational will be concatenated into input below.
	situational := renderTriggers(ts, triggers) + renderGraphOverview(tsx.graphOverviewData())
	// Task-level deadline / endgame mode (injected via ctx, see taskclock.go). In the endgame round, the task timeout
	// planner wrap-up words are concatenated into this round's user input as [this round's operational instruction] (along with situational), so it only does the final
	// goal determination and produces no new intents.
	tc := taskClockFrom(ctx)
	if tc.Final {
		situational += "\n\n[Task endgame wrap-up (special instruction for this round, overrides the normal planning flow above)]: " + resolveTaskTimeoutWrapup("planner")
	}
	// This task's working directory <workDir>/tasks/<taskID>, create it first.
	taskDir := ensureRunDir(p.workDir, taskID, 0)
	ctx = intercept.WithReviewContext(ctx, taskDir, intercept.ReviewBackground{})
	sysBody := plannerSystem(goal, p.workDir, taskDir)
	if p.wantConstraints() {
		sysBody += constraintBlock(ts) // Inject operational constraints (if any) into the system prompt to frame exploration boundaries
	}
	system, boundary := deferredSystem(sysBody, def)
	// planner has no wall-clock budget of its own; when there is a deadline, clamp MaxDuration to the remaining time so that a running planning round,
	// when the task hits its deadline, enters wrap-up (timeout → task-timeout wording, step count → per-run wording).
	maxDur, clamped := clampMaxDuration(tc.DeadlineUnix, 0)
	settle := wrapupSettlement("planner", nil)
	if tc.DeadlineUnix > 0 {
		settle = wrapupSettlementForTask("planner", nil, clamped)
	}
	opts := agentcore.Options{
		Provider:        p.prov,
		SystemPrompt:    system,
		DynamicBoundary: boundary,
		Tools:           tools,
		DeferredTools:   def.Deferred,
		UnlockSet:       def.Unlock,
		PermissionMode:  permission.ModeBypass,
		EnableWebFetch:  true, // Go through the recording proxy for audit trail; load the proxy CA to verify MITM re-signed HTTPS certificates
		WebFetchProxy:   p.proxyAddr,
		WebFetchCACert:  p.proxyCACert,
		// Web search (optional). ddgs needs no key; brave-free needs BraveKey; tavily needs TavilyKey.
		// WebSearchProxy is an independent egress proxy (http/https/socks5), unrelated to the MITM proxy that records traffic; if empty, connect directly.
		EnableWebSearch:       p.webSearch.Enabled,
		WebSearchBackend:      p.webSearch.Backend,
		BraveSearchAPIKey:     p.webSearch.BraveKey,
		TavilySearchAPIKey:    p.webSearch.TavilyKey,
		DeepSeekSearchBaseURL: p.webSearch.DeepSeekBaseURL,
		DeepSeekSearchAPIKey:  p.webSearch.DeepSeekAPIKey,
		DeepSeekSearchModel:   p.webSearch.DeepSeekModel,
		WebSearchProxy:        p.webSearch.Proxy,
		BashEnv:               proxyEnv(p.proxyAddr, p.proxyCACert), // Bash subcommands go through the proxy by default + trust the CA
		WorkingDir:            taskDir,                              // Working directory for this task <workDir>/tasks/<taskID>
		ToolOutputDir:         cmdOutDir(taskDir),
		MaxTurns:              p.maxTurns, // 0 = unlimited (configurable in agent management)
		MaxDuration:           maxDur,     // 0=unlimited; with a deadline = time remaining until deadline
		Compaction:            compactionConfig(p.compactionWindow()),
		// Planning to-dos shared across wakeups: let the serial chain persist across rounds (the session is new, the store is not).
		Todos: p.todoFor(ts.ID()),
		// Hit the step budget for [this round] → SDK runs wrap-up: land the conclusions already thought through this round (dispatch the add_intent it should,
		// prove_goal it can prove, record the serial chain with TodoWrite), rather than stopping planning — the planner will still be woken repeatedly afterward.
		// When clamped (constrained by the task deadline), use PromptByReason instead (see wrapupSettlementForTask).
		Settlement:   settle,
		NonStreaming: p.nonStreaming(), // When this profile selects non-streaming, use Provider.Complete
		MaxTokens:    p.maxTokens(),    // 0 = no limit sent, determined by the server default
	}
	if p.tx != nil { // persist raw LLM conversation; one accumulating file per task's planner
		opts.Transcript = p.tx
		opts.SessionID = fmt.Sprintf("exp%d-planner", ts.ID())
	}
	// Experimental feature: when enabled, noa takes over context compaction (archives are centralized under <workDir>/noa/<SessionID>, persistent).
	noaSession := fmt.Sprintf("exp%d-planner", ts.ID())
	enableNoa(&opts, p.noaEnabledFn, p.workDir, noaSession, noaWarn(noaSession))
	// Situation (just-completed intents + full graph) is now assembled into this round's user input (see input below). The user also contains
	// instructions + cross-wakeup to-dos (todo is the model's own planning notes, regenerable, fine to put in user).
	// The opening line is split into two kinds based on 'whether this round has concrete changes': changes → point to the [Actual Changes] block below; no changes
	// (heartbeat periodic inspection / hint / recovery, etc.) → don't falsely claim 'the graph has changed'; instead hint to also re-check running intents.
	lead := "There were just concrete changes (see [Actual Changes That Triggered This Round] below), plan the next step based on them:"
	if len(triggers) == 0 {
		lead = "This round is a wakeup from a **periodic check (heartbeat fired) / no concrete change signal** — the graph may not have new changes. Also re-check running intents: for those with no progress for a long time or going off track, use steer_work to correct; for those entirely wrong in direction, use kill_work to cut losses; then judge the goal and decide whether to add direction:"
		// On heartbeat / no-change wakeups, if the whole graph has no open or running intents → exploration has stalled (no worker running,
		// and no queued direction). Explicitly inform the planner and force it to add new direction this round, don't just re-check running intents and spin for a round.
		if active, err := ts.HasActiveIntent(); err == nil && !active {
			lead = "This round is a wakeup from a **periodic check (heartbeat fired)**, and currently **there are no open or running intents at all** — no worker is running, and there is no queued direction, exploration has stalled. You **must** produce one or more new intents this round that advance toward the goal and are **mutually non-duplicate** with existing intents in the graph (must not produce 0 intents); first judge based on the situation below whether the goal has been achieved, and if not, immediately add direction:"
		}
	}
	input := lead + situational + "\n\nBased on the situation above, judge the goal. When the goal has been [truly achieved] (target outcome obtained / target vulnerability confirmed), mark each one with prove_goal. **Hard bottom line: as long as the goal has not yet been achieved and there are currently no open or running intents (frontier_open=0 and running_intents is empty), this round must produce at least one intent that advances toward the goal—at this point there is no running work to wait for and no queued direction, so producing 0 intents = task stall. Only when there are already open/running intents making progress, or the goal has been achieved, may this round produce no new intents.**" +
		renderPlannerTodos(opts.Todos.List())
	// MaxDuration now interrupts running tools when wall-clock time is up and proceeds to wrap-up in place (on the live ctx); a single round getting stuck no longer
	// bypasses wrap-up, with no need for an external hard ctx fallback. ctx only carries pause / kill / shutdown.
	_, _, err = captureRun(ctx, opts, input,
		func(r db.Activity) {
			if emit != nil {
				r.Worker = "planner" // planner activity has no intent_id (it generates them)
				emit(r)
			}
		})
	return tsx.GoalMet, tsx.Reason, err
}
