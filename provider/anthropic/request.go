package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	anth "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"

	"github.com/jerkeyray/starling/internal/cborenc"
	"github.com/jerkeyray/starling/provider"
)

// buildParams converts a provider.Request into an anth.MessageNewParams.
// Model / Messages / MaxTokens are always set; promoted first-class
// fields (ToolChoice, StopSequences, TopK) are applied when non-empty.
// Vendor-only knobs ride along via Request.Params.
func buildParams(req *provider.Request) (anth.MessageNewParams, error) {
	maxTokens := int64(req.MaxOutputTokens)
	if maxTokens <= 0 {
		maxTokens = DefaultMaxOutputTokens
	}
	params := anth.MessageNewParams{
		Model:     anth.Model(req.Model),
		MaxTokens: maxTokens,
	}

	if req.SystemPrompt != "" {
		params.System = []anth.TextBlockParam{{Text: req.SystemPrompt}}
	}

	msgs, err := convertMessages(req.Messages)
	if err != nil {
		return params, err
	}
	params.Messages = msgs

	if len(req.Tools) > 0 {
		tools := make([]anth.ToolUnionParam, 0, len(req.Tools))
		for _, t := range req.Tools {
			tool, err := convertTool(t)
			if err != nil {
				return params, err
			}
			tools = append(tools, tool)
		}
		params.Tools = tools
	}

	if len(req.StopSequences) > 0 {
		params.StopSequences = req.StopSequences
	}

	if req.TopK != nil {
		params.TopK = param.NewOpt(int64(*req.TopK))
	}

	if tc := req.ToolChoice; tc != "" {
		params.ToolChoice = convertToolChoice(tc)
	}

	return params, nil
}

// convertToolChoice maps the portable ToolChoice string to Anthropic's
// discriminated union. OpenAI's "required" is translated to Anthropic's
// "any" for cross-provider ergonomics; specific tool names route to
// ToolChoiceToolParam.
func convertToolChoice(tc string) anth.ToolChoiceUnionParam {
	switch tc {
	case "auto":
		return anth.ToolChoiceUnionParam{OfAuto: &anth.ToolChoiceAutoParam{}}
	case "any", "required":
		return anth.ToolChoiceUnionParam{OfAny: &anth.ToolChoiceAnyParam{}}
	case "none":
		return anth.ToolChoiceUnionParam{OfNone: &anth.ToolChoiceNoneParam{}}
	default:
		// Specific tool name.
		return anth.ToolChoiceParamOfTool(tc)
	}
}

// convertMessages translates conversational turns into Anthropic's
// content-block shape. System messages get pulled into params.System
// upstream; here we only handle user / assistant / tool roles.
func convertMessages(in []provider.Message) ([]anth.MessageParam, error) {
	out := make([]anth.MessageParam, 0, len(in))
	for _, m := range in {
		mp, err := convertMessage(m)
		if err != nil {
			return nil, err
		}
		out = append(out, mp)
	}
	return out, nil
}

func convertMessage(m provider.Message) (anth.MessageParam, error) {
	cc := cacheControlFromAnnotations(m.Annotations)
	switch m.Role {
	case provider.RoleSystem:
		// Anthropic has no "system" role in messages; callers should
		// use Request.SystemPrompt. Surface the mistake rather than
		// silently dropping the content.
		return anth.MessageParam{}, errors.New("anthropic: system messages must be passed via Request.SystemPrompt")

	case provider.RoleUser:
		block := anth.NewTextBlock(m.Content)
		applyCacheControl(&block, cc)
		return anth.NewUserMessage(block), nil

	case provider.RoleAssistant:
		var blocks []anth.ContentBlockParamUnion
		if m.Content != "" {
			tb := anth.NewTextBlock(m.Content)
			blocks = append(blocks, tb)
		}
		for _, tu := range m.ToolUses {
			input, err := decodeToolInput(tu.Args)
			if err != nil {
				return anth.MessageParam{}, fmt.Errorf("anthropic: tool-use %q args: %w", tu.Name, err)
			}
			blocks = append(blocks, anth.NewToolUseBlock(tu.CallID, input, tu.Name))
		}
		if len(blocks) == 0 {
			// Anthropic rejects empty-content assistant messages; a
			// no-op text block keeps the turn addressable.
			blocks = append(blocks, anth.NewTextBlock(""))
		}
		// Cache-control on an assistant turn attaches to the final block.
		if cc != nil && len(blocks) > 0 {
			applyCacheControl(&blocks[len(blocks)-1], cc)
		}
		return anth.NewAssistantMessage(blocks...), nil

	case provider.RoleTool:
		if m.ToolResult == nil {
			return anth.MessageParam{}, errors.New("anthropic: RoleTool message missing ToolResult")
		}
		block := anth.NewToolResultBlock(m.ToolResult.CallID, m.ToolResult.Content, m.ToolResult.IsError)
		applyCacheControl(&block, cc)
		// Tool results are delivered on a user-role turn per Anthropic's
		// Messages API contract.
		return anth.NewUserMessage(block), nil
	}

	return anth.MessageParam{}, fmt.Errorf("anthropic: unknown role %q", string(m.Role))
}

