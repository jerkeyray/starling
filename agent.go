package starling

import (
	"context"
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/internal/cborenc"
	"github.com/jerkeyray/starling/internal/merkle"
	"github.com/jerkeyray/starling/internal/obs"
	"github.com/jerkeyray/starling/provider"
	"github.com/jerkeyray/starling/replay"
	"github.com/jerkeyray/starling/step"
	"github.com/jerkeyray/starling/tool"
	"github.com/oklog/ulid/v2"
	"github.com/zeebo/blake3"
	"go.opentelemetry.io/otel/attribute"
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
	// disable that axis. All four axes (MaxInputTokens, MaxOutputTokens,
	// MaxUSD, MaxWallClock) are enforced: input tokens pre-call,
	// output tokens and USD mid-stream on every usage chunk, wall clock
	// via context.WithDeadline on the run.
	Budget *Budget

	// Config carries model / system prompt / params / MaxTurns.
	Config Config

	// Namespace is an optional prefix for this agent's RunIDs, letting
	// multiple tenants share one event log without colliding if they
	// pick the same raw RunID. When non-empty, every event written by
	// this agent carries RunID = Namespace + "/" + <ULID>, and all
	// eventlog lookups (Read, Stream, Replay) must use the same prefixed
	// form. Empty namespace preserves pre-M3 behavior exactly.
	//
	// Must not contain "/" (the reserved separator); validate() rejects
	// this at Run time.
	Namespace string

	// replayRecorded, when non-nil, switches Run into replay mode:
	// step.Context operates under ModeReplay, the RunID is pulled from
	// the first recorded event rather than freshly minted, and every
	// emit() compares against the recorded event at the matching seq.
	// Set exclusively by replay.Verify; not part of the public API.
	replayRecorded []event.Event
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

	var runID string
	if len(a.replayRecorded) > 0 {
		// Replay uses the recorded RunID verbatim — it already carries
		// whatever namespace prefix the original run wrote.
		runID = a.replayRecorded[0].RunID
	} else {
		runID = newULID()
		if a.Namespace != "" {
			runID = a.Namespace + "/" + runID
		}
	}
	startWall := time.Now()

	// Wall-clock budget: wrap the run's ctx with a deadline so blocking
	// provider/tool calls unblock on expiry. DeadlineExceeded surfaces
	// through LLMCall / CallTool unchanged; emitTerminal inspects
	// a.Budget.MaxWallClock to decide between RunCancelled (external
	// cancellation) and RunFailed{ErrorType:"budget"} (wall-clock trip).
	if a.Budget != nil && a.Budget.MaxWallClock > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, startWall.Add(a.Budget.MaxWallClock))
		defer cancel()
	}

	logger := obs.Resolve(a.Config.Logger).With(obs.AttrRunID, runID)

	// Open the root OTel span. The no-op tracer is zero cost when no
	// SDK is wired up, so unconditional wrapping is safe.
	ctx, runSpan := obs.StartRunSpan(ctx, runID)
	defer runSpan.End()

	stepCfg := step.Config{
		Log:      a.Log,
		RunID:    runID,
		Provider: a.Provider,
		Tools:    step.NewRegistry(a.Tools...),
		Budget:   budgetStepConfig(a.Budget),
		Logger:   logger,
	}
	if len(a.replayRecorded) > 0 {
		stepCfg.Mode = step.ModeReplay
		stepCfg.Recorded = a.replayRecorded
	}
	stepCtx, err := step.NewContext(stepCfg)
	if err != nil {
		return nil, fmt.Errorf("starling: build step.Context: %w", err)
	}
	ctx = step.WithContext(ctx, stepCtx)

	// 1. Emit RunStarted.
	if err := a.emitRunStarted(ctx, stepCtx, goal); err != nil {
		return nil, fmt.Errorf("starling: emit RunStarted: %w", err)
	}
	logger.Info("run started", "model", a.Config.Model)

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

	// Log the terminal outcome. RunFailed escalates to Error; normal
	// completion and cancellation are informational.
	termAttrs := []any{obs.AttrKind, term.String(), obs.AttrDurMs, time.Since(startWall).Milliseconds()}
	switch term {
	case event.KindRunFailed:
		logger.Error("run failed", append(termAttrs, "err", errString(runErr))...)
		obs.SetSpanError(runSpan, runErr)
	case event.KindRunCancelled:
		logger.Info("run cancelled", termAttrs...)
		obs.SetSpanError(runSpan, runErr)
	default:
		logger.Info("run completed", termAttrs...)
	}

	result, readErr := a.buildResult(runID, startWall, term)
	// readErr is only populated if the log backend failed to re-read
	// events after a successful terminal emit — rare, but indicates a
	// log-backend problem the caller should know about. Combine it with
	// runErr so neither is silently lost.
	if readErr != nil {
		if runErr != nil {
			return result, errors.Join(runErr, readErr)
		}
		return result, readErr
	}
	return result, runErr
}

