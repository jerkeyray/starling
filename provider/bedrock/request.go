package bedrock

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/jerkeyray/starling/internal/cborenc"
	"github.com/jerkeyray/starling/provider"
)

func buildInput(req *provider.Request) (*bedrockruntime.ConverseStreamInput, error) {
	input := &bedrockruntime.ConverseStreamInput{
		ModelId: &req.Model,
	}

	if req.SystemPrompt != "" {
		input.System = []types.SystemContentBlock{
			&types.SystemContentBlockMemberText{Value: req.SystemPrompt},
		}
	}

	msgs, err := convertMessages(req.Messages)
	if err != nil {
		return nil, err
	}
	input.Messages = msgs

	inf := &types.InferenceConfiguration{}
	if req.MaxOutputTokens > 0 {
		v := int32(req.MaxOutputTokens)
		inf.MaxTokens = &v
	}
	if len(req.StopSequences) > 0 {
		inf.StopSequences = append([]string(nil), req.StopSequences...)
	}
	input.InferenceConfig = inf

	if len(req.Tools) > 0 {
		tools := make([]types.Tool, 0, len(req.Tools))
		for _, t := range req.Tools {
			tool, err := convertTool(t)
			if err != nil {
				return nil, err
			}
			tools = append(tools, tool)
		}
		input.ToolConfig = &types.ToolConfiguration{Tools: tools}
		if req.ToolChoice != "" {
			tc, err := convertToolChoice(req.ToolChoice)
			if err != nil {
				return nil, err
			}
			input.ToolConfig.ToolChoice = tc
		}
	} else if req.ToolChoice != "" && req.ToolChoice != "none" {
		return nil, errors.New("bedrock: ToolChoice requires at least one tool")
	}

	if err := applyParams(input, req.Params); err != nil {
		return nil, err
	}
	if req.TopK != nil {
		input.AdditionalModelRequestFields = mergeDocument(input.AdditionalModelRequestFields, map[string]any{"top_k": *req.TopK})
	}

	if input.InferenceConfig != nil && input.InferenceConfig.MaxTokens == nil && len(input.InferenceConfig.StopSequences) == 0 &&
		input.InferenceConfig.Temperature == nil && input.InferenceConfig.TopP == nil {
		input.InferenceConfig = nil
	}

	return input, nil
}

func convertMessages(in []provider.Message) ([]types.Message, error) {
	out := make([]types.Message, 0, len(in))
	callNames := map[string]string{}

	for _, m := range in {
		switch m.Role {
		case provider.RoleSystem:
			return nil, errors.New("bedrock: system messages must be passed via Request.SystemPrompt")
		case provider.RoleUser:
			out = append(out, types.Message{
				Role:    types.ConversationRoleUser,
				Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: m.Content}},
			})
		case provider.RoleAssistant:
			blocks := make([]types.ContentBlock, 0, 1+len(m.ToolUses))
			if m.Content != "" {
				blocks = append(blocks, &types.ContentBlockMemberText{Value: m.Content})
			}
			for _, tu := range m.ToolUses {
				input, err := decodeToolInput(tu.Args)
				if err != nil {
					return nil, fmt.Errorf("bedrock: tool-use %q args: %w", tu.Name, err)
				}
				blocks = append(blocks, &types.ContentBlockMemberToolUse{Value: types.ToolUseBlock{
					ToolUseId: &tu.CallID,
					Name:      &tu.Name,
					Input:     document.NewLazyDocument(input),
				}})
				callNames[tu.CallID] = tu.Name
			}
			if len(blocks) == 0 {
				blocks = append(blocks, &types.ContentBlockMemberText{Value: ""})
			}
			out = append(out, types.Message{Role: types.ConversationRoleAssistant, Content: blocks})
		case provider.RoleTool:
			if m.ToolResult == nil {
				return nil, errors.New("bedrock: RoleTool message missing ToolResult")
			}
			status := types.ToolResultStatusSuccess
			if m.ToolResult.IsError {
				status = types.ToolResultStatusError
			}
			_ = callNames[m.ToolResult.CallID]
			out = append(out, types.Message{
				Role: types.ConversationRoleUser,
				Content: []types.ContentBlock{&types.ContentBlockMemberToolResult{Value: types.ToolResultBlock{
					ToolUseId: &m.ToolResult.CallID,
					Status:    status,
					Content: []types.ToolResultContentBlock{
						&types.ToolResultContentBlockMemberText{Value: m.ToolResult.Content},
					},
				}}},
			})
		default:
			return nil, fmt.Errorf("bedrock: unknown role %q", string(m.Role))
		}
	}
	return out, nil
}

