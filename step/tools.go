package step

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/tool"
)

// ToolResult carries the outcome of a single tool invocation from
// CallTools. Result holds the tool's JSON output on success (nil on
// failure). Err is the error returned by the tool, the registry, or a
// panic — classified and emitted into the log in the usual way. The
// caller is expected to treat each ToolResult independently (one
// failing tool does not cancel siblings).
type ToolResult struct {
	CallID string
	Result json.RawMessage
	Err    error
}

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

// CallTools dispatches a batch of tool calls, emitting ToolCallScheduled
// for every call in input order up front, then running the tools
// concurrently with a semaphore (cap = Config.MaxParallelTools or
// DefaultMaxParallelTools when zero). Each tool's completion emits
// ToolCallCompleted or ToolCallFailed as it finishes, so seq numbers
// reflect actual completion order — that ordering is the committed
// ground truth the hash chain ratifies.
//
// A failing tool does not cancel siblings; its error surfaces in the
// matching ToolResult.Err. Callers decide whether to keep going. The
// returned slice preserves input order (NOT completion order), so
// callers can correlate results with the calls they supplied.
//
// Under ModeReplay, CallTools does NOT fan out — it executes tools
// sequentially in the order their Completed/Failed events appear in
// the recording so the re-emitted payloads land at the same seq as
// the original run. Byte-for-byte divergence surfaces as
// ErrReplayMismatch from the underlying emit.
//
// Panics if ctx has no step.Context attached.
func CallTools(ctx context.Context, calls []ToolCall) ([]ToolResult, error) {
	c := mustFrom(ctx, "CallTools")
	if len(calls) == 0 {
		return nil, nil
	}

	// 1. Pre-mint any missing CallIDs so all Scheduled events carry a
	// stable ID before the first worker starts (live mode). In replay
	// mode CallIDs always arrive populated from the recorded provider;
	// a missing one would mean the caller diverged from the recording.
	for i := range calls {
		if calls[i].CallID == "" {
			calls[i].CallID = newULID()
		}
	}

	// 2. Emit ToolCallScheduled for every call, in input order. emit()'s
	// mutex serializes the seq increment; this also means Scheduled
	// events are contiguous in the log — workers can't interleave their
	// Completed/Failed with a peer's Scheduled.
	for i := range calls {
		argsRaw := calls[i].Args
		if len(argsRaw) == 0 {
			argsRaw = json.RawMessage("{}")
		}
		argsCBOR, err := jsonToCanonicalCBOR(argsRaw)
		if err != nil {
			return nil, fmt.Errorf("step.CallTools(%q): encode args: %w", calls[i].Name, err)
		}
		if err := emit(ctx, c, event.KindToolCallScheduled, event.ToolCallScheduled{
			CallID:   calls[i].CallID,
			TurnID:   calls[i].TurnID,
			ToolName: calls[i].Name,
			Args:     argsCBOR,
			Attempt:  1,
		}); err != nil {
			return nil, fmt.Errorf("step.CallTools(%q): emit Scheduled: %w", calls[i].Name, err)
		}
	}

	results := make([]ToolResult, len(calls))
	for i := range calls {
		results[i].CallID = calls[i].CallID
	}

	// 3. Replay branch: dispatch sequentially in recorded completion
	// order. Every emit()'s payload will byte-match the recording;
	// any drift surfaces as ErrReplayMismatch.
	if c.mode == ModeReplay {
		order, err := replayCompletionOrder(c, calls)
		if err != nil {
			return nil, fmt.Errorf("step.CallTools: %w", err)
		}
		for _, idx := range order {
			res, cerr := executeOne(ctx, c, calls[idx])
			results[idx].Result = res
			results[idx].Err = cerr
		}
		return results, nil
	}

	// 4. Live fan-out. Buffered channel as a counting semaphore. We use
	// a WaitGroup rather than errgroup because per-call errors do not
	// cancel siblings — each error is collected into its ToolResult.
	cap := c.maxParallelTools
	if cap <= 0 {
		cap = DefaultMaxParallelTools
	}
	sem := make(chan struct{}, cap)
	var wg sync.WaitGroup
	wg.Add(len(calls))
	for i := range calls {
		i := i
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res, cerr := executeOne(ctx, c, calls[i])
			results[i].Result = res
			results[i].Err = cerr
		}()
	}
	wg.Wait()
	return results, nil
}

// executeOne runs a single tool assuming its ToolCallScheduled has
// already been emitted, and emits the matching Completed/Failed
// outcome. Shared between CallTools workers and the replay-order
// sequential dispatch. Safe for concurrent use (emit() serializes via
// Context.mu; runToolSafely has no shared mutable state).
func executeOne(ctx context.Context, c *Context, call ToolCall) (json.RawMessage, error) {
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
			return result, fmt.Errorf("step.CallTools(%q): encode result: %w", call.Name, cerr)
		}
		if err := emit(ctx, c, event.KindToolCallCompleted, event.ToolCallCompleted{
			CallID:     call.CallID,
			Result:     resultCBOR,
			DurationMs: durMs,
			Attempt:    1,
		}); err != nil {
			return result, fmt.Errorf("step.CallTools(%q): emit Completed: %w", call.Name, err)
		}
		return result, nil
	}

	errType := classifyToolError(ctx, execErr)
	return nil, emitToolFailed(ctx, c, call.CallID, execErr, errType, durMs)
}

// replayCompletionOrder returns the indices into calls in the order
// their Completed/Failed events appear in the recorded log, starting
// from the current chain cursor. Returns an ErrReplayMismatch-wrapped
// error when the recording doesn't contain an outcome for every
// CallID in calls (i.e. the caller diverged by adding tool calls the
// live run didn't make).
func replayCompletionOrder(c *Context, calls []ToolCall) ([]int, error) {
	idxByCallID := make(map[string]int, len(calls))
	for i := range calls {
		idxByCallID[calls[i].CallID] = i
	}

	c.mu.Lock()
	start := int(c.nextSeq - 1)
	recorded := c.recorded
	c.mu.Unlock()

	order := make([]int, 0, len(calls))
	seen := make(map[string]struct{}, len(calls))
	for i := start; i < len(recorded) && len(order) < len(calls); i++ {
		ev := recorded[i]
		var callID string
		switch ev.Kind {
		case event.KindToolCallCompleted:
			tcc, err := ev.AsToolCallCompleted()
			if err != nil {
				return nil, fmt.Errorf("%w: decode ToolCallCompleted at seq=%d: %v", ErrReplayMismatch, ev.Seq, err)
			}
			callID = tcc.CallID
		case event.KindToolCallFailed:
			tcf, err := ev.AsToolCallFailed()
			if err != nil {
				return nil, fmt.Errorf("%w: decode ToolCallFailed at seq=%d: %v", ErrReplayMismatch, ev.Seq, err)
			}
			callID = tcf.CallID
		default:
			continue
		}
		idx, ok := idxByCallID[callID]
		if !ok {
			return nil, fmt.Errorf("%w: recorded completion for CallID %q not in current batch", ErrReplayMismatch, callID)
		}
		if _, dup := seen[callID]; dup {
			continue
		}
		seen[callID] = struct{}{}
		order = append(order, idx)
	}
	if len(order) != len(calls) {
		return nil, fmt.Errorf("%w: expected %d tool outcomes in recording, found %d", ErrReplayMismatch, len(calls), len(order))
	}
	return order, nil
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