// react is the ReAct loop: call LLM, if it planned tools dispatch them,
// loop. Returns nil on clean completion (model produced text with no
// tool uses), or an error to feed into emitTerminal.
func (a *Agent) react(ctx context.Context, goal string) error {
	msgs := []provider.Message{{Role: provider.RoleUser, Content: goal}}
	// Config.MaxTurns is the documented contract: 0 (or negative) means
	// unlimited; positive caps the loop. ctx cancellation and budget
	// limits are the safety nets when the cap is off.
	maxTurns := a.Config.MaxTurns
	unlimited := maxTurns <= 0

	sc, _ := step.From(ctx)
	logger := sc.Logger()

	for turn := 0; unlimited || turn < maxTurns; turn++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		logger.Debug("turn start", "turn", turn)

		turnCtx, turnSpan := obs.StartTurnSpan(ctx, "", turn)

		req := &provider.Request{
			Model:        a.Config.Model,
			SystemPrompt: a.Config.SystemPrompt,
			Messages:     msgs,
			Tools:        toolDefs(a.Tools),
			Params:       a.Config.Params,
		}
		resp, err := step.LLMCall(turnCtx, req)
		if err != nil {
			obs.SetSpanError(turnSpan, err)
			turnSpan.End()
			return err
		}
		// Late-bind the TurnID as a span attribute now that LLMCall has
		// minted it. (OTel attributes are additive; earlier attrs stick.)
		turnSpan.SetAttributes(turnIDAttr(resp.TurnID))

		// Accumulate this assistant turn into history so the next
		// LLMCall sees what it already said + planned.
		msgs = append(msgs, provider.Message{
			Role:     provider.RoleAssistant,
			Content:  resp.Text,
			ToolUses: resp.ToolUses,
		})

		// Terminal turn: no planned tool calls → return.
		if len(resp.ToolUses) == 0 {
			turnSpan.End()
			return nil
		}

		// Dispatch planned tool calls. Single-tool turns stay on the
		// sequential CallTool path (no goroutine overhead); multi-tool
		// turns fan out through CallTools with a semaphore so total
		// latency is max(tool_i) rather than sum(tool_i).
		if len(resp.ToolUses) == 1 {
			tu := resp.ToolUses[0]
			result, cerr := step.CallTool(turnCtx, step.ToolCall{
				CallID: tu.CallID,
				TurnID: resp.TurnID,
				Name:   tu.Name,
				Args:   tu.Args,
			})
			if cerr != nil {
				obs.SetSpanError(turnSpan, cerr)
				turnSpan.End()
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
		} else {
			calls := make([]step.ToolCall, len(resp.ToolUses))
			for i, tu := range resp.ToolUses {
				calls[i] = step.ToolCall{
					CallID: tu.CallID,
					TurnID: resp.TurnID,
					Name:   tu.Name,
					Args:   tu.Args,
				}
			}
			results, cerr := step.CallTools(turnCtx, calls)
			if cerr != nil {
				// Only dispatch-level errors (emit failures, replay
				// mismatch) surface here. Per-tool errors live inside
				// the returned results.
				obs.SetSpanError(turnSpan, cerr)
				turnSpan.End()
				return cerr
			}
			// Check ctx first: if the run was cancelled mid-batch, a
			// tool may have returned ctx.Err() — surface that as the
			// terminal cause rather than a per-tool ToolError.
			if err := ctx.Err(); err != nil {
				obs.SetSpanError(turnSpan, err)
				turnSpan.End()
				return err
			}
			for i, res := range results {
				tu := resp.ToolUses[i]
				if res.Err != nil {
					obs.SetSpanError(turnSpan, res.Err)
					turnSpan.End()
					if errors.Is(res.Err, context.Canceled) || errors.Is(res.Err, context.DeadlineExceeded) {
						return res.Err
					}
					return &ToolError{Name: tu.Name, CallID: tu.CallID, Err: res.Err}
				}
				msgs = append(msgs, provider.Message{
					Role: provider.RoleTool,
					ToolResult: &provider.ToolResult{
						CallID:  tu.CallID,
						Content: string(res.Result),
					},
				})
			}
		}
		turnSpan.End()
	}

	return ErrMaxTurnsExceeded
}

