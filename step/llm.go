package step

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	cryptorand "crypto/rand"
	"io"
	"sync"
	"time"

	"github.com/jerkeyray/starling/budget"
	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/internal/cborenc"
	"github.com/jerkeyray/starling/internal/obs"
	"github.com/jerkeyray/starling/provider"
	"github.com/oklog/ulid/v2"
)

// LLMCall performs a single streaming completion against the Provider
// configured on ctx's step.Context. It emits the canonical event
// sequence (TurnStarted → (ReasoningEmitted)* → AssistantMessageCompleted)
// and returns the aggregated provider.Response for the caller to act on.
//
// Pre-call input-token budget is enforced against
// Config.Budget.MaxInputTokens. On breach, a BudgetExceeded event is
// emitted and ErrBudgetExceeded is returned; no TurnStarted is emitted.
//
// On mid-stream error or context cancellation, the error is returned
// unchanged and no AssistantMessageCompleted is emitted — the agent
// loop (T9) is responsible for emitting the terminal RunFailed /
// RunCancelled event.
//
// Panics if ctx has no step.Context attached, or if the Context was
// built without a Provider.
func LLMCall(ctx context.Context, req *provider.Request) (*provider.Response, error) {
	c := mustFrom(ctx, "LLMCall")
	p := c.prov()
	if p == nil {
		panic("step.LLMCall: step.Context was constructed without a Provider")
	}
	if req == nil {
		return nil, fmt.Errorf("step.LLMCall: req is nil")
	}

	// 1. Pre-call budget check.
	est := estimateRequestTokens(req)
	if cap := c.budgetCfg().MaxInputTokens; cap > 0 && est > cap {
		if err := emit(ctx, c, event.KindBudgetExceeded, event.BudgetExceeded{
			Limit:  "input_tokens",
			Cap:    float64(cap),
			Actual: float64(est),
			Where:  "pre_call",
		}); err != nil {
			return nil, fmt.Errorf("step.LLMCall: emit BudgetExceeded: %w", err)
		}
		c.logger.Warn("budget exceeded",
			obs.AttrLimit, "input_tokens",
			obs.AttrCap, cap,
			obs.AttrActual, est,
			"where", "pre_call")
		return nil, ErrBudgetExceeded
	}

	// 2. Mint TurnID. In replay mode this reads the recorded TurnID
	// at the current seq so TurnStarted's payload matches byte-for-byte.
	turnID, err := c.mintTurnID()
	if err != nil {
		return nil, fmt.Errorf("step.LLMCall: mint TurnID: %w", err)
	}

	// 3. PromptHash over canonical CBOR of req.
	promptHash, err := hashRequest(req)
	if err != nil {
		return nil, fmt.Errorf("step.LLMCall: hash request: %w", err)
	}

	// 4. TurnStarted.
	if err := emit(ctx, c, event.KindTurnStarted, event.TurnStarted{
		TurnID:      turnID,
		PromptHash:  promptHash,
		InputTokens: est,
	}); err != nil {
		return nil, fmt.Errorf("step.LLMCall: emit TurnStarted: %w", err)
	}

	// 5. Open stream.
	stream, err := p.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	// 6. Drain.
	var (
		textBuf       bytes.Buffer
		usage         provider.UsageUpdate
		stopReason    string
		rawRespHash   []byte
		providerReqID string
	)
	type pendingUse struct {
		CallID  string
		Name    string
		argsBuf bytes.Buffer
	}
	var uses []*pendingUse
	useIdx := make(map[string]int)

	// reasoningBuf accumulates thinking_delta text across a block; on the
	// trailing signature-carrying ChunkReasoning we flush one
	// ReasoningEmitted so the whole block, signature included, lands as a
	// single event. flushReasoning emits and resets the buffer.
	var reasoningBuf bytes.Buffer
	flushReasoning := func(sig []byte) error {
		if reasoningBuf.Len() == 0 && len(sig) == 0 {
			return nil
		}
		ev := event.ReasoningEmitted{
			TurnID:    turnID,
			Content:   reasoningBuf.String(),
			Sensitive: true, // per EVENTS.md §3.4: always true for schema symmetry
			Signature: sig,
		}
		reasoningBuf.Reset()
		return emit(ctx, c, event.KindReasoningEmitted, ev)
	}

drain:
	for {
		chunk, nerr := stream.Next(ctx)
		if nerr != nil {
			if errors.Is(nerr, io.EOF) {
				break
			}
			return nil, nerr
		}
		switch chunk.Kind {
		case provider.ChunkText:
			textBuf.WriteString(chunk.Text)
		case provider.ChunkReasoning:
			// Text-only mid-block chunks: buffer. Signature-carrying
			// trailing chunk (empty Text): flush as one event.
			if chunk.Text != "" {
				reasoningBuf.WriteString(chunk.Text)
			}
			if len(chunk.Signature) > 0 {
				if err := flushReasoning(chunk.Signature); err != nil {
					return nil, fmt.Errorf("step.LLMCall: emit ReasoningEmitted: %w", err)
				}
			}
		case provider.ChunkRedactedThinking:
			// Redacted-thinking blocks stand alone: opaque payload +
			// signature arrive together. Emit directly without using
			// the thinking-block buffer.
			if err := emit(ctx, c, event.KindReasoningEmitted, event.ReasoningEmitted{
				TurnID:    turnID,
				Content:   chunk.Text,
				Sensitive: true,
				Signature: chunk.Signature,
				Redacted:  true,
			}); err != nil {
				return nil, fmt.Errorf("step.LLMCall: emit ReasoningEmitted (redacted): %w", err)
			}
		case provider.ChunkToolUseStart:
			if chunk.ToolUse == nil {
				return nil, fmt.Errorf("step.LLMCall: ChunkToolUseStart missing ToolUse payload")
			}
			pu := &pendingUse{CallID: chunk.ToolUse.CallID, Name: chunk.ToolUse.Name}
			useIdx[chunk.ToolUse.CallID] = len(uses)
			uses = append(uses, pu)
		case provider.ChunkToolUseDelta:
			if chunk.ToolUse == nil {
				return nil, fmt.Errorf("step.LLMCall: ChunkToolUseDelta missing ToolUse payload")
			}
			idx, ok := useIdx[chunk.ToolUse.CallID]
			if !ok {
				return nil, fmt.Errorf("step.LLMCall: ChunkToolUseDelta for unknown CallID %q", chunk.ToolUse.CallID)
			}
			uses[idx].argsBuf.WriteString(chunk.ToolUse.ArgsDelta)
		case provider.ChunkToolUseEnd:
			// no-op: args buffer stays untouched; parse happens at emit time
		case provider.ChunkUsage:
			if chunk.Usage != nil {
				usage = *chunk.Usage
			}
			// Mid-stream enforcement. Providers report cumulative usage
			// (Anthropic updates across message_delta events, OpenAI
			// once at EOF), so each ChunkUsage is a fresh snapshot to
			// re-check against the caps. Trip emits BudgetExceeded
			// carrying the partial text + tokens seen so far, then
			// unwinds with ErrBudgetExceeded; the agent loop classifies
			// into RunFailed{ErrorType:"budget"}.
			if cfg := c.budgetCfg(); cfg.MaxOutputTokens > 0 || cfg.MaxUSD > 0 {
				limit, cap, actual := budget.Enforce(
					budget.Budget{MaxOutputTokens: cfg.MaxOutputTokens, MaxUSD: cfg.MaxUSD},
					req.Model, usage.InputTokens, usage.OutputTokens, time.Time{},
				)
				if limit != "" {
					if err := emit(ctx, c, event.KindBudgetExceeded, event.BudgetExceeded{
						Limit:         limit,
						Cap:           cap,
						Actual:        actual,
						Where:         "mid_stream",
						TurnID:        turnID,
						PartialText:   textBuf.String(),
						PartialTokens: usage.OutputTokens,
					}); err != nil {
						return nil, fmt.Errorf("step.LLMCall: emit BudgetExceeded: %w", err)
					}
					c.logger.Warn("budget exceeded",
						obs.AttrLimit, limit,
						obs.AttrCap, cap,
						obs.AttrActual, actual,
						obs.AttrTurnID, turnID,
						"where", "mid_stream")
					return nil, ErrBudgetExceeded
				}
			}
		case provider.ChunkEnd:
			// Flush any reasoning text that never received a trailing
			// signature (OpenAI-family reasoning summaries); signature
			// stays nil.
			if err := flushReasoning(nil); err != nil {
				return nil, fmt.Errorf("step.LLMCall: emit ReasoningEmitted: %w", err)
			}
			stopReason = chunk.StopReason
			rawRespHash = chunk.RawResponseHash
			providerReqID = chunk.ProviderReqID
			break drain
		}
	}

	// 7. Build PlannedToolUse list (CBOR-encoded args) for the event, and
	// the provider.ToolUse list (JSON args) for the Response.
	planned := make([]event.PlannedToolUse, 0, len(uses))
	respUses := make([]provider.ToolUse, 0, len(uses))
	for _, pu := range uses {
		rawJSON := bytes.Clone(pu.argsBuf.Bytes())
		if len(rawJSON) == 0 {
			// Model emitted no arg bytes — treat as empty object.
			rawJSON = []byte("{}")
		}
		argsCBOR, cerr := jsonToCanonicalCBOR(rawJSON)
		if cerr != nil {
			return nil, fmt.Errorf("step.LLMCall: tool %q args: %w", pu.Name, cerr)
		}
		planned = append(planned, event.PlannedToolUse{
			CallID:   pu.CallID,
			ToolName: pu.Name,
			Args:     argsCBOR,
		})
		respUses = append(respUses, provider.ToolUse{
			CallID: pu.CallID,
			Name:   pu.Name,
			Args:   rawJSON,
		})
	}

	// 8. Cost.
	cost, _ := budget.CostUSD(req.Model, usage.InputTokens, usage.OutputTokens)

	// 9. AssistantMessageCompleted.
	if err := emit(ctx, c, event.KindAssistantMessageCompleted, event.AssistantMessageCompleted{
		TurnID:            turnID,
		Text:              textBuf.String(),
		ToolUses:          planned,
		StopReason:        stopReason,
		InputTokens:       usage.InputTokens,
		OutputTokens:      usage.OutputTokens,
		CacheReadTokens:   usage.CacheReadTokens,
		CacheCreateTokens: usage.CacheCreateTokens,
		CostUSD:           cost,
		RawResponseHash:   rawRespHash,
		ProviderRequestID: providerReqID,
	}); err != nil {
		return nil, fmt.Errorf("step.LLMCall: emit AssistantMessageCompleted: %w", err)
	}

	return &provider.Response{
		Text:            textBuf.String(),
		ToolUses:        respUses,
		TurnID:          turnID,
		StopReason:      stopReason,
		Usage:           usage,
		CostUSD:         cost,
		RawResponseHash: rawRespHash,
		ProviderReqID:   providerReqID,
	}, nil
}

