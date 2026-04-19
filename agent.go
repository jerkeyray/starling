package starling

import (
	"context"
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/internal/cborenc"
	"github.com/jerkeyray/starling/provider"
	"github.com/jerkeyray/starling/step"
	"github.com/jerkeyray/starling/tool"
	"github.com/oklog/ulid/v2"
	"github.com/zeebo/blake3"
)

// Agent is the user-facing entry point. Construction is a plain
// struct-literal — every field is validated on Run, not at build time,
// so tests can poke at Agent{} freely.
type Agent struct {
	// Provider is the LLM adapter. Required.
	Provider provider.Provider

	// Tools are the tools the agent may plan. Optional; an agent with
	// no tools can still have conversations.
	Tools []tool.Tool

	// Log is the event log backend. Required.
	Log eventlog.EventLog

	// Budget is the budget enforcement struct. Optional; zero values
	// disable that axis. M1 honours MaxInputTokens only.
	Budget *Budget

	// Config carries model / system prompt / params / MaxTurns.
	Config Config
}

// Run starts a new agent run against the configured provider + tools.
// The returned RunResult summarizes the run; full detail is in Log.
//
// Terminal events are always emitted before Run returns (successful
// completion → RunCompleted; ctx cancellation → RunCancelled; any
// other error → RunFailed), so the log is self-describing regardless
// of how the run ends.
func (a *Agent) Run(ctx context.Context, goal string) (*RunResult, error) {
	if err := a.validate(); err != nil {
		return nil, err
	}

	runID := newULID()
	startWall := time.Now()

	stepCtx := step.NewContext(step.Config{
		Log:      a.Log,
		RunID:    runID,
		Provider: a.Provider,
		Tools:    step.NewRegistry(a.Tools...),
		Budget:   step.BudgetConfig{MaxInputTokens: budgetInputCap(a.Budget)},
	})
	ctx = step.WithContext(ctx, stepCtx)

	// 1. Emit RunStarted.
	if err := a.emitRunStarted(ctx, stepCtx, goal); err != nil {
		return nil, fmt.Errorf("starling: emit RunStarted: %w", err)
	}

	// 2. Run the ReAct loop, catching the terminal reason.
	runErr := a.react(ctx, goal)

	// 3. Emit the terminal event.
	term, terr := a.emitTerminal(ctx, stepCtx, runErr, startWall)
	if terr != nil {
		// Terminal emission failure is catastrophic — the run is now
		// un-terminated in the log. Surface the inner error if any,
		// else the emission error.
		if runErr != nil {
			return nil, runErr
		}
		return nil, terr
	}

	result := a.buildResult(runID, startWall, term)
	return result, runErr
}

// react is the ReAct loop: call LLM, if it planned tools dispatch them,
// loop. Returns nil on clean completion (model produced text with no
// tool uses), or an error to feed into emitTerminal.
func (a *Agent) react(ctx context.Context, goal string) error {
	msgs := []provider.Message{{Role: provider.RoleUser, Content: goal}}
	maxTurns := a.Config.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 16 // conservative default; callers can override
	}

	for turn := 0; turn < maxTurns; turn++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		req := &provider.Request{
			Model:        a.Config.Model,
			SystemPrompt: a.Config.SystemPrompt,
			Messages:     msgs,
			Tools:        toolDefs(a.Tools),
			Params:       a.Config.Params,
		}
		resp, err := step.LLMCall(ctx, req)
		if err != nil {
			return err
		}

		// Accumulate this assistant turn into history so the next
		// LLMCall sees what it already said + planned.
		msgs = append(msgs, provider.Message{
			Role:     provider.RoleAssistant,
			Content:  resp.Text,
			ToolUses: resp.ToolUses,
		})

		// Terminal turn: no planned tool calls → return.
		if len(resp.ToolUses) == 0 {
			return nil
		}

		// Dispatch each planned tool call sequentially (M1).
		for _, tu := range resp.ToolUses {
			result, cerr := step.CallTool(ctx, step.ToolCall{
				CallID: tu.CallID,
				TurnID: currentTurnID(ctx), // unused in M1; TurnID stamped inside events already via LLMCall
				Name:   tu.Name,
				Args:   tu.Args,
			})
			if cerr != nil {
				// Any tool failure bubbles up as a ToolError; agent loop
				// surrenders and the terminal event classifies.
				if errors.Is(cerr, context.Canceled) || errors.Is(cerr, context.DeadlineExceeded) {
					return cerr
				}
				return &ToolError{Name: tu.Name, CallID: tu.CallID, Err: cerr}
			}
			msgs = append(msgs, provider.Message{
				Role: provider.RoleTool,
				ToolResult: &provider.ToolResult{
					CallID:  tu.CallID,
					Content: string(result),
				},
			})
		}
	}

	return ErrMaxTurnsExceeded
}

