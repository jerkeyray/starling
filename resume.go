package starling

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/internal/cborenc"
	"github.com/jerkeyray/starling/internal/obs"
	"github.com/jerkeyray/starling/provider"
	"github.com/jerkeyray/starling/step"
	"go.opentelemetry.io/otel/trace"
)

// ResumeOption tunes Resume / ResumeWith behavior. See WithReissueTools.
type ResumeOption func(*resumeConfig)

type resumeConfig struct {
	reissueTools bool
}

func defaultResumeConfig() resumeConfig {
	return resumeConfig{reissueTools: true}
}

// WithReissueTools controls whether Resume re-runs tool calls that
// were scheduled but never completed. Defaults to true. Set false
// for tools that mutate external state and should fail loudly
// (ErrPartialToolCall) instead of silently retrying. Re-issued
// calls get fresh CallIDs.
func WithReissueTools(b bool) ResumeOption {
	return func(c *resumeConfig) { c.reissueTools = b }
}

// Resume continues a previously-started run from its last recorded
// event. If extraMessage is non-empty it is appended as a user turn
// before the loop resumes. A terminal event is always emitted before
// return.
//
// Returns:
//   - ErrRunNotFound: runID not in a.Log.
//   - ErrRunAlreadyTerminal: last event is terminal.
//   - ErrSchemaVersionMismatch: RunStarted schema unknown.
//   - ErrPartialToolCall: unpaired ToolCallScheduled with
//     WithReissueTools(false).
//   - ErrRunInUse: chain advanced between tail read and first append.
//
// Budget: MaxWallClock and step-level token/USD caps reset at the
// process boundary; MaxTurns counts across the whole run. See
// docs/RESUME.md.
func (a *Agent) Resume(ctx context.Context, runID, extraMessage string) (*RunResult, error) {
	return a.ResumeWith(ctx, runID, extraMessage)
}

