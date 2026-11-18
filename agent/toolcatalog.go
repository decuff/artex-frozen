package agent

import (
	"context"
	"encoding/json"

	actool "github.com/Autumn-27/norma/tool"
)

// This file turns 'built-in tools' from pure code into a catalog that is enumerable and can be overridden by the DB:
//   - BuiltinToolSeeds(): expands the built-in tool sets of the three execution agents into seed records (key +
//     description + parameter schema + default-bound agent), for the server to idempotently seed into the tools table at startup.
//   - ToolResolve hook: at runtime, based on the tools rows in the DB, performs 'filter by agent +
//     override description/schema + inject parameter defaults' on the assembled tools. key/handler remain at the code layer; the DB only changes 'prose and defaults'.
// handler (Call behavior) always comes from code—the DB cannot change it, only the descriptions and default inputs the model sees.

// ToolSeed is a seedable snapshot of a built-in tool: key is CoreTool.Name() (hard-bound to the handler,
// UI read-only), Desc/Schema come from the tool definition in code, Agents are which agents the code binds it to by default.
type ToolSeed struct {
	Key    string         // = CoreTool.Name(), primary key, immutable
	Desc   string         // top-level description (overridable in UI)
	Schema map[string]any // parameter JSON-Schema (structure read-only, description/default editable in UI)
	Agents []string       // default-bound agent keys (worker/planner/mainagent)
}

// builtinToolsByAgent constructs each execution agent's domain tool set using a 'read-only empty shell' ToolSet (nil stores).
// Tool constructors only stuff closures into the Spec and don't dereference stores at construction time, so nil is safe—
// these tools are only used here to read Name()/Description()/InputSchema(), never Call.
//
// Deliberately [excludes] the SDK generic tools actool.DefaultTools() (Read/Write/Edit/MultiEdit/LS/Glob/
// Grep/Bash): every agent always has them, there's no 'bound to whom' tradeoff, and most descriptions are in Prompt()
// (this table only overrides Description(), which would cause misleading partial overrides). Not seeded → no DB row → ToolResolve
// passes through as-is, no override, behavior same as before. Only artex's own domain tools are in the table and manageable.
func builtinToolsByAgent() map[string][]actool.CoreTool {
	ts := NewToolSet(nil, "")
	return map[string][]actool.CoreTool{
		"mainagent": ts.MainAgentTools(),
		"planner":   ts.PlannerTools(),
		"worker":    ts.WorkerTools(),
		// goals (goal decomposer) is bound by default to set_goals + set_constraints: via them it writes the decomposed goals and
		// extracted operational constraints into the DB. Shares the same managed tools with mainagent; the web side can edit descriptions/schema and check by agent.
		"goals": {ts.setGoals(), ts.setConstraints()},
		// auto is bound by default to vulnerability reporting + asset management tools; other domain tools can be checked on demand in the UI.
		// New DBs are written by this seed; old DBs are migrated by seedAutoDefaultBindings.
		"auto": {ts.addFinding(), ts.insertAssets(), ts.addCompanyScope(), ts.listAssets(), ts.listCompanies()},
		// pentest (standalone pentest agent) bound by default to: query asset / insert asset / report vuln / query vuln / query enterprise.
		// New DBs are written by this seed; old DBs are migrated by seedPentestDefaultBindings.
		"pentest": {ts.listAssets(), ts.insertAssets(), ts.addFinding(), ts.listFindings(), ts.listCompanies()},
	}
}

// defaultUnbound: these system tools are still listed in the catalog (visible on the web side, manually selectable per agent), but
// by default they are [not bound to any agent] — ToolResolve discards tools with empty bindings for all agents, so explicit opt-in is required.
// The reason they still remain in some agent's base tool set (e.g. goal_met in PlannerTools): first, it lets seed
// construct it to get desc/schema; second, after the user manually binds it back, it is in the runtime base and ToolResolve can keep it.
//
// goal_met: bypasses proving goals one by one and directly announces [the entire task complete] from the global level; it carries high weight and risk of misjudgment, and also
// duplicates 'prove_goal marks the last goal → automatic wrap-up', so by default it is not given to any agent; bind it manually when needed.
var defaultUnbound = map[string]bool{"goal_met": true}

