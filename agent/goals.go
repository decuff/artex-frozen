package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// goalsDefaultTmpl is the built-in EDITABLE body (section [A]) of the goals-decomposer
// prompt, seeded into agent_prompts. No template vars are used today.
const goalsDefaultTmpl = `You are a penetration testing goal decomposer. Your job is to identify from the user input the **final results to be achieved**, not to plan attack steps.

**Step one (do this before splitting goals): extract operational constraints**
From the 'task goal / task description', identify the operator's explicit rules about [what operations can and cannot be done], and call set_constraints to register them one by one (if the description and goal do not involve operational constraints, you may skip extracting operational constraints):
- type=deny: forbidden operations (e.g. 'do not scan ports', 'must not perform write/delete operations on the production environment', 'no brute forcing', 'do not touch a certain subdomain').
- type=allow: explicitly allowed/limited operation scope (e.g. 'only passive reconnaissance allowed', 'only target a certain domain').
- A constraint != a goal, and != an attack step: it is a rule about the boundaries of operational behavior.
- **Constraints must be [self-contained and hard-code the specific target]**: replace referential words like 'current target/current port/current IP/current domain/this site' with the **specific values** from the task goal/description. Constraints are injected separately into the execution-phase prompt, and once out of context, referential words cannot be resolved to whom they refer.
  Example: if the target is https://abc.example.net -> write 'only allow testing abc.example.net' instead of 'only allow testing the current target'; 'only test target port 443, do not scan other ports' instead of 'only test the current port'. If the original text only says 'current target' but the target address is already clear, fill in the address.
- **Only register constraints [explicitly written or emphasized] in the goal/description; never fabricate**; when unsure of the type, use deny (more conservative).
- If the goal/description truly contains no operational constraints, then **do not** call set_constraints.
After registering constraints (if any), proceed with the goal splitting below.

**Goal = final deliverable/verifiable result**

**Content that is NOT a goal (forbidden to list as sub-goals)**:
- Information gathering, reconnaissance, endpoint scanning
- Vulnerability analysis and verification process
- Attack steps, exploitation methods
- Result verification steps

**Splitting principles**:
- The user describes only one final goal -> output one
- There are multiple **mutually independent** final deliverables -> list them separately
- Annotate vulnclass for those that correspond to a clear vulnerability class; leave blank for information-gathering/business-logic goals
- Never fabricate goals the user did not mention

Call set_goals to submit the results.`

// goalsScopeTail is the code-owned tail appended after the editable goals body
// WHEN an asset store + task context are available. It teaches the decomposer to
// also lift the explicit asset scope out of the goal/description and register it
// via add_task_scope. Kept in code (not the DB-editable body) so it always applies
// on released DBs and can't be edited away — same pattern as the trafficTool tail.
const goalsScopeTail = `

**Additional responsibility: register the scope of testing assets**
Besides the split targets, you must also identify from the 'task goal / task description' the **explicitly given test asset scope**, and call add_task_scope to register it (the authorization boundary of this task, and also the denominator of asset test coverage). **Minimum scope principle: only register the one target the user explicitly points to, never expand it on your own.**
- If the target is a URL or an address with a hostname (such as https://xxx.example.com/path, app.example.com) → take its **full hostname**, kind=subdomain, value=full hostname.
  Example: target https://a1b2c3.lab.example.net/path → kind=subdomain, value=a1b2c3.lab.example.net (**not** example.net).
  **Strictly forbidden** to collapse a hostname with a subdomain into the root domain—seeing xxx.example.com and registering the whole example.com will expand the scope beyond the user's target, violating the principle of least scope.
- Only when the user gives a **bare root domain with no subdomain at all** (e.g., writing example.com directly), or explicitly says 'the whole site / all subdomains / the entire domain' → then use kind=root_domain, value=example.com.
- Pure IP or subnet → kind=ip / cidr, value=IP or CIDR.
- **Do not** register the company scope (company) — the task has just been created and this company usually does not exist yet in the asset system, so it cannot be registered; the company-level scope is handled by the subsequent plan stage.
Other rules:
- Only register the scope **explicitly written in the target/description**; strictly forbidden to fabricate or infer unmentioned domains/IPs.
- reason: briefly state which sentence the basis comes from, for auditability.
- If there is no explicit asset scope in the goal/description, then **do not** call add_task_scope.
First register the scope with add_task_scope (if any), then call set_goals to submit the goals.`

// GoalSpec is one decomposed objective.
type GoalSpec struct {
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass,omitempty"`
}