// decodeToolInput converts the model's JSON args back into a Go value
// suitable for ToolUseBlockParam.Input. Empty args map to {}, matching
// the OpenAI adapter's behavior.
func decodeToolInput(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return v, nil
}

func convertTool(t provider.ToolDefinition) (anth.ToolUnionParam, error) {
	tool := anth.ToolParam{Name: t.Name}
	if t.Description != "" {
		tool.Description = param.NewOpt(t.Description)
	}
	if len(t.Schema) > 0 {
		var schema anth.ToolInputSchemaParam
		if err := json.Unmarshal(t.Schema, &schema); err != nil {
			return anth.ToolUnionParam{}, fmt.Errorf("anthropic: tool %q schema: %w", t.Name, err)
		}
		tool.InputSchema = schema
	}
	return anth.ToolUnionParam{OfTool: &tool}, nil
}

// cacheControlFromAnnotations reads Message.Annotations["cache_control"]
// and converts it to the SDK's CacheControlEphemeralParam. Expected
// shape: map[string]any{"type":"ephemeral","ttl":"5m"|"1h"}. Missing or
// malformed entries return nil (the adapter emits no cache-control
// block). Unknown keys are ignored — this is an intentionally lenient
// escape hatch.
func cacheControlFromAnnotations(ann map[string]any) *anth.CacheControlEphemeralParam {
	if ann == nil {
		return nil
	}
	raw, ok := ann["cache_control"]
	if !ok {
		return nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	if t, _ := m["type"].(string); t != "ephemeral" && t != "" {
		// Only "ephemeral" is documented; ignore other shapes rather
		// than forward a value the SDK will reject.
		return nil
	}
	cc := anth.NewCacheControlEphemeralParam()
	if ttl, ok := m["ttl"].(string); ok && ttl != "" {
		cc.TTL = anth.CacheControlEphemeralTTL(ttl)
	}
	return &cc
}

// applyCacheControl sets the cache_control breakpoint on whichever block
// variant b carries. We branch by block type because ContentBlockParamUnion
// doesn't expose a generic setter.
func applyCacheControl(b *anth.ContentBlockParamUnion, cc *anth.CacheControlEphemeralParam) {
	if cc == nil || b == nil {
		return
	}
	switch {
	case b.OfText != nil:
		b.OfText.CacheControl = *cc
	case b.OfToolUse != nil:
		b.OfToolUse.CacheControl = *cc
	case b.OfToolResult != nil:
		b.OfToolResult.CacheControl = *cc
	}
}

// paramsFromCBOR decodes req.Params (CBOR) into []option.RequestOption by
// applying each top-level key via option.WithJSONSet. Callers must not
// use Params to override fields already set by buildParams (model,
// messages, tools, stream, max_tokens, stop_sequences, top_k,
// tool_choice, system). Attempts to override are silently dropped.
func paramsFromCBOR(raw cborenc.RawMessage) ([]option.RequestOption, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var m map[string]any
	if err := cborenc.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	reserved := map[string]struct{}{
		"model":          {},
		"messages":       {},
		"tools":          {},
		"tool_choice":    {},
		"stream":         {},
		"max_tokens":     {},
		"stop_sequences": {},
		"top_k":          {},
		"system":         {},
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		if _, skip := reserved[k]; skip {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	opts := make([]option.RequestOption, 0, len(keys))
	for _, k := range keys {
		opts = append(opts, option.WithJSONSet(k, m[k]))
	}
	return opts, nil
}