// BuiltinToolSeeds deduplicates and merges each agent's built-in tool set into a seed list: tools with the same name (e.g. list_assets
// present in multiple agents) are merged into one entry, with Agents taking the union; tools in defaultUnbound are forced to have empty bindings.
func BuiltinToolSeeds() []ToolSeed {
	byAgent := builtinToolsByAgent()
	order := []string{"mainagent", "goals", "planner", "worker", "auto", "pentest"}

	type acc struct {
		tool   actool.CoreTool
		agents []string
	}
	m := map[string]*acc{}
	var keys []string
	for _, ak := range order {
		for _, t := range byAgent[ak] {
			a, ok := m[t.Name()]
			if !ok {
				a = &acc{tool: t}
				m[t.Name()] = a
				keys = append(keys, t.Name())
			}
			a.agents = append(a.agents, ak)
		}
	}

	out := make([]ToolSeed, 0, len(keys))
	for _, k := range keys {
		a := m[k]
		agents := a.agents
		if defaultUnbound[k] {
			agents = []string{} // listed in the catalog, manually bindable, but by default not given to any agent (stores [] rather than null, consistent with other tools)
		}
		out = append(out, ToolSeed{
			Key:    k,
			Desc:   a.tool.Description(),
			Schema: a.tool.InputSchema(),
			Agents: agents,
		})
	}
	return out
}

// ToolResolve, if set, post-processes an agent's fully-assembled tool list against
// the DB tools table: it drops tools not bound to this agent (or globally disabled)
// and wraps the rest so the model sees the DB-overridden description/schema and
// Default input parameters get injected. Tools with no matching DB row (MCP/skill/host tools like
// traffic) pass through untouched. nil = tools unchanged. Wired in server/assembly.go.
var ToolResolve func(ctx context.Context, agentKey string, tools []actool.CoreTool) []actool.CoreTool

// DecorateTool wraps t so Description()/InputSchema() report the DB overrides and
// Call() injects scalar parameter defaults (from schema's "default" props) whenever
// the model omitted them. Name/Prompt/permission/scheduler flags delegate to t, so
// the tool's identity and handler are unchanged. Empty desc/schema fall back to t's.
func DecorateTool(t actool.CoreTool, desc string, schema map[string]any) actool.CoreTool {
	if desc == "" {
		desc = t.Description()
	}
	if len(schema) == 0 {
		schema = t.InputSchema()
	}
	return &overriddenTool{CoreTool: t, desc: desc, schema: schema}
}

// overriddenTool is a CoreTool decorator: it embeds the original (so all behavioral
// methods — Prompt/IsReadOnly/IsConcurrencySafe/CheckPermissions/Name — delegate)
// and overrides only the model-facing description/schema plus default injection.
type overriddenTool struct {
	actool.CoreTool
	desc   string
	schema map[string]any
}

func (o *overriddenTool) Description() string         { return o.desc }
func (o *overriddenTool) InputSchema() map[string]any { return o.schema }

func (o *overriddenTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	return o.CoreTool.Call(ctx, injectDefaults(in, o.schema), tc)
}

// injectDefaults fills scalar parameter defaults declared in the (possibly edited)
// schema into the input JSON whenever the model omitted the field or left it empty/
// null. Structure (names/types/required) is untouched — only default values are merged in.
func injectDefaults(in json.RawMessage, schema map[string]any) json.RawMessage {
	defs := scalarDefaults(schema)
	if len(defs) == 0 {
		return in
	}
	m := map[string]json.RawMessage{}
	if len(in) > 0 {
		if err := json.Unmarshal(in, &m); err != nil {
			return in // non-object input: don't touch it
		}
	}
	changed := false
	for k, dv := range defs {
		if cur, ok := m[k]; !ok || isEmptyJSON(cur) {
			m[k] = dv
			changed = true
		}
	}
	if !changed {
		return in
	}
	b, err := json.Marshal(m)
	if err != nil {
		return in
	}
	return b
}

// scalarDefaults extracts properties[k]["default"] for scalar params (string/
// integer/number/boolean). Array/object defaults are skipped: merging them is
// ambiguous and not worth the surprise.
func scalarDefaults(schema map[string]any) map[string]json.RawMessage {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return nil
	}
	out := map[string]json.RawMessage{}
	for name, raw := range props {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		dv, ok := p["default"]
		if !ok || dv == nil {
			continue
		}
		switch p["type"] {
		case "string", "integer", "number", "boolean":
			if b, err := json.Marshal(dv); err == nil {
				out[name] = b
			}
		}
	}
	return out
}

func isEmptyJSON(raw json.RawMessage) bool {
	s := string(raw)
	return s == "null" || s == `""`
}