// ResumeWith is Resume with options. Resume(ctx, id, msg) is
// equivalent to ResumeWith(ctx, id, msg).
func (a *Agent) ResumeWith(ctx context.Context, runID, extraMessage string, opts ...ResumeOption) (*RunResult, error) {
	if err := a.validate(); err != nil {
		return nil, err
	}
	if !a.Config.SkipSchemaCheck {
		if err := eventlog.Preflight(ctx, a.Log); err != nil {
			return nil, err
		}
	}
	if runID == "" {
		return nil, fmt.Errorf("starling: Resume: runID is empty")
	}
	if a.Namespace != "" && !strings.HasPrefix(runID, a.Namespace+"/") {
		return nil, fmt.Errorf("starling: Resume: runID %q does not match agent namespace %q", runID, a.Namespace)
	}

	cfg := defaultResumeConfig()
	for _, opt := range opts {
		opt(&cfg)
	}

	// 1. Load events.
	events, err := a.Log.Read(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("starling: Resume: read log: %w", err)
	}
	if len(events) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrRunNotFound, runID)
	}

	// 2. Terminal check.
	last := events[len(events)-1]
	if last.Kind.IsTerminal() {
		return nil, fmt.Errorf("%w: %s ended with %s", ErrRunAlreadyTerminal, runID, last.Kind)
	}

	// 3. Reconstruct state.
	state, err := reconstructState(events)
	if err != nil {
		return nil, err
	}

	// 4. Partial-tool-call policy.
	if len(state.PendingCalls) > 0 && !cfg.reissueTools {
		return nil, fmt.Errorf("%w: %d pending call(s) at seq=%d", ErrPartialToolCall, len(state.PendingCalls), last.Seq)
	}

	startWall := time.Now()

	// Wall-clock budget: wrap ctx for the resumed portion only.
	if a.Budget != nil && a.Budget.MaxWallClock > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, startWall.Add(a.Budget.MaxWallClock))
		defer cancel()
	}

	logger := obs.Resolve(a.Config.Logger).With(obs.AttrRunID, runID)

	ctx, runSpan := obs.StartRunSpan(ctx, runID)
	defer runSpan.End()

	// 5. Build a fresh step.Context seeded onto the existing chain.
	lastEnc, err := event.Marshal(last)
	if err != nil {
		return nil, fmt.Errorf("starling: Resume: hash tail event: %w", err)
	}
	stepCfg := step.Config{
		Log:                    a.Log,
		RunID:                  runID,
		Provider:               a.Provider,
		Tools:                  step.NewRegistry(a.Tools...),
		Budget:                 budgetStepConfig(a.Budget),
		Logger:                 logger,
		ResumeFromSeq:          last.Seq,
		ResumeFromPrevHash:     event.Hash(lastEnc),
		RequireRawResponseHash: a.Config.RequireRawResponseHash,
		EmitTimeout:            a.Config.EmitTimeout,
	}
	stepCtx, err := step.NewContext(stepCfg)
	if err != nil {
		return nil, fmt.Errorf("starling: Resume: build step.Context: %w", err)
	}
	ctx = step.WithContext(ctx, stepCtx)

	logger.Info("run resumed",
		"at_seq", last.Seq,
		"turns_so_far", state.TurnCount,
		"pending_calls", len(state.PendingCalls),
		"extra_message", extraMessage != "",
	)

	// 6. Emit RunResumed marker. A rejection here means another writer
	// advanced the chain between our Read and our first Append —
	// surface as ErrRunInUse.
	if err := stepEmit(ctx, stepCtx, event.KindRunResumed, event.RunResumed{
		AtSeq:        last.Seq,
		ExtraMessage: extraMessage,
		ReissueTools: cfg.reissueTools,
		PendingCalls: len(state.PendingCalls),
	}); err != nil {
		if errors.Is(err, eventlog.ErrInvalidAppend) {
			return nil, fmt.Errorf("%w: %v", ErrRunInUse, err)
		}
		return nil, fmt.Errorf("starling: Resume: emit RunResumed: %w", err)
	}

	// 7. Optional extra user message.
	if extraMessage != "" {
		if err := stepEmit(ctx, stepCtx, event.KindUserMessageAppended, event.UserMessageAppended{
			Content: extraMessage,
		}); err != nil {
			if errors.Is(err, eventlog.ErrInvalidAppend) {
				return nil, fmt.Errorf("%w: %v", ErrRunInUse, err)
			}
			return nil, fmt.Errorf("starling: Resume: emit UserMessageAppended: %w", err)
		}
		state.Msgs = append(state.Msgs, provider.Message{
			Role:    provider.RoleUser,
			Content: extraMessage,
		})
	}

	// 8. Re-issue pending tool calls. Mint fresh CallIDs and rewrite
	// the orphan IDs in the replayed assistant message so the provider
	// sees one consistent set of IDs across tool_use and tool_result.
	if len(state.PendingCalls) > 0 {
		idMap := make(map[string]string, len(state.PendingCalls))
		for i, pc := range state.PendingCalls {
			fresh := step.NewCallID()
			idMap[pc.CallID] = fresh
			state.PendingCalls[i].CallID = fresh
		}
		rewriteAssistantToolUseIDs(state.Msgs, idMap)

		for _, pc := range state.PendingCalls {
			result, cerr := step.CallTool(ctx, step.ToolCall{
				CallID: pc.CallID,
				TurnID: state.LastTurnID,
				Name:   pc.Name,
				Args:   pc.Args,
			})
			if cerr != nil {
				if errors.Is(cerr, eventlog.ErrInvalidAppend) {
					return nil, fmt.Errorf("%w: %v", ErrRunInUse, cerr)
				}
				if errors.Is(cerr, context.Canceled) || errors.Is(cerr, context.DeadlineExceeded) {
					// Enter runLoop with the error path so the terminal
					// event matches what a live Run would have emitted.
					return a.runLoop(ctx, stepCtx, runSpan, logger, startWall, state.Msgs, state.TurnCount)
				}
				return a.runLoopWithPreset(ctx, stepCtx, runSpan, logger, startWall,
					&ToolError{Name: pc.Name, CallID: pc.CallID, Err: cerr})
			}
			state.Msgs = append(state.Msgs, provider.Message{
				Role: provider.RoleTool,
				ToolResult: &provider.ToolResult{
					CallID:  pc.CallID,
					Content: string(result),
				},
			})
		}
	}

	// 9. Enter the normal loop.
	return a.runLoop(ctx, stepCtx, runSpan, logger, startWall, state.Msgs, state.TurnCount)
}

