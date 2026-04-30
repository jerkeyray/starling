package bedrock

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/jerkeyray/starling/provider"
	"github.com/jerkeyray/starling/provider/conformance"
)

type confAdapter struct{}

func (confAdapter) Name() string { return "bedrock" }

func (confAdapter) Capabilities() conformance.Capabilities {
	p := &bedrockProvider{cfg: config{providerID: "bedrock", apiVersion: defaultAPIVersion, client: &fakeClient{}}}
	return p.Capabilities()
}

func (confAdapter) NewProvider(t *testing.T, s conformance.Scenario) provider.Provider {
	t.Helper()
	switch s {
	case conformance.ScenarioTextOnly:
		idx := int32(0)
		inTok, outTok := int32(1), int32(1)
		return fakeProvider(testStream(
			&types.ConverseStreamOutputMemberContentBlockDelta{Value: types.ContentBlockDeltaEvent{
				ContentBlockIndex: &idx,
				Delta:             &types.ContentBlockDeltaMemberText{Value: "hi"},
			}},
			&types.ConverseStreamOutputMemberMessageStop{Value: types.MessageStopEvent{StopReason: types.StopReasonEndTurn}},
			&types.ConverseStreamOutputMemberMetadata{Value: types.ConverseStreamMetadataEvent{Usage: &types.TokenUsage{
				InputTokens: &inTok, OutputTokens: &outTok,
			}}},
		), nil, "req-conf-1")
	case conformance.ScenarioToolCall:
		idx := int32(0)
		inTok, outTok := int32(1), int32(1)
		return fakeProvider(testStream(
			&types.ConverseStreamOutputMemberContentBlockStart{Value: types.ContentBlockStartEvent{
				ContentBlockIndex: &idx,
				Start: &types.ContentBlockStartMemberToolUse{Value: types.ToolUseBlockStart{
					ToolUseId: stringPtr("call-1"),
					Name:      stringPtr("search"),
				}},
			}},
			&types.ConverseStreamOutputMemberContentBlockDelta{Value: types.ContentBlockDeltaEvent{
				ContentBlockIndex: &idx,
				Delta:             &types.ContentBlockDeltaMemberToolUse{Value: types.ToolUseBlockDelta{Input: stringPtr(`{"q":"go"}`)}},
			}},
			&types.ConverseStreamOutputMemberContentBlockStop{Value: types.ContentBlockStopEvent{ContentBlockIndex: &idx}},
			&types.ConverseStreamOutputMemberMessageStop{Value: types.MessageStopEvent{StopReason: types.StopReasonToolUse}},
			&types.ConverseStreamOutputMemberMetadata{Value: types.ConverseStreamMetadataEvent{Usage: &types.TokenUsage{
				InputTokens: &inTok, OutputTokens: &outTok,
			}}},
		), nil, "req-conf-2")
	case conformance.ScenarioStreamError:
		return fakeProvider(nil, errors.New("bedrock stream failed"), "")
	default:
		t.Fatalf("bedrock conformance: unhandled scenario %d", s)
		return nil
	}
}

func fakeProvider(stream *bedrockruntime.ConverseStreamEventStream, err error, reqID string) provider.Provider {
	return &bedrockProvider{cfg: config{
		providerID: "bedrock",
		apiVersion: defaultAPIVersion,
		client: &fakeClient{
			out: &converseOutput{stream: stream, requestID: reqID},
			err: err,
		},
	}}
}

func TestConformance(t *testing.T) {
	conformance.Run(t, confAdapter{})
}

func TestStreamCancellation(t *testing.T) {
	p := fakeProvider(testStream(), nil, "")
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := p.Stream(ctx, &provider.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	cancel()
	_, _ = stream.Next(ctx)
}
