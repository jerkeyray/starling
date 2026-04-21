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

// geminiStream adapts the genai SDK's iter.Seq2 stream to
// provider.EventStream, normalizing Gemini's quirks: tool-call args
// arrive complete in one part (emitted as Start+Delta+End),
// UsageMetadata lands only on the final frame, and a finished tool
// call reports STOP (remapped to "tool_use" when any functionCall
// was seen).
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

// handleResponse translates one streamed frame into zero or more
// StreamChunks.
func (s *geminiStream) handleResponse(resp *genai.GenerateContentResponse) {
	if resp == nil {
		return
	}

	// Hash marshaled frames (SDK doesn't expose raw bytes) so
	// RawResponseHash is deterministic across replays.
	if raw, err := json.Marshal(resp); err == nil {
		_, _ = s.rawHash.Write(raw)
	}

	if s.providerReqID == "" && resp.ResponseID != "" {
		s.providerReqID = resp.ResponseID
	}

	// UsageMetadata usually only lands on the final frame; overwrite
	// on every non-nil occurrence.
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
// ChunkText; FunctionCall parts become the canonical Start+Delta+End
// trio. Other part kinds (executable code, inline/file data) are
// ignored.
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
			// Gemini sometimes omits the ID; fall back to the name.
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
		// Drop thought-trace parts; ChunkReasoning's shape is
		// Anthropic-specific.
		if p.Thought {
			return
		}
		s.queue = append(s.queue, provider.StreamChunk{
			Kind: provider.ChunkText,
			Text: p.Text,
		})
	}
}

// translateFinishReason maps Gemini's FinishReason to the portable
// stop-reason string. Any turn with a functionCall normalizes to
// "tool_use" (Gemini reports STOP for tool-call completions).
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

// flushTerminal emits ChunkUsage + ChunkEnd once. Called exactly once.
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