// hashRequest returns blake3 of the canonical CBOR encoding of a
// request-shaped map. We build an ordered map manually (rather than
// marshalling *Request) because Request's fields use json.RawMessage
// for ToolUses/Args — we want structural equality, not byte equality
// of inner JSON, and CoreDet gives us that via cborenc.
func hashRequest(req *provider.Request) ([]byte, error) {
	// A straight cborenc.Marshal(req) is stable because CoreDet orders
	// map keys; struct fields are encoded in declaration order which is
	// also stable. json.RawMessage bytes go in verbatim — acceptable for
	// a prompt-hash that only needs to be reproducible from the same
	// request value.
	b, err := cborenc.Marshal(req)
	if err != nil {
		return nil, err
	}
	return event.Hash(b), nil
}

// jsonToCanonicalCBOR routes arbitrary JSON bytes through a generic
// interface into canonical CBOR. Lossy for numeric types (everything
// becomes float64 or int64), which is acceptable here — the args are
// going into the event log and back out as CBOR; callers of the tool
// receive the original JSON bytes, not the re-encoded form.
func jsonToCanonicalCBOR(raw []byte) (cborenc.RawMessage, error) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // preserve int/float distinction where possible
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("decode json: %w", err)
	}
	v = normalizeJSONNumbers(v)
	return cborenc.Marshal(v)
}

// normalizeJSONNumbers walks a json.Decoder UseNumber tree and
// converts json.Number into int64 (preferred) or float64.
func normalizeJSONNumbers(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, sub := range t {
			t[k] = normalizeJSONNumbers(sub)
		}
		return t
	case []any:
		for i, sub := range t {
			t[i] = normalizeJSONNumbers(sub)
		}
		return t
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		f, _ := t.Float64()
		return f
	default:
		return v
	}
}

// ulidMu guards access to ulid.MustNew with a crypto/rand entropy
// source. crypto/rand.Reader itself is concurrent-safe, but bundling
// Timestamp+MustNew under a mutex keeps ULIDs monotonic within a
// single process without us having to reach for ulid.Monotonic (which
// is explicitly documented as NOT concurrent-safe).
var ulidMu sync.Mutex

func newULID() string {
	ulidMu.Lock()
	defer ulidMu.Unlock()
	return ulid.MustNew(ulid.Timestamp(time.Now()), cryptorand.Reader).String()
}