func decodeToolInput(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	if v == nil {
		return map[string]any{}, nil
	}
	return v, nil
}

func convertTool(t provider.ToolDefinition) (types.Tool, error) {
	spec := types.ToolSpecification{
		Name:        &t.Name,
		Description: stringPtrOrNil(t.Description),
	}
	if len(t.Schema) > 0 {
		var schema any
		if err := json.Unmarshal(t.Schema, &schema); err != nil {
			return nil, fmt.Errorf("bedrock: tool %q schema: %w", t.Name, err)
		}
		spec.InputSchema = &types.ToolInputSchemaMemberJson{Value: document.NewLazyDocument(schema)}
	} else {
		spec.InputSchema = &types.ToolInputSchemaMemberJson{Value: document.NewLazyDocument(map[string]any{"type": "object"})}
	}
	return &types.ToolMemberToolSpec{Value: spec}, nil
}

func convertToolChoice(tc string) (types.ToolChoice, error) {
	switch tc {
	case "auto":
		return &types.ToolChoiceMemberAuto{Value: types.AutoToolChoice{}}, nil
	case "any", "required":
		return &types.ToolChoiceMemberAny{Value: types.AnyToolChoice{}}, nil
	case "none":
		return nil, errors.New(`bedrock: ToolChoice "none" is not supported by Converse`)
	default:
		return &types.ToolChoiceMemberTool{Value: types.SpecificToolChoice{Name: &tc}}, nil
	}
}

func applyParams(input *bedrockruntime.ConverseStreamInput, raw cborenc.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if err := cborenc.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("bedrock: decode Request.Params: %w", err)
	}

	reserved := map[string]struct{}{
		"modelId":          {},
		"model_id":         {},
		"messages":         {},
		"system":           {},
		"toolConfig":       {},
		"tool_config":      {},
		"inferenceConfig":  {},
		"inference_config": {},
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		if _, skip := reserved[k]; skip {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		v := m[k]
		switch k {
		case "temperature":
			f, ok := toFloat32(v)
			if !ok {
				return fmt.Errorf("bedrock: Params.temperature must be numeric")
			}
			ensureInference(input).Temperature = &f
		case "topP", "top_p":
			f, ok := toFloat32(v)
			if !ok {
				return fmt.Errorf("bedrock: Params.%s must be numeric", k)
			}
			ensureInference(input).TopP = &f
		case "additionalModelRequestFields", "additional_model_request_fields":
			mm, ok := toStringAnyMap(v)
			if !ok {
				return fmt.Errorf("bedrock: Params.%s must be an object", k)
			}
			input.AdditionalModelRequestFields = mergeDocument(input.AdditionalModelRequestFields, mm)
		case "additionalModelResponseFieldPaths", "additional_model_response_field_paths":
			paths, err := stringSlice(v)
			if err != nil {
				return fmt.Errorf("bedrock: Params.%s: %w", k, err)
			}
			input.AdditionalModelResponseFieldPaths = paths
		case "requestMetadata", "request_metadata":
			meta, err := stringMap(v)
			if err != nil {
				return fmt.Errorf("bedrock: Params.%s: %w", k, err)
			}
			input.RequestMetadata = meta
		case "performanceConfig", "performance_config":
			pc, err := performanceConfig(v)
			if err != nil {
				return fmt.Errorf("bedrock: Params.%s: %w", k, err)
			}
			input.PerformanceConfig = pc
		case "serviceTier", "service_tier":
			st, err := serviceTier(v)
			if err != nil {
				return fmt.Errorf("bedrock: Params.%s: %w", k, err)
			}
			input.ServiceTier = st
		case "promptVariables", "prompt_variables":
			pv, err := promptVariables(v)
			if err != nil {
				return fmt.Errorf("bedrock: Params.%s: %w", k, err)
			}
			input.PromptVariables = pv
		case "outputConfig", "output_config":
			oc, err := outputConfig(v)
			if err != nil {
				return fmt.Errorf("bedrock: Params.%s: %w", k, err)
			}
			input.OutputConfig = oc
		case "guardrailConfig", "guardrail_config":
			gc, err := guardrailConfig(v)
			if err != nil {
				return fmt.Errorf("bedrock: Params.%s: %w", k, err)
			}
			input.GuardrailConfig = gc
		default:
			return fmt.Errorf("bedrock: unsupported Request.Params key %q", k)
		}
	}
	return nil
}