// emitRunStarted snapshots all the run-start metadata into a
// RunStarted event. Hashes are over canonical CBOR of the underlying
// value so replay can verify that nothing silently changed between
// original run and replay.
func (a *Agent) emitRunStarted(ctx context.Context, sc *step.Context, goal string) error {
	paramsHash := hashBytes(a.Config.Params)
	sysPromptHash := hashBytes([]byte(a.Config.SystemPrompt))

	schemas := make([]event.ToolSchemaRef, 0, len(a.Tools))
	for _, t := range a.Tools {
		schemas = append(schemas, event.ToolSchemaRef{
			Name:       t.Name(),
			SchemaHash: hashBytes(t.Schema()),
		})
	}
	registryHash := toolRegistryHash(schemas)

	info := a.Provider.Info()

	return stepEmit(ctx, sc, event.KindRunStarted, event.RunStarted{
		SchemaVersion:    event.SchemaVersion,
		Goal:             goal,
		ProviderID:       info.ID,
		ModelID:          a.Config.Model,
		APIVersion:       info.APIVersion,
		ParamsHash:       paramsHash,
		Params:           a.Config.Params,
		SystemPromptHash: sysPromptHash,
		SystemPrompt:     a.Config.SystemPrompt,
		ToolRegistryHash: registryHash,
		ToolSchemas:      schemas,
		Budget:           budgetLimits(a.Budget),
	})
}

// emitTerminal picks the right terminal event kind from runErr,
// computes the Merkle root over every event already in the log (which
// excludes the terminal itself by design), and appends the terminal.
func (a *Agent) emitTerminal(ctx context.Context, sc *step.Context, runErr error, startWall time.Time) (event.Kind, error) {
	durMs := time.Since(startWall).Milliseconds()

	// Pull everything already logged under this run to compute the
	// Merkle root. Uses a detached ctx so a cancelled outer ctx
	// doesn't prevent us from reading.
	readCtx := context.WithoutCancel(ctx)
	evs, err := a.Log.Read(readCtx, sc.RunID())
	if err != nil {
		return 0, fmt.Errorf("read log: %w", err)
	}
	hashes, err := eventHashes(evs)
	if err != nil {
		return 0, fmt.Errorf("hash events: %w", err)
	}
	root := merkleRoot(hashes)

	// Aggregate counts / cost from the already-emitted events so the
	// terminal payload matches what replay would compute.
	stats := aggregateStats(evs)

	switch {
	case runErr == nil:
		return event.KindRunCompleted, stepEmit(ctx, sc, event.KindRunCompleted, event.RunCompleted{
			FinalText:         stats.FinalText,
			TurnCount:         stats.TurnCount,
			ToolCallCount:     stats.ToolCallCount,
			TotalCostUSD:      stats.TotalCostUSD,
			TotalInputTokens:  stats.InputTokens,
			TotalOutputTokens: stats.OutputTokens,
			DurationMs:        durMs,
			MerkleRoot:        root,
		})
	case errors.Is(runErr, context.Canceled), errors.Is(runErr, context.DeadlineExceeded):
		reason := "context_canceled"
		if errors.Is(runErr, context.DeadlineExceeded) {
			reason = "deadline_exceeded"
		}
		return event.KindRunCancelled, stepEmit(ctx, sc, event.KindRunCancelled, event.RunCancelled{
			Reason:     reason,
			MerkleRoot: root,
			DurationMs: durMs,
		})
	default:
		return event.KindRunFailed, stepEmit(ctx, sc, event.KindRunFailed, event.RunFailed{
			Error:      runErr.Error(),
			ErrorType:  classifyRunError(runErr),
			MerkleRoot: root,
			DurationMs: durMs,
		})
	}
}

// buildResult materializes the user-facing RunResult from the log.
// It re-reads the log (cheap for in-mem; SQLite caches) so the values
// match what a later reader would compute.
func (a *Agent) buildResult(runID string, startWall time.Time, term event.Kind) *RunResult {
	evs, _ := a.Log.Read(context.Background(), runID)
	stats := aggregateStats(evs)

	var root []byte
	if n := len(evs); n > 0 {
		switch evs[n-1].Kind {
		case event.KindRunCompleted:
			if rc, err := evs[n-1].AsRunCompleted(); err == nil {
				root = rc.MerkleRoot
			}
		case event.KindRunFailed:
			if rf, err := evs[n-1].AsRunFailed(); err == nil {
				root = rf.MerkleRoot
			}
		case event.KindRunCancelled:
			if rc, err := evs[n-1].AsRunCancelled(); err == nil {
				root = rc.MerkleRoot
			}
		}
	}

	return &RunResult{
		RunID:         runID,
		FinalText:     stats.FinalText,
		TurnCount:     int(stats.TurnCount),
		ToolCallCount: int(stats.ToolCallCount),
		TotalCostUSD:  stats.TotalCostUSD,
		InputTokens:   stats.InputTokens,
		OutputTokens:  stats.OutputTokens,
		Duration:      time.Since(startWall),
		TerminalKind:  term,
		MerkleRoot:    root,
	}
}