// runLoopWithPreset is runLoop with a pre-supplied runErr: used when a
// reissued tool call fails before the react loop starts, so the
// terminal event reflects the true cause. Skips react entirely.
func (a *Agent) runLoopWithPreset(
	ctx context.Context,
	stepCtx *step.Context,
	runSpan trace.Span,
	logger *slog.Logger,
	startWall time.Time,
	runErr error,
) (*RunResult, error) {
	runID := stepCtx.RunID()
	obs.SetSpanError(runSpan, runErr)

	term, terr := a.emitTerminal(ctx, stepCtx, runErr, startWall)
	if terr != nil {
		if runErr != nil {
			return nil, runErr
		}
		return nil, terr
	}

	termAttrs := []any{obs.AttrKind, term.String(), obs.AttrDurMs, time.Since(startWall).Milliseconds()}
	switch term {
	case event.KindRunFailed:
		logger.Error("run failed", append(termAttrs, "err", errString(runErr))...)
	case event.KindRunCancelled:
		logger.Info("run cancelled", termAttrs...)
	default:
		logger.Info("run completed", termAttrs...)
	}

	result, readErr := a.buildResult(runID, startWall, term)
	if readErr != nil {
		if runErr != nil {
			return result, errors.Join(runErr, readErr)
		}
		return result, readErr
	}
	return result, runErr
}

// resumeState is the conversation-level view reconstructed from a
// run's event stream. Built once at the top of Resume and handed to
// the loop.
type resumeState struct {
	Goal         string             // from RunStarted
	Msgs         []provider.Message // rebuilt chat history
	TurnCount    int                // number of TurnStarted events seen
	PendingCalls []provider.ToolUse // unpaired ToolCallScheduled (by CallID)
	LastTurnID   string             // TurnID of the most recent turn, for re-issue events
	SchemaVer    uint32
}

