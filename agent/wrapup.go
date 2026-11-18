package agent

import (
	"strings"

	"github.com/Autumn-27/norma/harness"
)

// Wrap-up / settlement prompt: when the agent is terminated due to [MaxTurns exhausted] or
// [timeout (run_seconds/MaxDuration)], the SDK's settlement phase injects this prompt,
// letting the agent first persist content it has identified but not yet written back, then output a summary, avoiding a half-finished job.
//
// Each agent's wrap-up prompt can be overridden on demand in the backend (stored in agents.wrapup_prompt); if left empty, the built-in
// default here is used. Only the [prompt body] is editable; which tools are disabled and how many turns the wrap-up itself gets are fixed code policy.

// WrapupOverride, if set, returns the stored wrap-up prompt for an agent key and
// whether a non-empty one exists. Wired by the server to the agents table (like
// PromptOverride for system prompts). nil / empty → the built-in default is used.
var WrapupOverride func(agentKey string) (string, bool)

// WrapupMaxTurnsOverride, if set, returns the admin-configured turn budget for the
// wrap-up phase of an agent and whether a positive one exists. Wired to the agents
// table. nil / ≤0 → the built-in per-agent default (wrapupTurnDefaults) is used.
var WrapupMaxTurnsOverride func(agentKey string) (int, bool)

// Built-in default wrap-up prompts, indexed by agent key. The worker reuses the historically hardcoded settleWrapUpPrompt
// (defined in worker.go); planner/mainagent each have a version; unmatched ones (custom agents) use the generic fallback.
var wrapupDefaults = map[string]string{
	"worker":    settleWrapUpPrompt,
	"planner":   plannerWrapUpDefault,
	"mainagent": mainAgentWrapUpDefault,
}

// wrapupTurnDefaults: built-in default turn budgets for each agent's wrap-up phase [itself] (can be overridden in the backend if >0).
// All get 10 turns, ensuring the wrap-up phase has enough steps to persist. Unmatched ones use genericWrapupTurns.
var wrapupTurnDefaults = map[string]int{
	"worker":    10,
	"planner":   10,
	"mainagent": 10,
}

const genericWrapupTurns = 10

const plannerWrapUpDefault = "Your planned steps for this round are about to run out—note that only [this round] is ending; the system will still wake you again to continue planning as the situation changes, so this is not task termination, and you need not wrap up the entire plan here. Please land the conclusions you have already thought through this round, don't let this round be wasted, but also [don't force intents just to wrap up] (0 intents this round is still a perfectly normal result): (1) If you have determined an exploration direction that [should be dispatched right now], submit it in a batch with a single add_intent (don't hold back what you've thought of); (2) For goals already proven achieved by some finding/fact, call prove_goal to mark them met (don't miss any); (3) If you identify a serial exploitation chain that needs step-by-step execution, record it with TodoWrite so the next wake-up can continue dispatching. When done, end this round directly; no summary text needed."

const mainAgentWrapUpDefault = "Your steps are about to run out, and this interaction is ending. Do not initiate any new exploration/operations. Please **in a single sentence of plain text** summarize for the user the current progress, key conclusions, and suggested next steps."

const genericWrapUpDefault = "You are about to be terminated due to budget exhaustion. Please first write back the results you have completed but not yet persisted, then **in a single sentence of plain text** summarize what you did and what key conclusions you reached (this sentence will be shown as the result of this run)."

// WrapupDefault returns the built-in default wrap-up prompt for an agent key —
// used by the admin UI as the "restore default" value and empty-field placeholder.
func WrapupDefault(agentKey string) string {
	if d, ok := wrapupDefaults[agentKey]; ok {
		return d
	}
	return genericWrapUpDefault
}

// WrapupTurnsDefault returns the built-in wrap-up turn budget for an agent key —
// used by the admin UI as the "0 = default N" hint.
func WrapupTurnsDefault(agentKey string) int {
	if n, ok := wrapupTurnDefaults[agentKey]; ok {
		return n
	}
	return genericWrapupTurns
}