// validate checks the Agent's required fields before Run starts. Kept
// off NewAgent so tests can use struct literals; centralized here so
// the Run entrypoint is the single enforcement point.
func (a *Agent) validate() error {
	if a.Provider == nil {
		return fmt.Errorf("starling: Agent.Provider is nil")
	}
	if a.Log == nil {
		return fmt.Errorf("starling: Agent.Log is nil")
	}
	if a.Config.Model == "" {
		return fmt.Errorf("starling: Agent.Config.Model is empty")
	}
	// Tool name uniqueness — silent duplicates would clobber each
	// other inside step.Registry, which is a sharp foot-gun.
	seen := make(map[string]struct{}, len(a.Tools))
	for _, t := range a.Tools {
		if _, dup := seen[t.Name()]; dup {
			return fmt.Errorf("starling: duplicate tool name %q", t.Name())
		}
		seen[t.Name()] = struct{}{}
	}
	return nil
}

// ----------------------------------------------------------------------
// Helpers
// ----------------------------------------------------------------------

// stats is the aggregated view computed from the event stream for the
// terminal event payload and RunResult.
type stats struct {
	TurnCount     uint32
	ToolCallCount uint32
	InputTokens   int64
	OutputTokens  int64
	TotalCostUSD  float64
	FinalText     string
}

func aggregateStats(evs []event.Event) stats {
	var s stats
	for i := range evs {
		switch evs[i].Kind {
		case event.KindTurnStarted:
			s.TurnCount++
		case event.KindToolCallScheduled:
			s.ToolCallCount++
		case event.KindAssistantMessageCompleted:
			amc, err := evs[i].AsAssistantMessageCompleted()
			if err != nil {
				continue
			}
			s.InputTokens += amc.InputTokens
			s.OutputTokens += amc.OutputTokens
			s.TotalCostUSD += amc.CostUSD
			s.FinalText = amc.Text
		}
	}
	return s
}

func hashBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	sum := blake3.Sum256(b)
	return sum[:]
}

func toolRegistryHash(schemas []event.ToolSchemaRef) []byte {
	if len(schemas) == 0 {
		return nil
	}
	enc, err := cborenc.Marshal(schemas)
	if err != nil {
		return nil
	}
	return event.Hash(enc)
}

func toolDefs(tools []tool.Tool) []provider.ToolDefinition {
	if len(tools) == 0 {
		return nil
	}
	defs := make([]provider.ToolDefinition, len(tools))
	for i, t := range tools {
		defs[i] = provider.ToolDefinition{
			Name:        t.Name(),
			Description: t.Description(),
			Schema:      t.Schema(),
		}
	}
	return defs
}

func budgetInputCap(b *Budget) int64 {
	if b == nil {
		return 0
	}
	return b.MaxInputTokens
}

func budgetLimits(b *Budget) *event.BudgetLimits {
	if b == nil {
		return nil
	}
	return &event.BudgetLimits{
		MaxInputTokens:  b.MaxInputTokens,
		MaxOutputTokens: b.MaxOutputTokens,
		MaxUSD:          b.MaxUSD,
		MaxWallClockMs:  b.MaxWallClock.Milliseconds(),
	}
}

func classifyRunError(err error) string {
	switch {
	case errors.Is(err, ErrBudgetExceeded), errors.Is(err, step.ErrBudgetExceeded):
		return "budget"
	case errors.Is(err, ErrMaxTurnsExceeded):
		return "max_turns"
	}
	var toolErr *ToolError
	if errors.As(err, &toolErr) {
		return "tool"
	}
	var provErr *ProviderError
	if errors.As(err, &provErr) {
		return "provider"
	}
	return "internal"
}

// currentTurnID is a TODO-shaped helper: the LLMCall-minted TurnID is
// not currently exposed on the Response, which means the agent loop
// can't stamp CallTool with the matching TurnID. For M1 we pass an
// empty TurnID; the link is reconstructable from seq ordering (the
// TurnStarted event immediately before each ToolCallScheduled).
//
// TODO(T8 follow-up): surface the minted TurnID on provider.Response
// so this becomes `return resp.TurnID`. Tracked as a M2 cleanup.
func currentTurnID(_ context.Context) string { return "" }

// ----------------------------------------------------------------------
// ULID
// ----------------------------------------------------------------------

var (
	agentUlidMu sync.Mutex
)

func newULID() string {
	agentUlidMu.Lock()
	defer agentUlidMu.Unlock()
	return ulid.MustNew(ulid.Timestamp(time.Now()), cryptorand.Reader).String()
}

// ----------------------------------------------------------------------
// stepEmit adapter
// ----------------------------------------------------------------------

// stepEmit bridges to step's internal emit. We can't call step.emit
// directly (unexported), so we piggyback on step.SideEffect — no,
// that records a different event kind. Instead we call a dedicated
// helper below that the step package exposes for the agent loop.
//
// For M1 the agent emits RunStarted / terminal events directly via
// a helper that mirrors step.emit's logic but uses the Context's
// exported methods. See step.Emit (added for T9).
func stepEmit[T any](ctx context.Context, sc *step.Context, kind event.Kind, payload T) error {
	return step.Emit(ctx, sc, kind, payload)
}
