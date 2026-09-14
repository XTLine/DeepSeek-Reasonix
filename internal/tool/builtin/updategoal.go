package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	goaldomain "reasonix/internal/goal"
	"reasonix/internal/tool"
)

func init() { tool.RegisterBuiltin(updateGoal{}) }

type updateGoal struct{}

func (updateGoal) Name() string { return "update_goal" }
func (updateGoal) Description() string {
	return "Update the exact current goal revision. edit, pause, and resume require current direct-human authority; complete and blocked are also allowed during the exact autonomous goal round. There is no continue action: leaving an active goal unchanged continues it automatically."
}
func (updateGoal) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"goal_id":{"type":"string","minLength":1},"revision":{"type":"integer","minimum":1},"action":{"type":"string","enum":["edit","pause","resume","complete","blocked"]},"objective":{"type":"string","minLength":1,"description":"Replacement objective; valid only for edit."},"max_goal_rounds":{"anyOf":[{"type":"integer","minimum":1},{"type":"null"}],"description":"Replacement limit for edit; null removes the limit."},"blocked_reason":{"type":"string","minLength":1,"description":"Concrete blocker; required only for blocked."}},"required":["goal_id","revision","action"]}`)
}
func (updateGoal) ReadOnly() bool { return false }
func (updateGoal) ProviderVisible(ctx context.Context) bool {
	_, ok := tool.GoalLifecycleFromContext(ctx)
	return ok
}
func (updateGoal) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if strings.Contains(string(args), `"status"`) && !strings.Contains(string(args), `"action"`) {
		return "", fmt.Errorf("legacy update_goal protocol is unsupported; call get_goal, then use goal_id, revision, and action; leaving an active goal unchanged continues automatically")
	}
	var input struct {
		GoalID        string             `json:"goal_id"`
		Revision      uint64             `json:"revision"`
		Action        tool.GoalAction    `json:"action"`
		Objective     *string            `json:"objective"`
		Limit         optionalRoundLimit `json:"max_goal_rounds"`
		BlockedReason *string            `json:"blocked_reason"`
	}
	if err := decodeGoalArgs(args, &input, "update_goal"); err != nil {
		return "", err
	}
	input.GoalID = strings.TrimSpace(input.GoalID)
	if input.GoalID == "" || input.Revision == 0 {
		return "", fmt.Errorf("goal_id and a positive revision are required")
	}
	request := tool.GoalUpdateRequest{Ref: goaldomain.Ref{ID: input.GoalID, Revision: input.Revision}, Action: input.Action}
	switch input.Action {
	case tool.GoalActionEdit:
		if input.BlockedReason != nil || (input.Objective == nil && !input.Limit.Present) {
			return "", fmt.Errorf("edit requires objective and/or max_goal_rounds and does not accept blocked_reason")
		}
		if input.Objective != nil {
			value, err := trimmedRequired(*input.Objective, "objective")
			if err != nil {
				return "", err
			}
			request.Objective = &value
		}
		if input.Limit.Present {
			limit, err := parseRoundLimit(input.Limit.Raw)
			if err != nil {
				return "", err
			}
			request.MaxGoalRounds = goaldomain.RoundLimitChange{Set: true, Value: limit}
		}
	case tool.GoalActionBlocked:
		if input.Objective != nil || input.Limit.Present || input.BlockedReason == nil {
			return "", fmt.Errorf("blocked requires blocked_reason and does not accept edit fields")
		}
		message, err := trimmedRequired(*input.BlockedReason, "blocked_reason")
		if err != nil {
			return "", err
		}
		request.BlockedReason = &goaldomain.BlockReason{Code: "model-blocked", Message: message}
	case tool.GoalActionPause, tool.GoalActionResume, tool.GoalActionComplete:
		if input.Objective != nil || input.Limit.Present || input.BlockedReason != nil {
			return "", fmt.Errorf("%s does not accept objective, max_goal_rounds, or blocked_reason", input.Action)
		}
	default:
		return "", fmt.Errorf("action must be one of edit|pause|resume|complete|blocked")
	}
	binding, err := goalBinding(ctx)
	if err != nil {
		return "", err
	}
	view, err := binding.Owner.UpdateGoal(ctx, request, binding.Authority)
	if err != nil {
		return "", goalToolError("update_goal", err)
	}
	instruction := ""
	if view.Phase == goaldomain.PhaseComplete || view.Phase == goaldomain.PhaseBlocked {
		instruction = "Finish the current turn with an accurate final summary for the user; no further automatic goal round will be admitted."
	}
	return goalToolResultWithInstruction(&view, instruction)
}
