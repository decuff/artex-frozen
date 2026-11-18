package agent

import (
	"encoding/json"
	"fmt"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

const findingIDGuidance = "\n\n**Vulnerability ID conventions**: finding_id is the independent vulnerability record ID; finding_node_id is the exploration node ID. The id in list_findings / list_task_findings / node_detail / get_task_node_detail remains the exploration node ID; the independent ID should be read from the finding_id in the same response. get_finding_traffic / bind_finding_traffic use the independent finding_id. The finding_id parameter of the legacy update_finding_report still takes finding_node_id. Do not use the number on the first line of report_finding for evidence tools, and do not guess other numbers after encountering an ID error."

// The server supplies the persisted setting. A missing setting/host is off.
// Consulted at assembly and again on writes so an already-running session
// cannot keep binding after the user switches the feature off.
var FindingTrafficBindingEnabled func() bool

func findingTrafficBindingEnabled() bool {
	return FindingTrafficBindingEnabled != nil && FindingTrafficBindingEnabled()
}

// Applied after ToolResolve: user descriptions and prompts remain intact, while
// all actual reporters (including Planner and custom chat agents) see the same
// API contract. Disabled/unbound tools are never reintroduced here.
func findingWorkflowTools(agentKey string, tools []actool.CoreTool) ([]actool.CoreTool, string) {
	if !findingTrafficBindingEnabled() {
		out := make([]actool.CoreTool, 0, len(tools))
		for _, tool := range tools {
			if tool.Name() == "bind_finding_traffic" {
				continue
			}
			if agentKey == "reporter" && (tool.Name() == "traffic_search" || tool.Name() == "traffic_get" || tool.Name() == "traffic_blob") {
				continue
			}
			switch tool.Name() {
			case "report_finding", "add_hint", "add_task_hint":
				// Work on a copy: toggling back on must restore the original schema.
				raw, _ := json.Marshal(tool.InputSchema())
				var schema map[string]any
				if json.Unmarshal(raw, &schema) == nil {
					stripTrafficParameters(schema)
					tool = DecorateTool(tool, tool.Description(), schema)
				}
			}
			out = append(out, tool)
		}
		return out, ""
	}
	out := append([]actool.CoreTool(nil), tools...)
	has := map[string]bool{}
	for i, tool := range out {
		has[tool.Name()] = true
		note := ""
		switch tool.Name() {
		case "report_finding":
			note = "\nBy default, the report Agent verifies and binds traffic before writing the report. The reporter should keep the verification command, key output, existing real traffic IDs and their purposes in evidence for the report Agent to verify against the execution record; no extra packet lookup is needed for binding. Explicit immediate binding is also supported: traffic_refs or evidence_hint_id can submit verified references; the latter reads the structured references of the hint specified for this task; if either is invalid, the entire report fails. TCP/no-packet cases do not need these optional parameters. The returned finding_id and finding_node_id represent the independent record and the exploration node respectively."
		case "add_hint", "add_task_hint":
			note = "\nWhen handing off a confirmed vulnerability, keep the IDs, purposes, notes and order of verified traffic in the traffic_refs of the corresponding hint (single entry at top level, batch in the corresponding hints element), and explain in text which specific vulnerability it proves. The caller must not hand off only text and discard existing traffic references. Unverified candidates cannot be passed as evidence."
		case "get_finding_traffic", "bind_finding_traffic", "list_findings", "list_task_findings", "node_detail", "get_task_node_detail", "update_finding_report":
			note = findingIDGuidance
		}
		if note != "" {
			out[i] = DecorateTool(tool, tool.Description()+note, tool.InputSchema())
		}
	}
	guidance := ""
	if has["report_finding"] || has["add_task_hint"] || has["add_hint"] {
		guidance = "\n\n**Traffic evidence handoff (optional)**: automatic binding is by default completed by the report Agent after the vulnerability is stored and before the report is written. The reporter should keep the verification command, key output, existing real traffic IDs and their purposes in evidence, and include intent_id in the task so the report Agent can trace it; no extra packet lookup is needed for binding. When Auto / Planner reports on behalf, do not discard the executor's existing references. add_hint / add_task_hint can hand off via traffic_refs; explicit immediate binding is still supported via report_finding's traffic_refs / evidence_hint_id. For TCP or no-packet cases, register normally; do not guess IDs, and do not re-probe just to fill in packets."
		if has["add_task_hint"] && !has["add_hint"] {
			guidance += "\nWhen the platform conversation has no task context, do not call report_finding directly; hand off to the existing corresponding task via add_task_hint, let the task Agent register it, and verify the result with list_task_findings."
		}
		if has["prove_goal"] || has["goal_met"] {
			guidance += "\nBefore determining the objective is complete, first finish reporting/handing off the existing evidence from this run. Do not end the task or cancel the Worker merely because the textual vulnerability has been registered while evidence handoff is still incomplete; no-packet cases do not require waiting or forcing a packet capture."
		}
	}
	if has["update_finding_report"] && has["bind_finding_traffic"] && has["get_finding_traffic"] {
		guidance += "\n\n**Auto-associate traffic before reporting (enabled)**: you are responsible for verifying and binding traffic for the vulnerability triggered in this run, then writing the report. First obtain the explicit finding_id and finding_node_id from the JSON returned by report_finding or from get_task_node_detail / list_task_findings. Read the vulnerability details, the execution record of the corresponding intent, and the existing evidence list, preferring the real IDs handed off by the reporter. If this verification is HTTP and traffic tools are available, use traffic_search to filter candidates, then use traffic_get to verify one by one that the request/response actually supports the vulnerability; domain and time are only for filtering and do not prove attribution. Associate the confirmed evidence in reproduction order with bind_finding_traffic(finding_id, traffic_refs), choosing baseline / proof / verification / supporting and explaining the purpose. Only operate on this vulnerability; do not create duplicate vulnerabilities or re-probe the target. After successful binding, call get_finding_traffic again to get the latest version, read the required body, then pass the actually read version as evidence_version to update_finding_report (its finding_id parameter still uses finding_node_id). Existing bindings need not be appended again. For TCP, uncollected, tools unavailable, or no exact match, skip automatic binding, write the report normally based on textual/command evidence and explain why; do not guess just to fill in traffic. Do not claim success when binding fails; keep the existing evidence and explain the reason for not binding in the report."
	}
	if guidance != "" || has["get_finding_traffic"] || has["update_finding_report"] {
		guidance += findingIDGuidance
	}
	return out, guidance
}

func stripTrafficParameters(schema map[string]any) {
	props, _ := schema["properties"].(map[string]any)
	delete(props, "traffic_refs")
	delete(props, "evidence_hint_id")
	if required, ok := schema["required"].([]any); ok {
		kept := required[:0]
		for _, key := range required {
			if key != "traffic_refs" && key != "evidence_hint_id" {
				kept = append(kept, key)
			}
		}
		schema["required"] = kept
	}
	if hints, ok := props["hints"].(map[string]any); ok {
		if items, ok := hints["items"].(map[string]any); ok {
			stripTrafficParameters(items)
		}
	}
}

// HintTrafficSchema is shared by the task-local and cross-task hint tools.
func HintTrafficSchema() map[string]any {
	return map[string]any{"type": "array", "description": "Optional: verified traffic references corresponding to the specific vulnerability in this hint, order preserved; after handoff, report_finding can pass evidence_hint_id to carry these references.", "items": obj(map[string]any{"traffic_id": str("real traffic ID"), "role": str("baseline / proof / verification / supporting"), "note": str("what conclusion this traffic supports")}, "traffic_id")}
}

func (t *ToolSet) findingRefsFromHint(hintID int64, explicit []db.TrafficRef) ([]db.TrafficRef, error) {
	if hintID <= 0 {
		return db.NormalizeTrafficRefs(explicit)
	}
	n, err := t.ts.GetNode(hintID) // local store only: inherited hints cannot supply evidence
	if err != nil {
		return nil, err
	}
	if n == nil || n.Kind != db.KindHint {
		return nil, fmt.Errorf("evidence_hint_id=%d must be a hint node of this task (inherited hints cannot be used directly for binding)", hintID)
	}
	var payload struct {
		Refs []db.TrafficRef `json:"traffic_refs"`
	}
	if err := json.Unmarshal(n.Payload, &payload); err != nil {
		return nil, err
	}
	return db.NormalizeTrafficRefs(append(append([]db.TrafficRef{}, explicit...), payload.Refs...))
}
