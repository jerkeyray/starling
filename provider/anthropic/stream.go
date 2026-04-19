package anthropic

import (
	"context"
	"errors"
	"io"
	"net/http"

	anth "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"
	"github.com/zeebo/blake3"

	"github.com/jerkeyray/starling/provider"
)

// anthropicStream adapts an *ssestream.Stream[anth.MessageStreamEventUnion]
// to the provider.EventStream contract. It tracks per-content-block
// state (the SDK reports block index only, not the block type, after
// the initial content_block_start) and emits normalized StreamChunk
// values one at a time via Next.
type anthropicStream struct {
	sdk      *ssestream.Stream[anth.MessageStreamEventUnion]
	httpResp **http.Response

	queue []provider.StreamChunk

	// blocks tracks open content blocks by their stream-local index so
	// content_block_delta and content_block_stop events can route back
	// to the right accumulator. Populated on content_block_start and
	// cleared on content_block_stop.
	blocks map[int64]*openBlock

	// Running state that turns into ChunkUsage + ChunkEnd at message_stop.
	usage         provider.UsageUpdate
	usageSeen     bool
	stopReason    string
	providerReqID string

	rawHash *blake3.Hasher

	firstEventSeen bool
	sdkDone        bool
	doneEnd        bool
	closed         bool
}

// blockKind enumerates which content-block variant is currently open at
// a given stream index. We need this because content_block_delta events
// carry only the delta payload — they don't re-identify the block type
// — so the state machine must remember it.
type blockKind uint8

const (
	blockText blockKind = iota + 1
	blockToolUse
	blockThinking
	blockRedactedThinking
)

type openBlock struct {
	kind         blockKind
	callID       string // tool_use only
	signature    []byte // thinking / redacted_thinking: buffered signature_delta payload
	redactedData string // redacted_thinking only: opaque payload from content_block_start
}

func newAnthropicStream(sdk *ssestream.Stream[anth.MessageStreamEventUnion], httpResp **http.Response) *anthropicStream {
	return &anthropicStream{
		sdk:      sdk,
		httpResp: httpResp,
		blocks:   make(map[int64]*openBlock),
		rawHash:  blake3.New(),
	}
}

// Next returns the next normalized StreamChunk, or io.EOF after the
// terminal ChunkEnd has been delivered.
func (s *anthropicStream) Next(ctx context.Context) (provider.StreamChunk, error) {
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

		if !s.sdk.Next() {
			if err := s.sdk.Err(); err != nil && !errors.Is(err, io.EOF) {
				return provider.StreamChunk{}, err
			}
			s.sdkDone = true
			continue
		}

		s.handleEvent(s.sdk.Current())
	}
}

// handleEvent dispatches one SDK event into zero or more normalized
// StreamChunks. The SDK presents every event type as a single union
// (MessageStreamEventUnion); we branch on Type.
func (s *anthropicStream) handleEvent(ev anth.MessageStreamEventUnion) {
	// Feed the raw SSE JSON bytes into the BLAKE3 hasher so
	// RawResponseHash depends on the on-wire payload, not on the SDK's
	// struct layout. Two replays of the same server bytes produce the
	// same digest.
	if raw := ev.RawJSON(); raw != "" {
		_, _ = s.rawHash.Write([]byte(raw))
	}

	if !s.firstEventSeen {
		s.firstEventSeen = true
		if s.httpResp != nil && *s.httpResp != nil {
			s.providerReqID = (*s.httpResp).Header.Get("request-id")
		}
	}

	switch ev.Type {
	case "message_start":
		m := ev.AsMessageStart()
		if m.Message.ID != "" {
			// Prefer the message ID when the HTTP header didn't
			// carry a request-id (some compat proxies strip it).
			if s.providerReqID == "" {
				s.providerReqID = m.Message.ID
			}
		}
		s.usage.InputTokens = m.Message.Usage.InputTokens
		s.usage.CacheCreateTokens = m.Message.Usage.CacheCreationInputTokens
		s.usage.CacheReadTokens = m.Message.Usage.CacheReadInputTokens
		s.usageSeen = true

	case "content_block_start":
		s.handleBlockStart(ev)

	case "content_block_delta":
		s.handleBlockDelta(ev)

	case "content_block_stop":
		s.handleBlockStop(ev)

	case "message_delta":
		d := ev.AsMessageDelta()
		if string(d.Delta.StopReason) != "" {
			s.stopReason = string(d.Delta.StopReason)
		}
		// message_delta carries cumulative output + cache tokens.
		s.usage.OutputTokens = d.Usage.OutputTokens
		if d.Usage.CacheCreationInputTokens > 0 {
			s.usage.CacheCreateTokens = d.Usage.CacheCreationInputTokens
		}
		if d.Usage.CacheReadInputTokens > 0 {
			s.usage.CacheReadTokens = d.Usage.CacheReadInputTokens
		}
		if d.Usage.InputTokens > 0 {
			// message_delta's input_tokens is cumulative too; take the
			// max so a late update doesn't clobber message_start.
			if d.Usage.InputTokens > s.usage.InputTokens {
				s.usage.InputTokens = d.Usage.InputTokens
			}
		}
		s.usageSeen = true

	case "message_stop":
		// No payload to act on — flushTerminal runs when sdkDone flips.

	case "ping":
		// Keep-alive; ignored.
	}
}

