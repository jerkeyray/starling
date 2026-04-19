package step

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/internal/obs"
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
//
// Retry: set Idempotent: true and MaxAttempts > 1 to enable retry on
// transient failures (errors matching tool.ErrTransient via errors.Is).
// Each retry emits a fresh ToolCallScheduled{Attempt: n} before the
// call, and ToolCallCompleted/Failed{Attempt: n} after. Backoff
// controls the sleep between attempts; nil selects an exponential
// default (100ms base, doubling, cap 10s, 0–25% jitter). Non-idempotent
// calls run exactly once regardless of MaxAttempts. Callers should set
// Idempotent only for operations they're comfortable re-executing
// (pure reads, side-effect-free queries, or operations with caller-
// supplied idempotency keys).
type ToolCall struct {
	CallID      string
	TurnID      string
	Name        string
	Args        json.RawMessage
	Idempotent  bool
	MaxAttempts int
	Backoff     func(attempt int) time.Duration
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
// A per-tool watchdog ("timeout" classification) is tracked as future
// work.
//
// When ToolCall opts into retry (Idempotent + MaxAttempts>1), every
// attempt emits its own Scheduled + Completed/Failed pair carrying
// Attempt: n. Retry happens only on tool.ErrTransient; ctx errors and
// ErrToolNotFound are always terminal.
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

	return executeOne(ctx, c, call)
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
// Retry (per-call Idempotent + MaxAttempts>1) applies inside each
// worker. Only the attempt-1 Scheduled events are contiguous in the
// log; retry Scheduleds land interleaved with sibling Completeds by
// design, because a retry is contingent on the prior attempt's Failed.
//
// Under ModeReplay, CallTools does NOT fan out — it executes tools
// sequentially in the order their final Completed/Failed events
// appear in the recording so the re-emitted payloads land at the same
// seq as the original run. Byte-for-byte divergence surfaces as
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
	// mutex serializes the seq increment; this also means attempt-1
	// Scheduled events are contiguous in the log — workers can't
	// interleave their Completed/Failed with a peer's first Scheduled.
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

// executeOne runs a single tool assuming its attempt-1 ToolCallScheduled
// has already been emitted, and emits the matching Completed/Failed
// outcome. For retries it emits fresh Scheduled{Attempt: n} events
// before each retry. Shared between CallTools workers, the replay-order
// sequential dispatch, and CallTool. Safe for concurrent use (emit()
// serializes via Context.mu; runToolSafely has no shared mutable state).
func executeOne(ctx context.Context, c *Context, call ToolCall) (json.RawMessage, error) {
	attempts := 1
	if call.Idempotent && call.MaxAttempts > 1 {
		attempts = call.MaxAttempts
	}
	backoffFn := call.Backoff
	if backoffFn == nil {
		backoffFn = defaultBackoff
	}

	for attempt := 1; attempt <= attempts; attempt++ {
		// Attempt-1 Scheduled is emitted by the caller (CallTool /
		// CallTools). For retries we emit a fresh Scheduled before the
		// call so the log carries one Scheduled per attempt.
		if attempt > 1 {
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
				Attempt:  uint32(attempt),
			}); err != nil {
				return nil, fmt.Errorf("step.CallTool(%q): emit Scheduled: %w", call.Name, err)
			}
		}

		reg := c.registry()
		if reg == nil {
			return nil, emitToolFailed(ctx, c, call.CallID, ErrToolNotFound, "tool", 0, uint32(attempt))
		}
		tl, ok := reg.Get(call.Name)
		if !ok {
			wrapped := fmt.Errorf("%w: %s", ErrToolNotFound, call.Name)
			return nil, emitToolFailed(ctx, c, call.CallID, wrapped, "tool", 0, uint32(attempt))
		}

		start := time.Now()
		result, execErr := runToolSafely(ctx, tl, call.Args)
		durMs := ReplayDurationMs(ctx, time.Since(start).Milliseconds())

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
				Attempt:    uint32(attempt),
			}); err != nil {
				return result, fmt.Errorf("step.CallTool(%q): emit Completed: %w", call.Name, err)
			}
			return result, nil
		}

		errType := classifyToolError(ctx, execErr)

		// Terminal iff we can't retry. ctx errors (incl. cancellation
		// and deadline) are never retried; ErrToolNotFound never is;
		// non-transient errors never are; and the final attempt is
		// always terminal.
		retryable := errors.Is(execErr, tool.ErrTransient) &&
			!errors.Is(execErr, ErrToolNotFound) &&
			ctx.Err() == nil
		if !retryable || attempt == attempts {
			return nil, emitToolFailed(ctx, c, call.CallID, execErr, errType, durMs, uint32(attempt))
		}

		// Not terminal: emit Failed for this attempt, sleep backoff,
		// loop. In replay mode the sleep is skipped — the recorded
		// event stream dictates ordering.
		if emitErr := emit(ctx, c, event.KindToolCallFailed, event.ToolCallFailed{
			CallID:     call.CallID,
			Error:      execErr.Error(),
			ErrorType:  errType,
			DurationMs: durMs,
			Attempt:    uint32(attempt),
		}); emitErr != nil {
			return nil, errors.Join(fmt.Errorf("step.CallTool: emit Failed: %w", emitErr), execErr)
		}
		c.logger.Warn("tool transient failure, retrying",
			obs.AttrToolName, call.Name,
			obs.AttrCallID, call.CallID,
			obs.AttrAttempt, attempt,
			"err", execErr.Error())
		if c.mode != ModeReplay {
			select {
			case <-time.After(backoffFn(attempt)):
			case <-ctx.Done():
				// ctx cancelled mid-backoff. The last Failed already
				// captured the transient error; surface ctx.Err so the
				// caller treats this the same as any other cancellation.
				return nil, ctx.Err()
			}
		}
	}
	// Unreachable: the loop always returns via success, terminal
	// failure, or ctx cancellation.
	return nil, fmt.Errorf("step.CallTool(%q): retry loop exhausted without result", call.Name)
}

