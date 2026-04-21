package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"

	"github.com/zeebo/blake3"
	"google.golang.org/genai"

	"github.com/jerkeyray/starling/provider"
)

// geminiStream adapts the genai SDK's streaming iterator to Starling's
// provider.EventStream. The SDK exposes a range-over-func iterator
// (iter.Seq2[*GenerateContentResponse, error]); we pull one frame at
// a time via iter.Pull2 and translate each frame into zero or more
// normalized StreamChunk values that get enqueued.
//
// Gemini's SSE shape differs from OpenAI/Anthropic in three ways that
// the translator has to smooth out:
//
//  1. Tool-call args arrive complete in one part, not as a stream of
//     deltas. We emit the canonical Start + single Delta + End
//     sequence so step.LLMCall's accumulator handles it identically.
//  2. UsageMetadata is populated only on the final frame, not
//     incrementally like Anthropic's message_delta.
//  3. There is no TOOL_USE finish reason — a tool call still finishes
//     with STOP. We override the normalized StopReason to "tool_use"
//     when at least one functionCall was observed so downstream code
//     can branch identically to OpenAI/Anthropic.
type geminiStream struct {
	next func() (*genai.GenerateContentResponse, error, bool)
	stop func()

	queue []provider.StreamChunk

	// Running state drained on the terminal frame.
	usage         provider.UsageUpdate
	usageSeen     bool
	stopReason    string
	providerReqID string
	sawToolCall   bool

	rawHash *blake3.Hasher

	sdkDone bool
	doneEnd bool
	closed  bool
}

func newGeminiStream(seq iter.Seq2[*genai.GenerateContentResponse, error]) *geminiStream {
	next, stop := iter.Pull2(seq)
	return &geminiStream{
		next:    next,
		stop:    stop,
		rawHash: blake3.New(),
	}
}

// Next returns the next normalized StreamChunk, or io.EOF after the
// terminal ChunkEnd has been delivered.
func (s *geminiStream) Next(ctx context.Context) (provider.StreamChunk, error) {
	for {
		if err := ctx.Err(); err != nil {
			return provider.StreamChunk{}, err
		}

		if len(s.queue) > 0 {
			c := s.queue[0]
			s.queue = s.queue[1:]
			return c, nil
		}

		if s.doneEnd {
			return provider.StreamChunk{}, io.EOF
		}

		if s.sdkDone {
			s.flushTerminal()
			continue
		}

		resp, err, ok := s.next()
		if !ok {
			s.sdkDone = true
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				s.sdkDone = true
				continue
			}
			return provider.StreamChunk{}, err
		}
		s.handleResponse(resp)
	}
}

// handleResponse translates one Gemini streamed response frame into
// zero or more StreamChunks. Each frame is a full
// GenerateContentResponse with partial candidate content.
func (s *geminiStream) handleResponse(resp *genai.GenerateContentResponse) {
	if resp == nil {
		return
	}

	// Hash the marshaled form of each frame so RawResponseHash
	// depends on the payload shape rather than the SDK's struct
	// layout. Two replays of the same server frames yield the same
	// digest. The SDK doesn't expose per-frame raw bytes, so this is
	// the closest deterministic substitute.
	if raw, err := json.Marshal(resp); err == nil {
		_, _ = s.rawHash.Write(raw)
	}

	if s.providerReqID == "" && resp.ResponseID != "" {
		s.providerReqID = resp.ResponseID
	}

	// UsageMetadata normally only lands on the final frame, but some
	// proxies emit it incrementally; take the latest non-nil values.
	if u := resp.UsageMetadata; u != nil {
		s.usage.InputTokens = int64(u.PromptTokenCount)
		s.usage.OutputTokens = int64(u.CandidatesTokenCount)
		s.usage.CacheReadTokens = int64(u.CachedContentTokenCount)
		s.usageSeen = true
	}

	if len(resp.Candidates) == 0 {
		return
	}
	cand := resp.Candidates[0]

	if cand.Content != nil {
		for _, part := range cand.Content.Parts {
			s.handlePart(part)
		}
	}

	if cand.FinishReason != "" {
		s.stopReason = translateFinishReason(cand.FinishReason, s.sawToolCall)
	}
}