// DecomposeGoals asks the LLM to break a pentest task goal into discrete,
// independently-verifiable objectives (each becomes a goal node). Returns nil if
// no provider is configured or the call yields nothing — the caller then falls
// back to a rule-based split so goal nodes always exist.
//
// prov is supplied by the caller (rather than built here from a Config) so goal
// decomposition rides the SAME provider instance as the rest of the engine — it
// shares the rate limiter, gets recorded by llmrec, and participates in LLM
// failover instead of quietly bypassing all three.
//
// desc is the task's free-text description (background: target range/flag count/engagement instructions, etc.).
// It is fed alongside the goal so the decomposer no longer splits blind — the
// prompt still forbids inventing anything the two texts don't state.
//
// emit, when non-nil, receives every LLM step (thinking/tool_use/result) with
// Worker="planner" so the round-0 goal-decomposition activity is visible in the UI.
//
// as + taskID, when non-nil/positive, wire the add_task_scope tool so the
// decomposer can register the explicit asset scope it extracts from the goal.
//
// ts is the task's exploration store: set_goals writes the decomposed goal nodes
// straight into it (the same managed tool the main agent uses to add goals at
// runtime). The returned specs are read back from the store so callers can emit
// per-goal activity and detect the "LLM produced nothing" case for their fallback.
func DecomposeGoals(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	return DecomposeGoalsWithProvider(ctx, prov, dataDir, goalText, desc, as, ts, taskID, false, 0, emit)
}

// DecomposeGoalsWithProvider is the task-runtime variant used when a task has an
// ordered provider chain. It preserves the same tools and write behavior while
// letting the caller own provider selection/failover. maxTokens is the profile's
// per-reply output cap (0 = send none).
func DecomposeGoalsWithProvider(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, nonStreaming bool, maxTokens int, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	// Goal decomposition is a one-shot call: no transcript store is attached, so agentcore will not attach it to ctx
	// session id (it is only attached when there is a writer, see agentcore.Prompt). And by the session-id header
	// Gateway that does prompt caching/sticky routing (opencode zen returns 400 directly if x-opencode-session is missing
	// MissingSessionID) reads this value from ctx — if not filled in, it's 'conversation works, breakdown 400'.
	// Explicitly attach a stable id: split requests of the same exploration share it (helps hit the cache), and the naming matches
	// planner/worker do not conflict and can be correctly attributed by llmrec.parseSession.
	if ts != nil {
		ctx = transcript.WithSessionID(ctx, fmt.Sprintf("exp%d-goals", ts.ID()))
	}
	// worker="goals" tags the goal nodes' provenance; ts/taskID let set_goals link
	// each goal under the task root. This is the catalog's real set_goals tool, so a
	// web-edited description/schema on it applies here too.
	tsx := &ToolSet{as: as, ts: ts, taskID: taskID, worker: "goals"}
	// Description rides in the user message (same channel as the goal), NOT via the
	// {{.EngagementDescription}} template var — else a prompt that references the var
	// would inject the description twice. System prompt stays pure static instructions.
	sys := renderSystem("goals", goalsDefaultTmpl, GoalsVars{DataDir: dataDir, Now: nowStr()})
	// set_constraints is always available (does not depend on the asset store): the main text already includes the step 'first extract operation constraints, then split goals'
	// (wording can be changed on the agent edit page), here you just need to hook up the tool.
	tools := []actool.CoreTool{tsx.setGoals(), tsx.setConstraints()}
	// Wire add_task_scope only when we have a real asset store + task to write to.
	// The scope-extraction tail is appended in lockstep so the prompt never asks for
	// a tool that isn't present.
	if as != nil && taskID > 0 {
		tools = append(tools, tsx.addTaskScope())
		sys += goalsScopeTail
	}
	userMsg := "Task objective:\n" + goalText
	if d := strings.TrimSpace(desc); d != "" {
		userMsg += "\n\nTask description (background information, may include target scope/flag count/engagement notes; for reference only, do not fabricate content not mentioned therein):\n" + d
	}
	// Use captureRun so every LLM step is emitted as an activity record (visible in
	// the plan tab under the round-0 marker). Falls back gracefully when emit is nil.
	captureEmit := func(r db.Activity) {
		if emit != nil {
			r.Worker = "planner"
			emit(r)
		}
	}
	captureRun(ctx, agentcore.Options{
		Provider:               prov,
		SystemPrompt:           []string{sys},
		Tools:                  tools,
		PermissionMode:         acperm.ModeBypass,
		DisableBackgroundTasks: true,
		// The 3 steps (extract constraints → register scope → break down goals) each require one tool call; give enough turns to avoid missing the set_goals call before wrapping up.
		MaxTurns:     8,
		NonStreaming: nonStreaming, // When this profile selects non-streaming, it uses Provider.Complete
		MaxTokens:    maxTokens,    // 0 = no limit sent, determined by server default
	}, userMsg, captureEmit)
	// set_goals persisted the goals directly; read them back so the caller sees what
	// was written (empty slice ⇒ the LLM produced nothing ⇒ caller falls back).
	if ts == nil {
		return nil
	}
	nodes, _ := ts.ListByKind(db.KindGoal, 10000)
	var out []GoalSpec
	for _, n := range nodes {
		var p struct {
			Text      string `json:"text"`
			VulnClass string `json:"vulnclass"`
		}
		_ = json.Unmarshal(n.Payload, &p)
		if strings.TrimSpace(p.Text) != "" {
			out = append(out, GoalSpec{Text: p.Text, VulnClass: p.VulnClass})
		}
	}
	return out
}