func ensureInference(input *bedrockruntime.ConverseStreamInput) *types.InferenceConfiguration {
	if input.InferenceConfig == nil {
		input.InferenceConfig = &types.InferenceConfiguration{}
	}
	return input.InferenceConfig
}

func mergeDocument(existing document.Interface, add map[string]any) document.Interface {
	merged := map[string]any{}
	if existing != nil {
		var prior map[string]any
		if b, err := existing.MarshalSmithyDocument(); err == nil && json.Unmarshal(b, &prior) == nil {
			for k, v := range prior {
				merged[k] = v
			}
		}
	}
	for k, v := range add {
		merged[k] = v
	}
	return document.NewLazyDocument(merged)
}

func toStringAnyMap(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, true
	case map[any]any:
		out := make(map[string]any, len(m))
		for k, v := range m {
			ks, ok := k.(string)
			if !ok {
				return nil, false
			}
			out[ks] = v
		}
		return out, true
	default:
		return nil, false
	}
}

func stringPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func toFloat32(v any) (float32, bool) {
	switch n := v.(type) {
	case float32:
		return n, true
	case float64:
		return float32(n), true
	case int:
		return float32(n), true
	case int64:
		return float32(n), true
	case uint64:
		return float32(n), true
	default:
		return 0, false
	}
}