// resolveWrapup returns the effective wrap-up prompt: the DB override (if set and
// non-empty) over the built-in default.
func resolveWrapup(agentKey string) string {
	if WrapupOverride != nil {
		if t, ok := WrapupOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return WrapupDefault(agentKey)
}

// resolveWrapupTurns returns the effective wrap-up turn budget: a positive DB
// override over the built-in per-agent default.
func resolveWrapupTurns(agentKey string) int {
	if WrapupMaxTurnsOverride != nil {
		if v, ok := WrapupMaxTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return WrapupTurnsDefault(agentKey)
}

// wrapupSettlement builds the settlement config for an agent's run. Prompt and the
// turn budget are admin-editable per agent; disabled tools are code-owned policy so
// a user can't edit away the "stop probing" guardrail. Resolved fresh each run
// (reads DB live), so edits apply on the next run without a restart.
func wrapupSettlement(agentKey string, disabledTools []string) *harness.Settlement {
	return &harness.Settlement{
		Prompt:        resolveWrapup(agentKey),
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
}

// ---------- Task-level timeout wrap-up prompts (see docs/task-level-timeout-and-wrapup-design.md) ----------
//
// These are two separate sets from the per-run closing words: per-run is "your budget for this run is used up"; task timeout is
// "the whole task has hit its deadline and is about to end". The semantics are often opposite (especially for the planner: per-run says "don't stop, keep planning",
// while task timeout says "stop planning at the deadline and make the final decision"). Only configure for worker/planner.

// WrapupTaskTimeoutOverride / …TurnsOverride: DB overrides for task-timeout wrap-up prompts and turn counts
// (wired to agents.task_timeout_wrapup_prompt / _max_turns, worker/planner only).
var (
	WrapupTaskTimeoutOverride      func(agentKey string) (string, bool)
	WrapupTaskTimeoutTurnsOverride func(agentKey string) (int, bool)
)

var taskTimeoutWrapupDefaults = map[string]string{
	"worker":  workerTaskTimeoutDefault,
	"planner": plannerTaskTimeoutDefault,
}

const workerTaskTimeoutDefault = "**The entire task has reached its timeout limit and is about to end** (not your budget for this run, but the whole exploration hitting its deadline). This is the last chance: (1) Persist [all] the content you have identified but not yet written back—new assets via insert_assets, exploration conclusions/facts via record_fact, confirmed vulnerabilities via report_finding; (2) Do not start any new commands/probes; (3) **Finally, in a single sentence of plain text**, summarize your key conclusions on this intent."

const plannerTaskTimeoutDefault = "**The entire task has reached its timeout limit and is about to end** (not this round, but the whole task terminating). Based on the current [all] facts and findings, make the final goal judgment: for goals already proven achieved by evidence, call prove_goal to mark them met (don't miss any). **Do not generate any new intents** (intents dispatched now will no longer be executed). Wrap up once the judgment is done; no summary text needed."

// TaskTimeoutWrapupDefault returns the built-in default task-timeout wrap-up prompt for an agent (for backend placeholder/restore default).
func TaskTimeoutWrapupDefault(agentKey string) string {
	return taskTimeoutWrapupDefaults[agentKey] // not configured (mainagent/chat) returns empty string
}

// resolveTaskTimeoutWrapup: DB override (non-empty) > built-in default. Empty string means the agent has no task-timeout prompt
// (non worker/planner), in which case the caller should fall back to the per-run prompt.
func resolveTaskTimeoutWrapup(agentKey string) string {
	if WrapupTaskTimeoutOverride != nil {
		if t, ok := WrapupTaskTimeoutOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return TaskTimeoutWrapupDefault(agentKey)
}

func resolveTaskTimeoutTurns(agentKey string) int {
	if WrapupTaskTimeoutTurnsOverride != nil {
		if v, ok := WrapupTaskTimeoutTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return resolveWrapupTurns(agentKey) // defaults to reusing the per-run turn count
}

// wrapupSettlementForTask builds settlement for a worker/planner run that is aware
// of the task deadline. See §5 of the design doc:
//   - clamped=true  → this run is squeezed by the task deadline: because Timeout wrap-up = task hits its deadline → task-timeout wording;
//     because MaxTurns wrap-up = steps run out first within the squeeze window, task still has a few minutes left → fall back to per-run wording.
//   - clamped=false → task still has time: both reasons use per-run wording (i.e. degrades to wrapupSettlement).
//
// The PromptByReason handed to the harness picks on the spot by the [actual] reason at wrap-up; without build there is a mismatch.
func wrapupSettlementForTask(agentKey string, disabledTools []string, clamped bool) *harness.Settlement {
	perRun := resolveWrapup(agentKey)
	st := &harness.Settlement{
		Prompt:        perRun, // fallback (also the value for both reasons when not clamped)
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
	if clamped {
		if tt := resolveTaskTimeoutWrapup(agentKey); tt != "" {
			st.PromptByReason = map[harness.TerminalReason]string{
				harness.ReasonTimeout:  tt,     // task hits its deadline
				harness.ReasonMaxTurns: perRun, // steps run out first, task still has time left
			}
			st.MaxTurns = resolveTaskTimeoutTurns(agentKey)
		}
	}
	return st
}