func (s *anthropicStream) handleBlockStart(ev anth.MessageStreamEventUnion) {
	start := ev.AsContentBlockStart()
	cb := start.ContentBlock
	switch cb.Type {
	case "text":
		s.blocks[start.Index] = &openBlock{kind: blockText}

	case "tool_use":
		b := &openBlock{kind: blockToolUse, callID: cb.ID}
		s.blocks[start.Index] = b
		s.queue = append(s.queue, provider.StreamChunk{
			Kind: provider.ChunkToolUseStart,
			ToolUse: &provider.ToolUseChunk{
				CallID: cb.ID,
				Name:   cb.Name,
			},
		})

	case "thinking":
		s.blocks[start.Index] = &openBlock{kind: blockThinking}
		// A non-streamed initial thinking payload can arrive here
		// (rare, but spec-legal). Emit it as a reasoning text chunk.
		if cb.Thinking != "" {
			s.queue = append(s.queue, provider.StreamChunk{
				Kind: provider.ChunkReasoning,
				Text: cb.Thinking,
			})
		}

	case "redacted_thinking":
		// Redacted-thinking blocks are self-contained: the opaque
		// payload arrives on start, not as a delta. We hold it here
		// and emit one ChunkRedactedThinking on content_block_stop,
		// paired with any signature that may follow via
		// signature_delta.
		s.blocks[start.Index] = &openBlock{kind: blockRedactedThinking}
		// Stash the opaque data in a reasoning-text slot on the open
		// block via a side-channel: reuse the signature accumulator
		// pattern below. We add a dedicated field.
		s.blocks[start.Index].redactedData = cb.Data

	default:
		// Server-hosted tools and other block types are deferred —
		// open a sentinel so deltas and stop events for this index
		// are swallowed rather than misrouted.
		s.blocks[start.Index] = &openBlock{kind: 0}
	}
}

func (s *anthropicStream) handleBlockDelta(ev anth.MessageStreamEventUnion) {
	delta := ev.AsContentBlockDelta()
	b, ok := s.blocks[delta.Index]
	if !ok {
		return
	}
	d := delta.Delta
	switch d.Type {
	case "text_delta":
		if b.kind == blockText && d.Text != "" {
			s.queue = append(s.queue, provider.StreamChunk{
				Kind: provider.ChunkText,
				Text: d.Text,
			})
		}

	case "input_json_delta":
		if b.kind == blockToolUse && d.PartialJSON != "" {
			s.queue = append(s.queue, provider.StreamChunk{
				Kind: provider.ChunkToolUseDelta,
				ToolUse: &provider.ToolUseChunk{
					CallID:    b.callID,
					ArgsDelta: d.PartialJSON,
				},
			})
		}

	case "thinking_delta":
		if b.kind == blockThinking && d.Thinking != "" {
			s.queue = append(s.queue, provider.StreamChunk{
				Kind: provider.ChunkReasoning,
				Text: d.Thinking,
			})
		}

	case "signature_delta":
		// Signature bytes are carried as a base64-ish string in the
		// wire format; pass through opaquely. They attach to the
		// currently-open thinking or redacted_thinking block and are
		// flushed on content_block_stop.
		if d.Signature != "" {
			b.signature = append(b.signature, []byte(d.Signature)...)
		}

	case "citations_delta":
		// Deferred — citations land in a follow-up task once we have
		// a unified cross-provider citation abstraction.
	}
}

func (s *anthropicStream) handleBlockStop(ev anth.MessageStreamEventUnion) {
	stop := ev.AsContentBlockStop()
	b, ok := s.blocks[stop.Index]
	if !ok {
		return
	}
	delete(s.blocks, stop.Index)

	switch b.kind {
	case blockToolUse:
		s.queue = append(s.queue, provider.StreamChunk{
			Kind:    provider.ChunkToolUseEnd,
			ToolUse: &provider.ToolUseChunk{CallID: b.callID},
		})

	case blockThinking:
		// Close the block by emitting a signature-only ChunkReasoning.
		// step.LLMCall uses it as the flush signal to combine all
		// buffered thinking_delta text + this signature into one
		// ReasoningEmitted event.
		s.queue = append(s.queue, provider.StreamChunk{
			Kind:      provider.ChunkReasoning,
			Signature: b.signature,
		})

	case blockRedactedThinking:
		s.queue = append(s.queue, provider.StreamChunk{
			Kind:      provider.ChunkRedactedThinking,
			Text:      b.redactedData,
			Signature: b.signature,
		})
	}
}

// flushTerminal emits the ChunkUsage + ChunkEnd pair once the SDK
// stream has drained. Called exactly once.
func (s *anthropicStream) flushTerminal() {
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

// Close releases SDK resources. Safe to call more than once.
func (s *anthropicStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.sdk.Close()
}