// reconstructState walks events once and returns the resumeState. The
// switch default is an explicit error: when a new event kind is added
// to the package, Resume must be updated or the kind marked
// irrelevant here.
func reconstructState(events []event.Event) (*resumeState, error) {
	if len(events) == 0 {
		return nil, fmt.Errorf("starling: reconstructState: empty events")
	}
	rs, err := events[0].AsRunStarted()
	if err != nil {
		return nil, fmt.Errorf("starling: reconstructState: first event is not RunStarted: %w", err)
	}
	if rs.SchemaVersion != event.SchemaVersion {
		return nil, fmt.Errorf("%w: run was written with schema=%d, this binary understands %d",
			ErrSchemaVersionMismatch, rs.SchemaVersion, event.SchemaVersion)
	}

	state := &resumeState{
		Goal:      rs.Goal,
		SchemaVer: rs.SchemaVersion,
		Msgs:      []provider.Message{{Role: provider.RoleUser, Content: rs.Goal}},
	}

	// Pending tool calls keyed by CallID so Completed/Failed can drop
	// them in any order. Ordered separately via a parallel slice so the
	// output preserves insertion order (the model cares about call
	// ordering at prompt time).
	pendingByID := make(map[string]provider.ToolUse)
	var pendingOrder []string

	// Tracks calls scheduled in the current turn. In normal flow the
	// event order is TurnStarted → AssistantMessageCompleted →
	// ToolCallScheduled → ToolCallCompleted, so these entries get
	// drained by ToolCallCompleted/Failed in-turn. Any survivor at
	// end-of-stream is an orphan from a mid-turn crash; promoted to
	// pending in the post-loop sweep below. TurnStarted clears the
	// map so a fresh turn starts clean.
	currentTurnScheduled := make(map[string]provider.ToolUse)
	var currentTurnScheduledOrder []string

	resetCurrent := func() {
		currentTurnScheduled = make(map[string]provider.ToolUse)
		currentTurnScheduledOrder = currentTurnScheduledOrder[:0]
	}

	for _, ev := range events[1:] {
		switch ev.Kind {
		case event.KindUserMessageAppended:
			p, err := ev.AsUserMessageAppended()
			if err != nil {
				return nil, fmt.Errorf("decode UserMessageAppended at seq=%d: %w", ev.Seq, err)
			}
			state.Msgs = append(state.Msgs, provider.Message{
				Role:    provider.RoleUser,
				Content: p.Content,
			})

		case event.KindTurnStarted:
			p, err := ev.AsTurnStarted()
			if err != nil {
				return nil, fmt.Errorf("decode TurnStarted at seq=%d: %w", ev.Seq, err)
			}
			state.TurnCount++
			state.LastTurnID = p.TurnID
			resetCurrent()

		case event.KindAssistantMessageCompleted:
			p, err := ev.AsAssistantMessageCompleted()
			if err != nil {
				return nil, fmt.Errorf("decode AssistantMessageCompleted at seq=%d: %w", ev.Seq, err)
			}
			// Flush assistant message with the tool uses it planned.
			uses := make([]provider.ToolUse, len(p.ToolUses))
			for i, tu := range p.ToolUses {
				uses[i] = provider.ToolUse{
					CallID: tu.CallID,
					Name:   tu.ToolName,
					Args:   []byte(tu.Args),
				}
			}
			state.Msgs = append(state.Msgs, provider.Message{
				Role:     provider.RoleAssistant,
				Content:  p.Text,
				ToolUses: uses,
			})

		case event.KindToolCallScheduled:
			p, err := ev.AsToolCallScheduled()
			if err != nil {
				return nil, fmt.Errorf("decode ToolCallScheduled at seq=%d: %w", ev.Seq, err)
			}
			// Args are stored as CBOR on the wire; convert back to
			// JSON for provider.ToolUse (which models what the model
			// emitted originally — JSON).
			argsJSON, err := cborToJSON(p.Args)
			if err != nil {
				return nil, fmt.Errorf("convert ToolCallScheduled.Args to JSON at seq=%d: %w", ev.Seq, err)
			}
			tu := provider.ToolUse{
				CallID: p.CallID,
				Name:   p.ToolName,
				Args:   argsJSON,
			}
			if _, seen := currentTurnScheduled[p.CallID]; !seen {
				currentTurnScheduled[p.CallID] = tu
				currentTurnScheduledOrder = append(currentTurnScheduledOrder, p.CallID)
			}

		case event.KindToolCallCompleted:
			p, err := ev.AsToolCallCompleted()
			if err != nil {
				return nil, fmt.Errorf("decode ToolCallCompleted at seq=%d: %w", ev.Seq, err)
			}
			// Result is stored as canonical CBOR; convert back to JSON
			// so provider.ToolResult.Content carries the same shape the
			// live agent path emits (json.RawMessage from the tool).
			resultJSON, err := cborToJSON(p.Result)
			if err != nil {
				return nil, fmt.Errorf("convert ToolCallCompleted.Result to JSON at seq=%d: %w", ev.Seq, err)
			}
			// Drop from pending / current-turn maps and append a tool
			// message so the upcoming LLMCall sees the completed call.
			if _, ok := pendingByID[p.CallID]; ok {
				delete(pendingByID, p.CallID)
				pendingOrder = removeString(pendingOrder, p.CallID)
			}
			delete(currentTurnScheduled, p.CallID)
			currentTurnScheduledOrder = removeString(currentTurnScheduledOrder, p.CallID)
			state.Msgs = append(state.Msgs, provider.Message{
				Role: provider.RoleTool,
				ToolResult: &provider.ToolResult{
					CallID:  p.CallID,
					Content: string(resultJSON),
				},
			})

		case event.KindToolCallFailed:
			p, err := ev.AsToolCallFailed()
			if err != nil {
				return nil, fmt.Errorf("decode ToolCallFailed at seq=%d: %w", ev.Seq, err)
			}
			if _, ok := pendingByID[p.CallID]; ok {
				delete(pendingByID, p.CallID)
				pendingOrder = removeString(pendingOrder, p.CallID)
			}
			delete(currentTurnScheduled, p.CallID)
			currentTurnScheduledOrder = removeString(currentTurnScheduledOrder, p.CallID)
			// The previous run's react loop would have bailed on a
			// tool error. We still include the failure in history so
			// the model has the full picture if the user resumes past
			// it.
			state.Msgs = append(state.Msgs, provider.Message{
				Role: provider.RoleTool,
				ToolResult: &provider.ToolResult{
					CallID:  p.CallID,
					Content: p.Error,
					IsError: true,
				},
			})

		case event.KindRunResumed:
			// A previous resume point. No state effect beyond the
			// events already replayed on either side of it.

		case event.KindReasoningEmitted,
			event.KindSideEffectRecorded,
			event.KindBudgetExceeded,
			event.KindContextTruncated:
			// Non-turn-boundary events; no state change.

		case event.KindRunStarted:
			return nil, fmt.Errorf("starling: reconstructState: unexpected RunStarted at seq=%d", ev.Seq)

		case event.KindRunCompleted, event.KindRunFailed, event.KindRunCancelled:
			return nil, fmt.Errorf("starling: reconstructState: terminal %s at seq=%d should have been filtered earlier", ev.Kind, ev.Seq)

		default:
			return nil, fmt.Errorf("starling: reconstructState: unhandled event kind %s at seq=%d — update Resume state reconstruction", ev.Kind, ev.Seq)
		}
	}

	// Current-turn scheduled without AssistantMessageCompleted (the
	// previous process died mid-turn between Scheduled and the
	// assistant finishing). Treat as pending too.
	for _, id := range currentTurnScheduledOrder {
		if _, seen := pendingByID[id]; seen {
			continue
		}
		pendingByID[id] = currentTurnScheduled[id]
		pendingOrder = append(pendingOrder, id)
	}

	state.PendingCalls = make([]provider.ToolUse, 0, len(pendingOrder))
	for _, id := range pendingOrder {
		state.PendingCalls = append(state.PendingCalls, pendingByID[id])
	}
	return state, nil
}

