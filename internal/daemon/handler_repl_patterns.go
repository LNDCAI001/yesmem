package daemon

import (
	"encoding/json"

	"github.com/LNDCAI001/yesmem/internal/storage"
)

func (h *Handler) handleRecordReplPattern(params map[string]any) Response {
	project, _ := params["project"].(string)
	shapeHash, _ := params["shape_hash"].(string)
	example, _ := params["example"].(string)
	matchedCap, _ := params["matched_cap"].(string)
	if project == "" || shapeHash == "" {
		return Response{Error: "project and shape_hash required"}
	}
	if err := h.store.RecordReplPattern(project, shapeHash, example, matchedCap); err != nil {
		return Response{Error: err.Error()}
	}
	b, _ := json.Marshal(map[string]any{"status": "ok"})
	return Response{Result: b}
}

func (h *Handler) handleGetReplPatternSuggestion(params map[string]any) Response {
	project, _ := params["project"].(string)
	if project == "" {
		return Response{Error: "project required"}
	}
	threshold := 3
	if t, ok := params["threshold"].(float64); ok && t > 0 {
		threshold = int(t)
	}

	result := map[string]any{}

	var p *storage.ReplPatternObservation
	var err error
	if rawCaps, ok := params["active_caps"]; ok {
		var activeCaps []string
		if arr, ok := rawCaps.([]any); ok {
			for _, c := range arr {
				if s, ok := c.(string); ok && s != "" {
					activeCaps = append(activeCaps, s)
				}
			}
		}
		p, err = h.store.GetReadyReplPatternSuggestionForActiveCaps(project, threshold, activeCaps)
	} else {
		p, err = h.store.GetReadyReplPatternSuggestion(project, threshold)
	}
	if err != nil {
		return Response{Error: err.Error()}
	}
	if p != nil {
		result["pattern"] = map[string]any{
			"id":                p.ID,
			"project":           p.Project,
			"shape_hash":        p.ShapeHash,
			"first_cmd_example": p.FirstCmdExample,
			"matched_cap":       p.MatchedCap,
			"count":             p.Count,
			"dismiss_count":     p.DismissCount,
			"first_seen":        p.FirstSeen,
			"last_seen":         p.LastSeen,
		}
	}

	workflows, err := h.store.GetWorkflowSuggestions(project, 3)
	if err == nil && len(workflows) > 0 {
		wf := workflows[0]
		result["workflow"] = map[string]any{
			"sequence_hash": wf.SequenceHash,
			"example_tools": wf.ExampleTools,
			"count":         wf.Count,
		}
	}

	if len(result) == 0 {
		return Response{Result: json.RawMessage("null")}
	}
	b, _ := json.Marshal(result)
	return Response{Result: b}
}

func (h *Handler) handleDismissReplPattern(params map[string]any) Response {
	project, _ := params["project"].(string)
	shapeHash, _ := params["shape_hash"].(string)
	if project == "" || shapeHash == "" {
		return Response{Error: "project and shape_hash required"}
	}
	if err := h.store.DismissReplPattern(project, shapeHash); err != nil {
		return Response{Error: err.Error()}
	}
	b, _ := json.Marshal(map[string]any{"status": "ok"})
	return Response{Result: b}
}

func (h *Handler) handleRecordTurnSequence(params map[string]any) Response {
	threadID, _ := params["thread_id"].(string)
	project, _ := params["project"].(string)
	turnHash, _ := params["turn_hash"].(string)
	exampleTools, _ := params["example_tools"].(string)
	if threadID == "" || project == "" || turnHash == "" {
		return Response{Error: "thread_id, project, and turn_hash required"}
	}
	if err := h.store.RecordTurnHash(threadID, project, turnHash, exampleTools); err != nil {
		return Response{Error: err.Error()}
	}
	b, _ := json.Marshal(map[string]any{"status": "ok"})
	return Response{Result: b}
}
