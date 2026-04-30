package bedrock

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/zeebo/blake3"

	"github.com/jerkeyray/starling/provider"
)

type bedrockStream struct {
	sdk           *bedrockruntime.ConverseStreamEventStream
	events        <-chan types.ConverseStreamOutput
	providerReqID string

	queue []provider.StreamChunk

	blocks map[int32]*openBlock
	usage  *provider.UsageUpdate
	stop   string

	rawHash *blake3.Hasher
	doneEnd bool
	closed  bool
}

type blockKind uint8

const (
	blockToolUse blockKind = iota + 1
	blockReasoning
	blockRedactedReasoning
)

type openBlock struct {
	kind         blockKind
	callID       string
	signature    []byte
	redactedData string
}

func newBedrockStream(sdk *bedrockruntime.ConverseStreamEventStream, requestID string) *bedrockStream {
	return &bedrockStream{
		sdk:           sdk,
		events:        sdk.Events(),
		providerReqID: requestID,
		blocks:        map[int32]*openBlock{},
		rawHash:       blake3.New(),
	}
}

func (s *bedrockStream) Next(ctx context.Context) (provider.StreamChunk, error) {
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

		select {
		case <-ctx.Done():
			return provider.StreamChunk{}, ctx.Err()
		case ev, ok := <-s.events:
			if !ok {
				if err := s.sdk.Err(); err != nil && !errors.Is(err, io.EOF) {
					return provider.StreamChunk{}, err
				}
				s.flushTerminal()
				continue
			}
			s.handleEvent(ev)
		}
	}
}

func (s *bedrockStream) handleEvent(ev types.ConverseStreamOutput) {
	s.hashEvent(ev)
	switch e := ev.(type) {
	case *types.ConverseStreamOutputMemberContentBlockStart:
		s.handleBlockStart(e.Value)
	case *types.ConverseStreamOutputMemberContentBlockDelta:
		s.handleBlockDelta(e.Value)
	case *types.ConverseStreamOutputMemberContentBlockStop:
		s.handleBlockStop(e.Value)
	case *types.ConverseStreamOutputMemberMessageStop:
		s.stop = string(e.Value.StopReason)
	case *types.ConverseStreamOutputMemberMetadata:
		if e.Value.Usage != nil {
			u := provider.UsageUpdate{}
			if e.Value.Usage.InputTokens != nil {
				u.InputTokens = int64(*e.Value.Usage.InputTokens)
			}
			if e.Value.Usage.OutputTokens != nil {
				u.OutputTokens = int64(*e.Value.Usage.OutputTokens)
			}
			if e.Value.Usage.CacheReadInputTokens != nil {
				u.CacheReadTokens = int64(*e.Value.Usage.CacheReadInputTokens)
			}
			if e.Value.Usage.CacheWriteInputTokens != nil {
				u.CacheCreateTokens = int64(*e.Value.Usage.CacheWriteInputTokens)
			}
			s.usage = &u
		}
	case *types.ConverseStreamOutputMemberMessageStart:
		// Role is implicit in Starling's stream model.
	}
}

func (s *bedrockStream) handleBlockStart(ev types.ContentBlockStartEvent) {
	if ev.ContentBlockIndex == nil {
		return
	}
	switch start := ev.Start.(type) {
	case *types.ContentBlockStartMemberToolUse:
		id := deref(start.Value.ToolUseId)
		s.blocks[*ev.ContentBlockIndex] = &openBlock{kind: blockToolUse, callID: id}
		s.queue = append(s.queue, provider.StreamChunk{
			Kind: provider.ChunkToolUseStart,
			ToolUse: &provider.ToolUseChunk{
				CallID: id,
				Name:   deref(start.Value.Name),
			},
		})
	default:
		s.blocks[*ev.ContentBlockIndex] = &openBlock{}
	}
}

func (s *bedrockStream) handleBlockDelta(ev types.ContentBlockDeltaEvent) {
	if ev.ContentBlockIndex == nil {
		return
	}
	idx := *ev.ContentBlockIndex
	switch d := ev.Delta.(type) {
	case *types.ContentBlockDeltaMemberText:
		if d.Value != "" {
			s.queue = append(s.queue, provider.StreamChunk{Kind: provider.ChunkText, Text: d.Value})
		}
	case *types.ContentBlockDeltaMemberToolUse:
		b := s.blocks[idx]
		if b == nil || b.kind != blockToolUse {
			return
		}
		s.queue = append(s.queue, provider.StreamChunk{
			Kind: provider.ChunkToolUseDelta,
			ToolUse: &provider.ToolUseChunk{
				CallID:    b.callID,
				ArgsDelta: deref(d.Value.Input),
			},
		})
	case *types.ContentBlockDeltaMemberReasoningContent:
		s.handleReasoningDelta(idx, d.Value)
	}
}

func (s *bedrockStream) handleReasoningDelta(idx int32, d types.ReasoningContentBlockDelta) {
	b := s.blocks[idx]
	if b == nil {
		b = &openBlock{kind: blockReasoning}
		s.blocks[idx] = b
	}
	switch r := d.(type) {
	case *types.ReasoningContentBlockDeltaMemberText:
		b.kind = blockReasoning
		if r.Value != "" {
			s.queue = append(s.queue, provider.StreamChunk{Kind: provider.ChunkReasoning, Text: r.Value})
		}
	case *types.ReasoningContentBlockDeltaMemberSignature:
		b.kind = blockReasoning
		if r.Value != "" {
			b.signature = append(b.signature, []byte(r.Value)...)
		}
	case *types.ReasoningContentBlockDeltaMemberRedactedContent:
		b.kind = blockRedactedReasoning
		if len(r.Value) > 0 {
			b.redactedData += base64.StdEncoding.EncodeToString(r.Value)
		}
	}
}

func (s *bedrockStream) handleBlockStop(ev types.ContentBlockStopEvent) {
	if ev.ContentBlockIndex == nil {
		return
	}
	b := s.blocks[*ev.ContentBlockIndex]
	delete(s.blocks, *ev.ContentBlockIndex)
	if b == nil {
		return
	}
	switch b.kind {
	case blockToolUse:
		s.queue = append(s.queue, provider.StreamChunk{
			Kind:    provider.ChunkToolUseEnd,
			ToolUse: &provider.ToolUseChunk{CallID: b.callID},
		})
	case blockReasoning:
		if len(b.signature) > 0 {
			s.queue = append(s.queue, provider.StreamChunk{
				Kind:      provider.ChunkReasoning,
				Signature: b.signature,
			})
		}
	case blockRedactedReasoning:
		s.queue = append(s.queue, provider.StreamChunk{
			Kind:      provider.ChunkRedactedThinking,
			Text:      b.redactedData,
			Signature: b.signature,
		})
	}
}

func (s *bedrockStream) flushTerminal() {
	if s.usage != nil {
		u := *s.usage
		s.queue = append(s.queue, provider.StreamChunk{Kind: provider.ChunkUsage, Usage: &u})
	}
	s.queue = append(s.queue, provider.StreamChunk{
		Kind:            provider.ChunkEnd,
		StopReason:      s.stop,
		RawResponseHash: s.rawHash.Sum(nil),
		ProviderReqID:   s.providerReqID,
	})
	s.doneEnd = true
}

func (s *bedrockStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.sdk.Close()
}

func (s *bedrockStream) hashEvent(ev types.ConverseStreamOutput) {
	if b, err := json.Marshal(ev); err == nil {
		_, _ = s.rawHash.Write(b)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
