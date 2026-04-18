package openai

import (
	"context"
	"errors"
	"io"
	"net/http"

	oai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/zeebo/blake3"

	"github.com/jerkeyray/starling/provider"
)

// openaiStream adapts an *ssestream.Stream[oai.ChatCompletionChunk] to the
// provider.EventStream contract. It buffers normalized StreamChunk values
// produced from each SDK chunk and emits them one at a time via Next.
type openaiStream struct {
	sdk      *ssestream.Stream[oai.ChatCompletionChunk]
	httpResp **http.Response

	queue []provider.StreamChunk

	rawHash   *blake3.Hasher
	openTools []openTool
	toolIdx   map[int64]int // SDK tool-call index -> position in openTools

	providerReqID string
	stopReason    string
	finalUsage    *provider.UsageUpdate

	firstChunkSeen bool
	sdkDone        bool
	doneEnd        bool
	closed         bool
}

type openTool struct {
	index  int64
	callID string
}

func newOpenAIStream(sdk *ssestream.Stream[oai.ChatCompletionChunk], httpResp **http.Response) *openaiStream {
	return &openaiStream{
		sdk:      sdk,
		httpResp: httpResp,
		rawHash:  blake3.New(),
		toolIdx:  make(map[int64]int),
	}
}

// Next returns the next normalized StreamChunk, or io.EOF after the terminal
// ChunkEnd has been delivered.
func (s *openaiStream) Next(ctx context.Context) (provider.StreamChunk, error) {
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
			// No more SDK chunks; synthesize terminal chunks once.
			s.flushTerminal()
			continue
		}

		if !s.sdk.Next() {
			if err := s.sdk.Err(); err != nil && !errors.Is(err, io.EOF) {
				return provider.StreamChunk{}, err
			}
			s.sdkDone = true
			continue
		}

		s.handleChunk(s.sdk.Current())
	}
}

// handleChunk translates one SDK chunk into zero or more normalized chunks
// appended to s.queue, and updates running state (usage, stop reason, raw
// hash).
func (s *openaiStream) handleChunk(chunk oai.ChatCompletionChunk) {
	if !s.firstChunkSeen {
		s.firstChunkSeen = true
		if s.httpResp != nil && *s.httpResp != nil {
			s.providerReqID = (*s.httpResp).Header.Get("x-request-id")
		}
	}

	// Contribute to RawResponseHash using the SDK's unmodified wire bytes.
	// That keeps the hash reproducible across SDK versions — two replays of
	// the same server response produce the same digest.
	if raw := chunk.RawJSON(); raw != "" {
		_, _ = s.rawHash.Write([]byte(raw))
	}

	// Terminal usage chunk: empty Choices + populated Usage.
	if len(chunk.Choices) == 0 {
		if chunk.Usage.TotalTokens > 0 {
			s.finalUsage = &provider.UsageUpdate{
				InputTokens:     chunk.Usage.PromptTokens,
				OutputTokens:    chunk.Usage.CompletionTokens,
				CacheReadTokens: chunk.Usage.PromptTokensDetails.CachedTokens,
			}
		}
		return
	}

	for _, choice := range chunk.Choices {
		s.handleChoice(choice)
	}
}

func (s *openaiStream) handleChoice(choice oai.ChatCompletionChunkChoice) {
	if choice.Delta.Content != "" {
		s.queue = append(s.queue, provider.StreamChunk{
			Kind: provider.ChunkText,
			Text: choice.Delta.Content,
		})
	}

	for _, tc := range choice.Delta.ToolCalls {
		pos, known := s.toolIdx[tc.Index]
		if !known {
			callID := tc.ID
			s.openTools = append(s.openTools, openTool{index: tc.Index, callID: callID})
			pos = len(s.openTools) - 1
			s.toolIdx[tc.Index] = pos
			s.queue = append(s.queue, provider.StreamChunk{
				Kind: provider.ChunkToolUseStart,
				ToolUse: &provider.ToolUseChunk{
					CallID: callID,
					Name:   tc.Function.Name,
				},
			})
		}
		if tc.Function.Arguments != "" {
			s.queue = append(s.queue, provider.StreamChunk{
				Kind: provider.ChunkToolUseDelta,
				ToolUse: &provider.ToolUseChunk{
					CallID:    s.openTools[pos].callID,
					ArgsDelta: tc.Function.Arguments,
				},
			})
		}
	}

	if choice.FinishReason != "" {
		// Last finish_reason wins; we assume N=1 (openai-go default).
		s.stopReason = choice.FinishReason
	}
}

// flushTerminal appends ChunkToolUseEnd (for each open tool), ChunkUsage
// (if final usage was seen), and ChunkEnd to the queue, then marks the
// stream as done.
func (s *openaiStream) flushTerminal() {
	for _, t := range s.openTools {
		s.queue = append(s.queue, provider.StreamChunk{
			Kind:    provider.ChunkToolUseEnd,
			ToolUse: &provider.ToolUseChunk{CallID: t.callID},
		})
	}
	s.openTools = nil
	s.toolIdx = nil

	if s.finalUsage != nil {
		s.queue = append(s.queue, provider.StreamChunk{
			Kind:  provider.ChunkUsage,
			Usage: s.finalUsage,
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

// Close releases SDK resources. Safe to call more than once.
func (s *openaiStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.sdk.Close()
}