// handlePart emits StreamChunks for a single Part. Text parts become
// ChunkText; FunctionCall parts become the canonical Start + Delta +
// End trio. Other part kinds (executable code, code results, thought
// traces, inline/file data) are currently ignored — they'd require
// interface extensions the core doesn't model yet.
func (s *geminiStream) handlePart(p *genai.Part) {
	if p == nil {
		return
	}
	switch {
	case p.FunctionCall != nil:
		fc := p.FunctionCall
		s.sawToolCall = true
		callID := fc.ID
		if callID == "" {
			// The Gemini API doesn't always emit an ID on function
			// calls; fall back to the name so downstream CallID
			// bookkeeping has a stable handle. Multiple unnamed
			// concurrent calls are rare in practice.
			callID = fc.Name
		}
		argsJSON, err := json.Marshal(fc.Args)
		if err != nil {
			argsJSON = []byte("{}")
		}
		s.queue = append(s.queue,
			provider.StreamChunk{
				Kind: provider.ChunkToolUseStart,
				ToolUse: &provider.ToolUseChunk{
					CallID: callID,
					Name:   fc.Name,
				},
			},
			provider.StreamChunk{
				Kind: provider.ChunkToolUseDelta,
				ToolUse: &provider.ToolUseChunk{
					CallID:    callID,
					ArgsDelta: string(argsJSON),
				},
			},
			provider.StreamChunk{
				Kind:    provider.ChunkToolUseEnd,
				ToolUse: &provider.ToolUseChunk{CallID: callID},
			},
		)

	case p.Text != "":
		// Thought-trace parts carry Text too, but are flagged via the
		// Thought bool. Drop them here since provider.ChunkReasoning's
		// semantics (Anthropic-shaped signature plumbing) don't match
		// Gemini's format.
		if p.Thought {
			return
		}
		s.queue = append(s.queue, provider.StreamChunk{
			Kind: provider.ChunkText,
			Text: p.Text,
		})
	}
}

// translateFinishReason maps Gemini's FinishReason enum into the
// portable stop-reason string the step package expects. If a
// functionCall was observed this turn, the normalized stop is
// "tool_use" regardless of the underlying finish — Gemini always
// reports STOP for tool-call completions, but downstream code
// branches on the portable string to pump the tool loop.
func translateFinishReason(r genai.FinishReason, sawToolCall bool) string {
	if sawToolCall && (r == genai.FinishReasonStop || r == "") {
		return "tool_use"
	}
	switch r {
	case genai.FinishReasonStop:
		return "stop"
	case genai.FinishReasonMaxTokens:
		return "max_tokens"
	case genai.FinishReasonSafety,
		genai.FinishReasonRecitation,
		genai.FinishReasonBlocklist,
		genai.FinishReasonProhibitedContent,
		genai.FinishReasonSPII,
		genai.FinishReasonImageSafety:
		return "filtered"
	case genai.FinishReasonMalformedFunctionCall,
		genai.FinishReasonUnexpectedToolCall:
		return "error"
	case genai.FinishReasonLanguage, genai.FinishReasonOther:
		return "other"
	}
	return string(r)
}

// flushTerminal emits the ChunkUsage + ChunkEnd pair once the SDK
// iterator has drained. Called exactly once.
func (s *geminiStream) flushTerminal() {
	if s.usageSeen {
		u := s.usage
		s.queue = append(s.queue, provider.StreamChunk{
			Kind:  provider.ChunkUsage,
			Usage: &u,
		})
	}
	s.queue = append(s.queue, provider.StreamChunk{
		Kind:            provider.ChunkEnd,
		StopReason:      s.stopReason,
		RawResponseHash: s.rawHash.Sum(nil),
		ProviderReqID:   s.providerReqID,
	})
	s.doneEnd = true
}

// Close releases the SDK iterator. Safe to call more than once.
func (s *geminiStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	s.stop()
	return nil
}