func stringSlice(v any) ([]string, error) {
	switch vv := v.(type) {
	case []string:
		return append([]string(nil), vv...), nil
	case []any:
		out := make([]string, 0, len(vv))
		for _, item := range vv {
			s, ok := item.(string)
			if !ok {
				return nil, errors.New("all entries must be strings")
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, errors.New("must be an array")
	}
}

func stringMap(v any) (map[string]string, error) {
	switch vv := v.(type) {
	case map[string]string:
		out := make(map[string]string, len(vv))
		for k, v := range vv {
			out[k] = v
		}
		return out, nil
	case map[string]any:
		out := make(map[string]string, len(vv))
		for k, item := range vv {
			s, ok := item.(string)
			if !ok {
				return nil, errors.New("all values must be strings")
			}
			out[k] = s
		}
		return out, nil
	case map[any]any:
		out := make(map[string]string, len(vv))
		for k, item := range vv {
			ks, ok := k.(string)
			if !ok {
				return nil, errors.New("all keys must be strings")
			}
			s, ok := item.(string)
			if !ok {
				return nil, errors.New("all values must be strings")
			}
			out[ks] = s
		}
		return out, nil
	default:
		return nil, errors.New("must be an object")
	}
}

func performanceConfig(v any) (*types.PerformanceConfiguration, error) {
	m, ok := toStringAnyMap(v)
	if !ok {
		return nil, errors.New("must be an object")
	}
	latency, ok := stringValue(m, "latency")
	if !ok || latency == "" {
		return nil, errors.New(`missing string field "latency"`)
	}
	return &types.PerformanceConfiguration{Latency: types.PerformanceConfigLatency(latency)}, nil
}

func serviceTier(v any) (*types.ServiceTier, error) {
	switch vv := v.(type) {
	case string:
		return &types.ServiceTier{Type: types.ServiceTierType(vv)}, nil
	default:
		m, ok := toStringAnyMap(v)
		if !ok {
			return nil, errors.New("must be a string or object")
		}
		t, ok := stringValue(m, "type")
		if !ok || t == "" {
			return nil, errors.New(`missing string field "type"`)
		}
		return &types.ServiceTier{Type: types.ServiceTierType(t)}, nil
	}
}

func promptVariables(v any) (map[string]types.PromptVariableValues, error) {
	m, ok := toStringAnyMap(v)
	if !ok {
		return nil, errors.New("must be an object")
	}
	out := make(map[string]types.PromptVariableValues, len(m))
	for k, raw := range m {
		switch vv := raw.(type) {
		case string:
			out[k] = &types.PromptVariableValuesMemberText{Value: vv}
		default:
			vm, ok := toStringAnyMap(raw)
			if !ok {
				return nil, fmt.Errorf("%s must be a string or object", k)
			}
			text, ok := stringValue(vm, "text")
			if !ok {
				text, ok = stringValue(vm, "value")
			}
			if !ok {
				return nil, fmt.Errorf(`%s missing string field "text"`, k)
			}
			out[k] = &types.PromptVariableValuesMemberText{Value: text}
		}
	}
	return out, nil
}

func outputConfig(v any) (*types.OutputConfig, error) {
	m, ok := toStringAnyMap(v)
	if !ok {
		return nil, errors.New("must be an object")
	}
	textFormatRaw, ok := firstValue(m, "textFormat", "text_format")
	if !ok {
		return nil, errors.New(`missing object field "textFormat"`)
	}
	tf, ok := toStringAnyMap(textFormatRaw)
	if !ok {
		return nil, errors.New(`field "textFormat" must be an object`)
	}
	typeValue, ok := stringValue(tf, "type")
	if !ok || typeValue == "" {
		typeValue = string(types.OutputFormatTypeJsonSchema)
	}
	structureRaw, ok := firstValue(tf, "structure")
	if !ok {
		return nil, errors.New(`missing object field "textFormat.structure"`)
	}
	structure, ok := toStringAnyMap(structureRaw)
	if !ok {
		return nil, errors.New(`field "textFormat.structure" must be an object`)
	}
	jsonSchemaRaw, ok := firstValue(structure, "jsonSchema", "json_schema")
	if !ok {
		return nil, errors.New(`missing object field "textFormat.structure.jsonSchema"`)
	}
	jsonSchema, ok := toStringAnyMap(jsonSchemaRaw)
	if !ok {
		return nil, errors.New(`field "textFormat.structure.jsonSchema" must be an object`)
	}
	schemaRaw, ok := firstValue(jsonSchema, "schema")
	if !ok {
		return nil, errors.New(`missing field "textFormat.structure.jsonSchema.schema"`)
	}
	schemaBytes, err := json.Marshal(jsonCompatible(schemaRaw))
	if err != nil {
		return nil, fmt.Errorf("marshal json schema: %w", err)
	}
	schema := string(schemaBytes)
	def := types.JsonSchemaDefinition{Schema: &schema}
	if name, ok := stringValue(jsonSchema, "name"); ok {
		def.Name = &name
	}
	if desc, ok := stringValue(jsonSchema, "description"); ok {
		def.Description = &desc
	}
	return &types.OutputConfig{TextFormat: &types.OutputFormat{
		Type: types.OutputFormatType(typeValue),
		Structure: &types.OutputFormatStructureMemberJsonSchema{
			Value: def,
		},
	}}, nil
}

func guardrailConfig(v any) (*types.GuardrailStreamConfiguration, error) {
	m, ok := toStringAnyMap(v)
	if !ok {
		return nil, errors.New("must be an object")
	}
	cfg := &types.GuardrailStreamConfiguration{}
	if id, ok := stringValue(m, "guardrailIdentifier", "guardrail_identifier", "identifier"); ok {
		cfg.GuardrailIdentifier = &id
	}
	if version, ok := stringValue(m, "guardrailVersion", "guardrail_version", "version"); ok {
		cfg.GuardrailVersion = &version
	}
	if mode, ok := stringValue(m, "streamProcessingMode", "stream_processing_mode"); ok {
		cfg.StreamProcessingMode = types.GuardrailStreamProcessingMode(mode)
	}
	if trace, ok := stringValue(m, "trace"); ok {
		cfg.Trace = types.GuardrailTrace(trace)
	}
	return cfg, nil
}

func stringValue(m map[string]any, keys ...string) (string, bool) {
	v, ok := firstValue(m, keys...)
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func firstValue(m map[string]any, keys ...string) (any, bool) {
	for _, k := range keys {
		v, ok := m[k]
		if ok {
			return v, true
		}
	}
	return nil, false
}

func jsonCompatible(v any) any {
	switch vv := v.(type) {
	case map[any]any:
		out := make(map[string]any, len(vv))
		for k, val := range vv {
			ks, ok := k.(string)
			if !ok {
				ks = fmt.Sprint(k)
			}
			out[ks] = jsonCompatible(val)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(vv))
		for k, val := range vv {
			out[k] = jsonCompatible(val)
		}
		return out
	case []any:
		out := make([]any, len(vv))
		for i, val := range vv {
			out[i] = jsonCompatible(val)
		}
		return out
	default:
		return v
	}
}
