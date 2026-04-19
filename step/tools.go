package step

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/tool"
)

// ToolCall describes a single tool invocation the caller wants the
// runtime to execute. The CallID + TurnID pair links the invocation
// back to the AssistantMessageCompleted that planned it (so replay
// can match Scheduled/Completed events against the turn that caused
// them).
//
// Callers typically populate ToolCall from a provider.ToolUse returned
// by LLMCall. CallID empty => a ULID is minted; TurnID is not defaulted
// because a missing TurnID is almost always a caller bug.
type ToolCall struct {
	CallID string
	TurnID string
	Name   string
	Args   json.RawMessage
}

// CallTool invokes the named tool against the Registry configured on
// ctx's step.Context, emitting ToolCallScheduled before the call and
// either ToolCallCompleted or ToolCallFailed after.
//
// Error classification into the event's ErrorType field:
//   - tool panic                              → "panic"  (err wraps tool.ErrPanicked)
//   - context cancelled / deadline exceeded   → "cancelled"
//   - tool not in registry                    → "tool"   (err is ErrToolNotFound)
//   - any other error returned by the tool    → "tool"
//
// M3's watchdog will add "timeout".
//
// Panics if ctx has no step.Context attached.
func CallTool(ctx context.Context, call ToolCall) (json.RawMessage, error) {
	c := mustFrom(ctx, "CallTool")

	if call.CallID == "" {
		call.CallID = newULID()
	}

	argsRaw := call.Args
	if len(argsRaw) == 0 {
		argsRaw = json.RawMessage("{}")
	}
	argsCBOR, err := jsonToCanonicalCBOR(argsRaw)
	if err != nil {
		return nil, fmt.Errorf("step.CallTool(%q): encode args: %w", call.Name, err)
	}

	if err := emit(ctx, c, event.KindToolCallScheduled, event.ToolCallScheduled{
		CallID:   call.CallID,
		TurnID:   call.TurnID,
		ToolName: call.Name,
		Args:     argsCBOR,
		Attempt:  1,
	}); err != nil {
		return nil, fmt.Errorf("step.CallTool(%q): emit Scheduled: %w", call.Name, err)
	}

	reg := c.registry()
	if reg == nil {
		return nil, emitToolFailed(ctx, c, call.CallID, ErrToolNotFound, "tool", 0)
	}
	tl, ok := reg.Get(call.Name)
	if !ok {
		wrapped := fmt.Errorf("%w: %s", ErrToolNotFound, call.Name)
		return nil, emitToolFailed(ctx, c, call.CallID, wrapped, "tool", 0)
	}

	start := time.Now()
	result, execErr := runToolSafely(ctx, tl, call.Args)
	durMs := time.Since(start).Milliseconds()

	if execErr == nil {
		resultRaw := result
		if len(resultRaw) == 0 {
			resultRaw = json.RawMessage("null")
		}
		resultCBOR, cerr := jsonToCanonicalCBOR(resultRaw)
		if cerr != nil {
			return result, fmt.Errorf("step.CallTool(%q): encode result: %w", call.Name, cerr)
		}
		if err := emit(ctx, c, event.KindToolCallCompleted, event.ToolCallCompleted{
			CallID:     call.CallID,
			Result:     resultCBOR,
			DurationMs: durMs,
			Attempt:    1,
		}); err != nil {
			return result, fmt.Errorf("step.CallTool(%q): emit Completed: %w", call.Name, err)
		}
		return result, nil
	}

	errType := classifyToolError(ctx, execErr)
	return nil, emitToolFailed(ctx, c, call.CallID, execErr, errType, durMs)
}

// runToolSafely invokes tl.Execute and converts a panic into an error
// wrapping tool.ErrPanicked. Tools built via tool.Typed already recover
// internally, but we belt-and-suspenders it here so custom Tool
// implementations can't take down the agent loop.
func runToolSafely(ctx context.Context, tl tool.Tool, input json.RawMessage) (out json.RawMessage, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: %v", tool.ErrPanicked, r)
			out = nil
		}
	}()
	// Tools are expected to accept any JSON input; pass empty object if
	// the caller provided nothing so Execute signatures stay uniform.
	if len(input) == 0 {
		input = json.RawMessage("{}")
	}
	return tl.Execute(ctx, input)
}

// classifyToolError maps an execErr into the canonical ErrorType set.
func classifyToolError(ctx context.Context, execErr error) string {
	if errors.Is(execErr, tool.ErrPanicked) {
		return "panic"
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		// Either the tool returned ctx.Err directly or it wrapped a
		// cancellation error. Either way, classify as cancelled.
		if errors.Is(execErr, context.Canceled) ||
			errors.Is(execErr, context.DeadlineExceeded) ||
			errors.Is(execErr, ctxErr) {
			return "cancelled"
		}
	}
	return "tool"
}

// emitToolFailed emits the Failed event and returns the underlying
// error (so CallTool's caller gets the wrapped error, not a
// log-emission error unless the emit itself failed).
func emitToolFailed(ctx context.Context, c *Context, callID string, execErr error, errType string, durMs int64) error {
	if emitErr := emit(ctx, c, event.KindToolCallFailed, event.ToolCallFailed{
		CallID:     callID,
		Error:      execErr.Error(),
		ErrorType:  errType,
		DurationMs: durMs,
		Attempt:    1,
	}); emitErr != nil {
		// errors.Join preserves both chains so errors.Is(err, ErrToolNotFound)
		// (or any other sentinel inside execErr) still routes even when the
		// log emit itself failed.
		return errors.Join(fmt.Errorf("step.CallTool: emit Failed: %w", emitErr), execErr)
	}
	return execErr
}