// turnIDAttr returns the KeyValue attribute for a turn's ID, imported
// locally so agent.go doesn't pull OTel directly elsewhere.
func turnIDAttr(turnID string) attribute.KeyValue {
	return attribute.String(obs.AttrTurnID, turnID)
}

// emitRunStarted snapshots all the run-start metadata into a
// RunStarted event. Hashes are over canonical CBOR of the underlying
// value so replay can verify that nothing silently changed between
// original run and replay.
func (a *Agent) emitRunStarted(ctx context.Context, sc *step.Context, goal string) error {
	paramsHash := hashBytes(a.Config.Params)
	sysPromptHash := hashBytes([]byte(a.Config.SystemPrompt))

	// Sort by Name so reordering the same set of tools produces the same
	// ToolRegistryHash. step.Registry.Names() is documented to return
	// alphabetical order for exactly this reason; the event emission path
	// must match.
	schemas := make([]event.ToolSchemaRef, 0, len(a.Tools))
	for _, t := range a.Tools {
		schemas = append(schemas, event.ToolSchemaRef{
			Name:       t.Name(),
			SchemaHash: hashBytes(t.Schema()),
		})
	}
	sort.Slice(schemas, func(i, j int) bool { return schemas[i].Name < schemas[j].Name })
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
	// Wall-clock durations don't round-trip under replay (live and
	// replay take different real time), so on replay we substitute the
	// recorded value. step.ReplayDurationMs is a no-op in live mode.
	durMs := step.ReplayDurationMs(ctx, time.Since(startWall).Milliseconds())

	// Pull everything already logged under this run to compute the
	// Merkle root. Uses a detached ctx so a cancelled outer ctx
	// doesn't prevent us from reading.
	readCtx := context.WithoutCancel(ctx)
	evs, err := a.Log.Read(readCtx, sc.RunID())
	if err != nil {
		return 0, fmt.Errorf("read log: %w", err)
	}
	hashes, err := merkle.EventHashes(evs)
	if err != nil {
		return 0, fmt.Errorf("hash events: %w", err)
	}
	root := merkle.Root(hashes)

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
	case errors.Is(runErr, context.DeadlineExceeded) && a.Budget != nil && a.Budget.MaxWallClock > 0:
		// Wall-clock budget trip. Emit BudgetExceeded{wall_clock} so
		// the log carries the trip detail (matches the mid_stream
		// shape for the other axes), then RunFailed{ErrorType:"budget"}.
		// Using durMs (already replay-stable) keeps live/replay
		// byte-identical.
		if err := stepEmit(ctx, sc, event.KindBudgetExceeded, event.BudgetExceeded{
			Limit:  "wall_clock",
			Cap:    float64(a.Budget.MaxWallClock.Milliseconds()),
			Actual: float64(durMs),
			Where:  "mid_stream",
		}); err != nil {
			return 0, fmt.Errorf("emit BudgetExceeded(wall_clock): %w", err)
		}
		// Re-read events so the Merkle root covers the BudgetExceeded
		// we just emitted.
		evs2, err := a.Log.Read(readCtx, sc.RunID())
		if err != nil {
			return 0, fmt.Errorf("read log: %w", err)
		}
		hashes2, err := merkle.EventHashes(evs2)
		if err != nil {
			return 0, fmt.Errorf("hash events: %w", err)
		}
		root = merkle.Root(hashes2)
		return event.KindRunFailed, stepEmit(ctx, sc, event.KindRunFailed, event.RunFailed{
			Error:      runErr.Error(),
			ErrorType:  "budget",
			MerkleRoot: root,
			DurationMs: durMs,
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
//
// The returned error is populated only when the log re-read fails
// (terminal event is already written at this point, so the run
// itself is final); callers should errors.Join it with any run error.
func (a *Agent) buildResult(runID string, startWall time.Time, term event.Kind) (*RunResult, error) {
	evs, readErr := a.Log.Read(context.Background(), runID)
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
	}, readErr
}

// RunReplay re-executes the agent in replay mode against recorded.
// Intended for callers of the replay package; not part of the normal
// user flow. Goal, RunID, and provider streams are all reconstructed
// from recorded; the original Provider and Log are overridden (the
// Provider by a replay provider, the Log by a fresh in-memory log)
// so the live side is fully isolated from the recording.
//
// Returns nil on a clean byte-matching replay. On divergence, returns
// an error that wraps step.ErrReplayMismatch — the replay package
// wraps that further into ErrNonDeterminism.
func (a *Agent) RunReplay(ctx context.Context, recorded []event.Event) error {
	sink := eventlog.NewInMemory()
	defer sink.Close()
	return a.RunReplayInto(ctx, recorded, sink)
}

// RunReplayInto is RunReplay with a caller-supplied sink log instead
// of an internal in-memory one. Intended for callers (notably
// replay.Stream) that need to observe the byte-matching events as
// they are appended — subscribe to sink.Stream(...) before calling.
//
// The sink's lifecycle is the caller's responsibility; this method
// does NOT close it.
func (a *Agent) RunReplayInto(ctx context.Context, recorded []event.Event, sink eventlog.EventLog) error {
	if len(recorded) == 0 {
		return fmt.Errorf("starling: RunReplay called with no recorded events")
	}
	if sink == nil {
		return fmt.Errorf("starling: RunReplayInto called with nil sink")
	}
	rs, err := recorded[0].AsRunStarted()
	if err != nil {
		return fmt.Errorf("starling: RunReplay: decode RunStarted: %w", err)
	}

	// Build a replay provider backed by the recorded stream. Info
	// comes from the recorded RunStarted so the provider ID / API
	// version match at emit-compare time.
	replayProv, err := replay.NewProvider(provider.Info{ID: rs.ProviderID, APIVersion: rs.APIVersion}, recorded)
	if err != nil {
		return err
	}

	// Shallow clone so we don't mutate the caller's Agent. Override
	// the transient fields: Provider (replay), Log (caller-supplied
	// sink), and set replayRecorded so Run() flips into replay mode.
	clone := *a
	clone.Provider = replayProv
	clone.Log = sink
	clone.replayRecorded = recorded

	_, runErr := clone.Run(ctx, rs.Goal)
	return runErr
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
	if strings.ContainsRune(a.Namespace, '/') {
		return fmt.Errorf("starling: Agent.Namespace must not contain '/' (reserved separator); got %q", a.Namespace)
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

// budgetStepConfig projects the full Agent.Budget onto the step-level
// BudgetConfig (input/output tokens + USD). Wall-clock is intentionally
// omitted — it's enforced at the agent level via context.WithDeadline.
func budgetStepConfig(b *Budget) step.BudgetConfig {
	if b == nil {
		return step.BudgetConfig{}
	}
	return step.BudgetConfig{
		MaxInputTokens:  b.MaxInputTokens,
		MaxOutputTokens: b.MaxOutputTokens,
		MaxUSD:          b.MaxUSD,
	}
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

// errString renders err for log attrs without panicking on nil. The
// terminal slog lines use it because a RunFailed can be triggered by
// a non-error path (e.g. MaxTurnsExceeded) that still benefits from
// an attribute.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
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
