package agent

import (
	"context"
	"errors"
	"fmt"
)

// AbortCause names why an agent run's context was cancelled. Every cancellation
// site should attach one so the activity trace can report the real initiator.
type AbortCause struct {
	Code  string
	Short string
	Text  string
}

func (c *AbortCause) Error() string { return c.Text }

func cause(code, short, text string) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: text}
}

// Causef builds a cause that includes runtime-specific detail.
func Causef(code, short, format string, args ...any) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: fmt.Sprintf(format, args...)}
}

var (
	// Task-level execution context.
	AbortPausedByUser = cause("paused_by_user", "The user paused the task",
		"The user paused the task via the task control API (POST /api/tasks/{id}/control, action=pause). This Planner/Worker run was actively cancelled; in-progress intents will return to frontier(open) and be re-claimed and executed from scratch after the task is resumed")
	AbortPausedByOrchestrator = cause("paused_by_orchestrator", "The orchestrator Agent paused the task",
		"The orchestrator Agent called the pause_task tool to pause this task. This Planner/Worker run was actively cancelled; in-progress intents will return to frontier(open) and be re-executed after resuming")
	AbortTaskDeleted = cause("task_deleted", "The task was deleted",
		"The task is being deleted (DELETE /api/tasks/{id}); the deletion barrier has cancelled the Planner, Worker, and main Agent currently running for this task; the results of this run will no longer be used")
	AbortPausedOnReload = cause("paused_on_reload", "The backend restored the task's paused state",
		"On startup, the backend restored the task's paused state based on the state persisted in the database. This run was cancelled; under normal circumstances there are no running Agents during the restore phase")
	AbortGoalMet = cause("goal_met", "The planner determined the task goal has been achieved",
		"The planner determined the task goal has been achieved and set the task to done, then cancelled the still-running Workers; these intents will be marked as stopped rather than failed")
	AbortSettleDrainTimeout = cause("settle_drain_timeout", "The wait time for the task's timeout settle-up has been exhausted",
		"After the task reached timeout, it waited for the running Workers to gracefully settle up, but the 90-second drain grace period was still insufficient, so a hard cancel was executed; intents will be marked as exhausted, and facts and assets already written during the settle-up phase will be retained")

	// Per-work context.
	AbortKilledByPlanner = cause("killed_by_planner", "The planner terminated this intent",
		"The planner called kill_work to actively terminate this intent, usually indicating the direction has gone off track or there is no further value in continuing; the intent will be marked as stopped and will not be automatically re-claimed")
	AbortWorkPausedByUser = cause("work_paused_by_user", "The user paused this Worker intent",
		"The user paused the running Worker. This invocation was cancelled and the intent turned to paused; all registered intents, facts, gaps, and activity records are retained, and execution restarts from scratch after resuming")
	AbortWorkCancelledByUser = cause("work_cancelled_by_user", "The user deleted this Worker intent",
		"The user deleted the running Worker. This invocation was cancelled; after the Worker exits the write zone, the server handles this intent according to the deletion mode chosen by the user—soft delete only marks it as deleted and retains all outputs, while hard delete cascades to remove this intent and downstream nodes supported solely by it")
	AbortWorkFinished = cause("work_finished", "The Worker finished normally and released its context",
		"The Worker finished normally, and the engine released its context resources in detachWork. This is not a run interruption; if it appears in an interruption message, it means a race occurred between the cancellation and the wrap-up event")
	AbortPausedRaceGuard = cause("paused_race_guard", "Refuse to start a new run while the task is paused",
		"When the task is in a paused state, the engine refuses to issue a new execution context, to prevent a race between claim and pause from causing the Worker to keep starting; already-claimed intents will return to frontier")

	// Main Agent and standalone conversation contexts.
	AbortChatStoppedByUser = cause("chat_stopped_by_user", "The user stopped this round of conversation",
		"The user clicked stop, actively aborting this round of the main Agent or conversation Agent run. Activity records already produced will be retained, and the next message can still be sent")
	AbortChatPausedWithTask = cause("chat_paused_with_task", "The task was paused and the main Agent conversation was aborted",
		"When the user pauses the task, the running main Agent conversation is also canceled synchronously. Activity records already generated are retained; after resuming the task, this round of messages will not be automatically replayed")
	AbortChatTurnFinished = cause("chat_turn_finished", "This round of conversation ended normally and the context has been released",
		"This round of conversation ended normally, and the server is releasing the context resources for this round. This is not a run interruption; if it appears in an interruption message, it means a race occurred between the cancellation and the finishing event")

	// Process-level and per-run hard backstop.
	AbortShutdown = cause("shutdown", "The backend process is shutting down",
		"The backend process received SIGINT or SIGTERM and is restarting, updating, or shutting down. All running Agents will be canceled; after restart, leftover running intents will be reset to open and re-executed")
	AbortRunHardTimeout = cause("run_hard_timeout", "The hard timeout fallback for a single run has been triggered",
		"A single run exceeded the soft wall-clock budget plus the additional grace period, meaning a model request or some tool did not return for a long time, preventing the normal turn-boundary wrap-up from executing. Please focus on checking the last tool call that did not return before the interruption")
)

// AbortReason resolves the named cause attached to a cancelled run context.
func AbortReason(ctx context.Context) (code, short, text string, ok bool) {
	c := context.Cause(ctx)
	if c == nil {
		return "", "", "", false
	}
	var ac *AbortCause
	if errors.As(c, &ac) {
		return ac.Code, ac.Short, ac.Text, true
	}
	switch {
	case errors.Is(c, context.DeadlineExceeded):
		return "deadline_exceeded", "The upstream context reached its deadline",
			"The upstream context reached its deadline, but the setter did not attach a named cause via WithTimeoutCause: " + c.Error(), true
	case errors.Is(c, context.Canceled):
		return "canceled_no_cause", "The canceling party did not attach a named cause",
			"The upstream context was canceled, but the canceling party did not attach a named cause via context.WithCancelCause; please register the cause in agent/cancelcause.go and wire it into this cancellation point", true
	default:
		return "other", firstLine(c.Error(), 80), c.Error(), true
	}
}
