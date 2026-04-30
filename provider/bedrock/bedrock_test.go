package bedrock

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/jerkeyray/starling/internal/cborenc"
	"github.com/jerkeyray/starling/provider"
)

type fakeClient struct {
	out *converseOutput
	err error
	got *bedrockruntime.ConverseStreamInput
}

func (f *fakeClient) ConverseStream(_ context.Context, in *bedrockruntime.ConverseStreamInput) (*converseOutput, error) {
	f.got = in
	if f.err != nil {
		return nil, f.err
	}
	return f.out, nil
}

type fakeReader struct {
	ch     chan types.ConverseStreamOutput
	err    error
	closed bool
}

func newFakeReader(events ...types.ConverseStreamOutput) *fakeReader {
	ch := make(chan types.ConverseStreamOutput, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return &fakeReader{ch: ch}
}

func (r *fakeReader) Events() <-chan types.ConverseStreamOutput { return r.ch }
func (r *fakeReader) Close() error {
	r.closed = true
	return nil
}
func (r *fakeReader) Err() error { return r.err }

func testStream(events ...types.ConverseStreamOutput) *bedrockruntime.ConverseStreamEventStream {
	reader := newFakeReader(events...)
	return bedrockruntime.NewConverseStreamEventStream(func(es *bedrockruntime.ConverseStreamEventStream) {
		es.Reader = reader
	})
}

func drain(t *testing.T, s provider.EventStream) []provider.StreamChunk {
	t.Helper()
	var out []provider.StreamChunk
	for {
		c, err := s.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, c)
	}
}