// defaultBackoff is used when ToolCall.Backoff is nil: exponential
// 100ms, 200ms, 400ms, ... capped at 10s, with 0–25% additive jitter.
// The jitter is non-deterministic but only runs in live mode; replay
// skips the sleep entirely so replay stays byte-stable.
func defaultBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	base := 100 * time.Millisecond
	// Shift safely up to attempt-1 = 6 (6.4s); larger attempts clamp.
	shift := attempt - 1
	if shift > 6 {
		shift = 6
	}
	d := base << shift
	if d > 10*time.Second {
		d = 10 * time.Second
	}
	jitter := time.Duration(rand.Int63n(int64(d / 4)))
	return d + jitter
}

// replayCompletionOrder returns the indices into calls in the order
// their FINAL Completed/Failed events appear in the recorded log,
// starting from the current chain cursor. With retry, a CallID can
// have multiple Failed events followed by a Completed or a final
// Failed — we pick the last outcome per CallID so replay's emit order
// matches the live run's completion order.
//
// Returns an ErrReplayMismatch-wrapped error when the recording
// doesn't contain an outcome for every CallID in calls (i.e. the
// caller diverged by adding tool calls the live run didn't make).
func replayCompletionOrder(c *Context, calls []ToolCall) ([]int, error) {
	idxByCallID := make(map[string]int, len(calls))
	for i := range calls {
		idxByCallID[calls[i].CallID] = i
	}

	c.mu.Lock()
	start := int(c.nextSeq - 1)
	recorded := c.recorded
	c.mu.Unlock()

	lastIdx := make(map[string]int, len(calls))
	for i := start; i < len(recorded); i++ {
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
		if _, ok := idxByCallID[callID]; !ok {
			return nil, fmt.Errorf("%w: recorded completion for CallID %q not in current batch", ErrReplayMismatch, callID)
		}
		lastIdx[callID] = i
	}
	if len(lastIdx) != len(calls) {
		return nil, fmt.Errorf("%w: expected %d tool outcomes in recording, found %d", ErrReplayMismatch, len(calls), len(lastIdx))
	}

	// Build ordering: sort CallIDs by their final-outcome index.
	type pair struct {
		idx, last int
	}
	pairs := make([]pair, 0, len(calls))
	for cid, li := range lastIdx {
		pairs = append(pairs, pair{idx: idxByCallID[cid], last: li})
	}
	// Insertion sort — len is bounded by tool-call fanout (typically <16).
	for i := 1; i < len(pairs); i++ {
		for j := i; j > 0 && pairs[j-1].last > pairs[j].last; j-- {
			pairs[j-1], pairs[j] = pairs[j], pairs[j-1]
		}
	}
	order := make([]int, len(pairs))
	for i, p := range pairs {
		order[i] = p.idx
	}
	return order, nil
}

// emitToolFailed emits the Failed event and returns the underlying
// error (so CallTool's caller gets the wrapped error, not a
// log-emission error unless the emit itself failed).
func emitToolFailed(ctx context.Context, c *Context, callID string, execErr error, errType string, durMs int64, attempt uint32) error {
	if emitErr := emit(ctx, c, event.KindToolCallFailed, event.ToolCallFailed{
		CallID:     callID,
		Error:      execErr.Error(),
		ErrorType:  errType,
		DurationMs: durMs,
		Attempt:    attempt,
	}); emitErr != nil {
		// errors.Join preserves both chains so errors.Is(err, ErrToolNotFound)
		// (or any other sentinel inside execErr) still routes even when the
		// log emit itself failed.
		return errors.Join(fmt.Errorf("step.CallTool: emit Failed: %w", emitErr), execErr)
	}
	return execErr
}