// rewriteAssistantToolUseIDs rewrites ToolUse.CallID values in the most
// recent assistant message of msgs using idMap.
func rewriteAssistantToolUseIDs(msgs []provider.Message, idMap map[string]string) {
	if len(idMap) == 0 {
		return
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != provider.RoleAssistant {
			continue
		}
		for j := range msgs[i].ToolUses {
			if fresh, ok := idMap[msgs[i].ToolUses[j].CallID]; ok {
				msgs[i].ToolUses[j].CallID = fresh
			}
		}
		return
	}
}

// cborToJSON decodes canonical CBOR into a generic value and re-marshals
// it as JSON. Used to turn stored tool-call args (CBOR on the wire)
// back into the JSON shape the provider originally emitted.
//
// Numeric precision: round-tripping via `any` collapses CBOR's
// int/float distinction, and integers outside ±2^53 lose precision
// because JSON has no other way to represent them. Tool args from a
// model fit comfortably in that range in practice, but callers pushing
// very large integers through a resumed tool call should be aware.
func cborToJSON(raw cborenc.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage("{}"), nil
	}
	var v any
	if err := cborenc.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("decode cbor: %w", err)
	}
	v = cborToJSONCompatible(v)
	return json.Marshal(v)
}

// cborToJSONCompatible walks a value decoded from CBOR and rewrites
// map[any]any into map[string]any (JSON has only string keys) and
// []byte into base64 strings is left to json.Marshal.
func cborToJSONCompatible(v any) any {
	switch m := v.(type) {
	case map[any]any:
		out := make(map[string]any, len(m))
		for k, val := range m {
			ks, ok := k.(string)
			if !ok {
				ks = fmt.Sprint(k)
			}
			out[ks] = cborToJSONCompatible(val)
		}
		return out
	case map[string]any:
		for k, val := range m {
			m[k] = cborToJSONCompatible(val)
		}
		return m
	case []any:
		for i, val := range m {
			m[i] = cborToJSONCompatible(val)
		}
		return m
	}
	return v
}

func removeString(ss []string, target string) []string {
	for i, s := range ss {
		if s == target {
			return append(ss[:i], ss[i+1:]...)
		}
	}
	return ss
}