func TestBuildInput_TextToolsAndParams(t *testing.T) {
	topK := 200
	raw, err := cborenc.Marshal(map[string]any{
		"temperature":                       0.4,
		"topP":                              0.8,
		"additionalModelResponseFieldPaths": []any{"/stop_sequence"},
		"additionalModelRequestFields":      map[string]any{"anthropic_version": "bedrock-2023-05-31"},
		"requestMetadata":                   map[string]any{"run": "test"},
		"performanceConfig":                 map[string]any{"latency": "optimized"},
		"serviceTier":                       map[string]any{"type": "priority"},
		"promptVariables":                   map[string]any{"topic": "bedrock"},
		"guardrailConfig": map[string]any{
			"identifier":           "gr-1",
			"version":              "1",
			"streamProcessingMode": "async",
			"trace":                "enabled",
		},
		"outputConfig": map[string]any{
			"textFormat": map[string]any{
				"type": "json_schema",
				"structure": map[string]any{
					"jsonSchema": map[string]any{
						"name":   "answer",
						"schema": map[string]any{"type": "object"},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}

	input, err := buildInput(&provider.Request{
		Model:           "anthropic.claude-3-5-sonnet-20241022-v2:0",
		SystemPrompt:    "be brief",
		MaxOutputTokens: 256,
		StopSequences:   []string{"STOP"},
		TopK:            &topK,
		ToolChoice:      "search",
		Params:          raw,
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: "hi"},
		},
		Tools: []provider.ToolDefinition{{
			Name:        "search",
			Description: "search docs",
			Schema:      []byte(`{"type":"object","properties":{"q":{"type":"string"}}}`),
		}},
	})
	if err != nil {
		t.Fatalf("buildInput: %v", err)
	}
	if input.ModelId == nil || *input.ModelId != "anthropic.claude-3-5-sonnet-20241022-v2:0" {
		t.Fatalf("ModelId = %v", input.ModelId)
	}
	if len(input.System) != 1 {
		t.Fatalf("System len = %d, want 1", len(input.System))
	}
	if input.InferenceConfig == nil || input.InferenceConfig.MaxTokens == nil || *input.InferenceConfig.MaxTokens != 256 {
		t.Fatalf("MaxTokens mismatch: %+v", input.InferenceConfig)
	}
	if input.InferenceConfig.Temperature == nil || *input.InferenceConfig.Temperature != 0.4 {
		t.Fatalf("Temperature = %v, want 0.4", input.InferenceConfig.Temperature)
	}
	if input.InferenceConfig.TopP == nil || *input.InferenceConfig.TopP != 0.8 {
		t.Fatalf("TopP = %v, want 0.8", input.InferenceConfig.TopP)
	}
	if !reflect.DeepEqual(input.AdditionalModelResponseFieldPaths, []string{"/stop_sequence"}) {
		t.Fatalf("AdditionalModelResponseFieldPaths = %#v", input.AdditionalModelResponseFieldPaths)
	}
	if input.RequestMetadata["run"] != "test" {
		t.Fatalf("RequestMetadata = %#v", input.RequestMetadata)
	}
	if input.PerformanceConfig == nil || input.PerformanceConfig.Latency != types.PerformanceConfigLatencyOptimized {
		t.Fatalf("PerformanceConfig = %#v", input.PerformanceConfig)
	}
	if input.ServiceTier == nil || input.ServiceTier.Type != types.ServiceTierTypePriority {
		t.Fatalf("ServiceTier = %#v", input.ServiceTier)
	}
	if input.PromptVariables["topic"].(*types.PromptVariableValuesMemberText).Value != "bedrock" {
		t.Fatalf("PromptVariables = %#v", input.PromptVariables)
	}
	if input.GuardrailConfig == nil || input.GuardrailConfig.GuardrailIdentifier == nil ||
		*input.GuardrailConfig.GuardrailIdentifier != "gr-1" {
		t.Fatalf("GuardrailConfig = %#v", input.GuardrailConfig)
	}
	if input.OutputConfig == nil || input.OutputConfig.TextFormat == nil {
		t.Fatalf("OutputConfig = %#v", input.OutputConfig)
	}
	js, ok := input.OutputConfig.TextFormat.Structure.(*types.OutputFormatStructureMemberJsonSchema)
	if !ok || js.Value.Name == nil || *js.Value.Name != "answer" {
		t.Fatalf("OutputConfig schema = %#v", input.OutputConfig.TextFormat.Structure)
	}
	extra := docMap(t, input.AdditionalModelRequestFields)
	if extra["top_k"] != float64(200) && extra["top_k"] != uint64(200) && extra["top_k"] != int64(200) {
		t.Fatalf("top_k = %#v", extra["top_k"])
	}
	if extra["anthropic_version"] != "bedrock-2023-05-31" {
		t.Fatalf("anthropic_version = %#v", extra["anthropic_version"])
	}
	if input.ToolConfig == nil || len(input.ToolConfig.Tools) != 1 {
		t.Fatalf("ToolConfig mismatch: %+v", input.ToolConfig)
	}
	if _, ok := input.ToolConfig.ToolChoice.(*types.ToolChoiceMemberTool); !ok {
		t.Fatalf("ToolChoice type = %T, want ToolChoiceMemberTool", input.ToolConfig.ToolChoice)
	}
}

func TestBuildInput_RejectsUnsupportedParam(t *testing.T) {
	raw, err := cborenc.Marshal(map[string]any{"guardrailTrace": "enabled"})
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	_, err = buildInput(&provider.Request{Model: "m", Params: raw})
	if err == nil || !strings.Contains(err.Error(), `unsupported Request.Params key "guardrailTrace"`) {
		t.Fatalf("buildInput err = %v, want unsupported key", err)
	}
}

func TestBuildInput_ToolRoundTripMessages(t *testing.T) {
	input, err := buildInput(&provider.Request{
		Model: "m",
		Messages: []provider.Message{
			{Role: provider.RoleAssistant, ToolUses: []provider.ToolUse{{
				CallID: "call-1",
				Name:   "search",
				Args:   []byte(`{"q":"go"}`),
			}}},
			{Role: provider.RoleTool, ToolResult: &provider.ToolResult{
				CallID:  "call-1",
				Content: "result",
			}},
		},
	})
	if err != nil {
		t.Fatalf("buildInput: %v", err)
	}
	if len(input.Messages) != 2 {
		t.Fatalf("Messages len = %d, want 2", len(input.Messages))
	}
	tu, ok := input.Messages[0].Content[0].(*types.ContentBlockMemberToolUse)
	if !ok {
		t.Fatalf("assistant content = %T, want tool use", input.Messages[0].Content[0])
	}
	args := docMap(t, tu.Value.Input)
	if args["q"] != "go" {
		t.Fatalf("tool args = %#v", args)
	}
	tr, ok := input.Messages[1].Content[0].(*types.ContentBlockMemberToolResult)
	if !ok {
		t.Fatalf("tool content = %T, want tool result", input.Messages[1].Content[0])
	}
	if tr.Value.Status != types.ToolResultStatusSuccess {
		t.Fatalf("status = %q", tr.Value.Status)
	}
}

func TestStream_TextToolReasoningUsageEnd(t *testing.T) {
	idx0, idx1 := int32(0), int32(1)
	inTok, outTok, cacheRead, cacheWrite := int32(10), int32(5), int32(2), int32(3)
	s := newBedrockStream(testStream(
		&types.ConverseStreamOutputMemberContentBlockDelta{Value: types.ContentBlockDeltaEvent{
			ContentBlockIndex: &idx0,
			Delta:             &types.ContentBlockDeltaMemberText{Value: "hi"},
		}},
		&types.ConverseStreamOutputMemberContentBlockStart{Value: types.ContentBlockStartEvent{
			ContentBlockIndex: &idx1,
			Start: &types.ContentBlockStartMemberToolUse{Value: types.ToolUseBlockStart{
				ToolUseId: stringPtr("call-1"),
				Name:      stringPtr("search"),
			}},
		}},
		&types.ConverseStreamOutputMemberContentBlockDelta{Value: types.ContentBlockDeltaEvent{
			ContentBlockIndex: &idx1,
			Delta:             &types.ContentBlockDeltaMemberToolUse{Value: types.ToolUseBlockDelta{Input: stringPtr(`{"q":"go"}`)}},
		}},
		&types.ConverseStreamOutputMemberContentBlockStop{Value: types.ContentBlockStopEvent{ContentBlockIndex: &idx1}},
		&types.ConverseStreamOutputMemberContentBlockDelta{Value: types.ContentBlockDeltaEvent{
			ContentBlockIndex: &idx0,
			Delta: &types.ContentBlockDeltaMemberReasoningContent{Value: &types.ReasoningContentBlockDeltaMemberText{
				Value: "thinking",
			}},
		}},
		&types.ConverseStreamOutputMemberContentBlockDelta{Value: types.ContentBlockDeltaEvent{
			ContentBlockIndex: &idx0,
			Delta: &types.ContentBlockDeltaMemberReasoningContent{Value: &types.ReasoningContentBlockDeltaMemberSignature{
				Value: "sig",
			}},
		}},
		&types.ConverseStreamOutputMemberContentBlockStop{Value: types.ContentBlockStopEvent{ContentBlockIndex: &idx0}},
		&types.ConverseStreamOutputMemberMessageStop{Value: types.MessageStopEvent{StopReason: types.StopReasonToolUse}},
		&types.ConverseStreamOutputMemberMetadata{Value: types.ConverseStreamMetadataEvent{Usage: &types.TokenUsage{
			InputTokens:           &inTok,
			OutputTokens:          &outTok,
			CacheReadInputTokens:  &cacheRead,
			CacheWriteInputTokens: &cacheWrite,
		}}},
	), "req-1")

	chunks := drain(t, s)
	wantKinds := []provider.ChunkKind{
		provider.ChunkText,
		provider.ChunkToolUseStart,
		provider.ChunkToolUseDelta,
		provider.ChunkToolUseEnd,
		provider.ChunkReasoning,
		provider.ChunkReasoning,
		provider.ChunkUsage,
		provider.ChunkEnd,
	}
	if len(chunks) != len(wantKinds) {
		t.Fatalf("got %d chunks, want %d: %+v", len(chunks), len(wantKinds), chunks)
	}
	for i, want := range wantKinds {
		if chunks[i].Kind != want {
			t.Fatalf("chunk[%d] kind = %s, want %s", i, chunks[i].Kind, want)
		}
	}
	if chunks[2].ToolUse.ArgsDelta != `{"q":"go"}` {
		t.Fatalf("tool args delta = %q", chunks[2].ToolUse.ArgsDelta)
	}
	if string(chunks[5].Signature) != "sig" {
		t.Fatalf("reasoning signature = %q", chunks[5].Signature)
	}
	if chunks[6].Usage.InputTokens != 10 || chunks[6].Usage.OutputTokens != 5 ||
		chunks[6].Usage.CacheReadTokens != 2 || chunks[6].Usage.CacheCreateTokens != 3 {
		t.Fatalf("usage = %+v", chunks[6].Usage)
	}
	if chunks[7].StopReason != "tool_use" || chunks[7].ProviderReqID != "req-1" {
		t.Fatalf("end = %+v", chunks[7])
	}
	if len(chunks[7].RawResponseHash) != 32 {
		t.Fatalf("RawResponseHash len = %d, want 32", len(chunks[7].RawResponseHash))
	}
}

func TestStream_RedactedReasoning(t *testing.T) {
	idx := int32(0)
	s := newBedrockStream(testStream(
		&types.ConverseStreamOutputMemberContentBlockDelta{Value: types.ContentBlockDeltaEvent{
			ContentBlockIndex: &idx,
			Delta: &types.ContentBlockDeltaMemberReasoningContent{Value: &types.ReasoningContentBlockDeltaMemberRedactedContent{
				Value: []byte{0xff, 0x00, 0x01},
			}},
		}},
		&types.ConverseStreamOutputMemberContentBlockStop{Value: types.ContentBlockStopEvent{ContentBlockIndex: &idx}},
	), "")

	chunks := drain(t, s)
	if chunks[0].Kind != provider.ChunkRedactedThinking || chunks[0].Text != "/wAB" {
		t.Fatalf("redacted chunk = %+v", chunks[0])
	}
}

func stringPtr(s string) *string { return &s }

func docMap(t *testing.T, d document.Interface) map[string]any {
	t.Helper()
	var out map[string]any
	b, err := d.MarshalSmithyDocument()
	if err != nil {
		t.Fatalf("marshal document: %v", err)
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode document: %v", err)
	}
	return out
}
