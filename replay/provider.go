package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/internal/cborenc"
	"github.com/jerkeyray/starling/provider"
)

// newReplayProvider builds a provider.Provider that reconstructs stream
// chunks from a recorded event log. Each Stream() call advances to the
// next turn's recorded events and replays them as a canonical chunk
// sequence. The step package re-aggregates those chunks into the same
// AssistantMessageCompleted payload that was originally recorded, so
// emit-compare in step.Context under ModeReplay verifies equality.
//
// The reconstructed stream omits token timings (deltas are delivered
// as a single Text chunk, tool-use args as a single Delta) — the
// aggregate state after draining matches the live run, which is all
// the replay verifier checks.
func newReplayProvider(info provider.Info, recorded []event.Event) (*replayProvider, error) {
	turns, err := groupByTurn(recorded)
	if err != nil {
		return nil, err
	}
	return &replayProvider{info: info, turns: turns}, nil
}

type replayProvider struct {
	info  provider.Info
	turns []turnEvents
	idx   int
}

type turnEvents struct {
	reasoning []event.ReasoningEmitted
	message   event.AssistantMessageCompleted
}

// Info returns the recorded provider metadata so RunStarted's
// ProviderID / APIVersion comparison in emit-compare matches.
func (p *replayProvider) Info() provider.Info { return p.info }

func (p *replayProvider) Stream(ctx context.Context, req *provider.Request) (provider.EventStream, error) {
	if p.idx >= len(p.turns) {
		return nil, fmt.Errorf("replay: provider called %d times, only %d turns recorded", p.idx+1, len(p.turns))
	}
	turn := p.turns[p.idx]
	p.idx++
	chunks, err := buildChunks(turn)
	if err != nil {
		return nil, err
	}
	return &replayStream{chunks: chunks}, nil
}

type replayStream struct {
	chunks []provider.StreamChunk
	pos    int
}

func (s *replayStream) Next(ctx context.Context) (provider.StreamChunk, error) {
	if s.pos >= len(s.chunks) {
		return provider.StreamChunk{}, io.EOF
	}
	c := s.chunks[s.pos]
	s.pos++
	return c, nil
}

func (s *replayStream) Close() error { return nil }

// groupByTurn walks the event stream and collects each turn's reasoning
// + completion events. TurnStarted marks a boundary; events between
// two TurnStarted boundaries (or between the last TurnStarted and
// stream end) belong to that turn.
func groupByTurn(events []event.Event) ([]turnEvents, error) {
	var turns []turnEvents
	var current *turnEvents
	flush := func() {
		if current != nil {
			turns = append(turns, *current)
			current = nil
		}
	}
	for _, ev := range events {
		switch ev.Kind {
		case event.KindTurnStarted:
			flush()
			current = &turnEvents{}
		case event.KindReasoningEmitted:
			if current == nil {
				continue
			}
			r, err := ev.AsReasoningEmitted()
			if err != nil {
				return nil, fmt.Errorf("replay: decode ReasoningEmitted at seq=%d: %w", ev.Seq, err)
			}
			current.reasoning = append(current.reasoning, r)
		case event.KindAssistantMessageCompleted:
			if current == nil {
				return nil, fmt.Errorf("replay: AssistantMessageCompleted at seq=%d without TurnStarted", ev.Seq)
			}
			m, err := ev.AsAssistantMessageCompleted()
			if err != nil {
				return nil, fmt.Errorf("replay: decode AssistantMessageCompleted at seq=%d: %w", ev.Seq, err)
			}
			current.message = m
			flush()
		}
	}
	// A turn with no completion (only possible on a mid-turn crash)
	// isn't replayable — surface it explicitly rather than silently
	// drop.
	if current != nil {
		return nil, fmt.Errorf("replay: trailing TurnStarted with no AssistantMessageCompleted")
	}
	return turns, nil
}

// buildChunks emits the canonical StreamChunk sequence for one turn:
// reasoning chunks, then text, then tool-use start/delta/end per
// planned tool, then usage, then end.
func buildChunks(t turnEvents) ([]provider.StreamChunk, error) {
	var chunks []provider.StreamChunk
	for _, r := range t.reasoning {
		chunks = append(chunks, provider.StreamChunk{
			Kind: provider.ChunkReasoning,
			Text: r.Content,
		})
	}
	if t.message.Text != "" {
		chunks = append(chunks, provider.StreamChunk{
			Kind: provider.ChunkText,
			Text: t.message.Text,
		})
	}
	for _, tu := range t.message.ToolUses {
		argsJSON, err := cborArgsToJSON(tu.Args)
		if err != nil {
			return nil, fmt.Errorf("replay: tool %q (call %s) args CBOR→JSON: %w", tu.ToolName, tu.CallID, err)
		}
		chunks = append(chunks,
			provider.StreamChunk{
				Kind:    provider.ChunkToolUseStart,
				ToolUse: &provider.ToolUseChunk{CallID: tu.CallID, Name: tu.ToolName},
			},
			provider.StreamChunk{
				Kind:    provider.ChunkToolUseDelta,
				ToolUse: &provider.ToolUseChunk{CallID: tu.CallID, ArgsDelta: argsJSON},
			},
			provider.StreamChunk{
				Kind:    provider.ChunkToolUseEnd,
				ToolUse: &provider.ToolUseChunk{CallID: tu.CallID},
			},
		)
	}
	chunks = append(chunks, provider.StreamChunk{
		Kind: provider.ChunkUsage,
		Usage: &provider.UsageUpdate{
			InputTokens:       t.message.InputTokens,
			OutputTokens:      t.message.OutputTokens,
			CacheReadTokens:   t.message.CacheReadTokens,
			CacheCreateTokens: t.message.CacheCreateTokens,
		},
	})
	chunks = append(chunks, provider.StreamChunk{
		Kind:            provider.ChunkEnd,
		StopReason:      t.message.StopReason,
		RawResponseHash: t.message.RawResponseHash,
		ProviderReqID:   t.message.ProviderRequestID,
	})
	return chunks, nil
}

// cborArgsToJSON decodes CBOR-encoded tool args into a generic value
// and re-encodes as JSON. step.LLMCall will convert the JSON back to
// canonical CBOR on aggregation — that round-trip is stable because
// the canonical encoder sorts map keys, so the resulting
// PlannedToolUse.Args byte-matches the recorded value.
func cborArgsToJSON(args cborenc.RawMessage) (string, error) {
	if len(args) == 0 {
		return "{}", nil
	}
	// Decode into map[string]any directly — tool-use args are always
	// JSON objects at the provider boundary, and map[string]any is the
	// one CBOR target type that encoding/json can marshal without
	// follow-up conversion (the default map[interface{}]interface{}
	// cannot).
	var v map[string]any
	if err := cborenc.Unmarshal(args, &v); err != nil {
		return "", err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
