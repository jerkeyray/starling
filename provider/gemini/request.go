package gemini

import (
	"encoding/json"
	"errors"
	"fmt"

	"google.golang.org/genai"

	"github.com/jerkeyray/starling/provider"
)

// buildParams converts a provider.Request into the (contents, config)
// pair the genai SDK expects. Params (vendor-only knobs) is not
// threaded yet; promote individual knobs (temperature, topP, etc.)
// when callers need them.
func buildParams(req *provider.Request) ([]*genai.Content, *genai.GenerateContentConfig, error) {
	cfg := &genai.GenerateContentConfig{}

	if sp := req.SystemPrompt; sp != "" {
		cfg.SystemInstruction = genai.NewContentFromText(sp, genai.RoleUser)
	}

	contents, err := convertMessages(req.Messages)
	if err != nil {
		return nil, nil, err
	}

	if len(req.Tools) > 0 {
		decls := make([]*genai.FunctionDeclaration, 0, len(req.Tools))
		for _, t := range req.Tools {
			d, err := convertTool(t)
			if err != nil {
				return nil, nil, err
			}
			decls = append(decls, d)
		}
		cfg.Tools = []*genai.Tool{{FunctionDeclarations: decls}}
	}

	if tc := req.ToolChoice; tc != "" {
		cfg.ToolConfig = &genai.ToolConfig{
			FunctionCallingConfig: convertToolChoice(tc),
		}
	}

	if len(req.StopSequences) > 0 {
		cfg.StopSequences = req.StopSequences
	}
	if req.MaxOutputTokens > 0 {
		cfg.MaxOutputTokens = int32(req.MaxOutputTokens)
	}
	if req.TopK != nil {
		tk := float32(*req.TopK)
		cfg.TopK = &tk
	}

	return contents, cfg, nil
}

// convertToolChoice maps the portable ToolChoice to Gemini's
// FunctionCallingConfig. A tool name routes to MODE_ANY with
// AllowedFunctionNames set to that one function.
func convertToolChoice(tc string) *genai.FunctionCallingConfig {
	switch tc {
	case "auto":
		return &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAuto}
	case "any", "required":
		return &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAny}
	case "none":
		return &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeNone}
	default:
		return &genai.FunctionCallingConfig{
			Mode:                 genai.FunctionCallingConfigModeAny,
			AllowedFunctionNames: []string{tc},
		}
	}
}

// convertMessages translates turns into Gemini's Content/Part shape.
// Gemini has only "user" and "model" roles; tool results are
// delivered as user turns carrying a functionResponse part. We walk
// forward tracking CallID→Name so RoleTool messages can look up the
// name Gemini requires on functionResponse.
func convertMessages(in []provider.Message) ([]*genai.Content, error) {
	out := make([]*genai.Content, 0, len(in))
	callNames := map[string]string{}

	for _, m := range in {
		switch m.Role {
		case provider.RoleSystem:
			return nil, errors.New("gemini: system messages must be passed via Request.SystemPrompt")

		case provider.RoleUser:
			out = append(out, genai.NewContentFromText(m.Content, genai.RoleUser))

		case provider.RoleAssistant:
			parts := make([]*genai.Part, 0, 1+len(m.ToolUses))
			if m.Content != "" {
				parts = append(parts, &genai.Part{Text: m.Content})
			}
			for _, tu := range m.ToolUses {
				args, err := decodeToolArgs(tu.Args)
				if err != nil {
					return nil, fmt.Errorf("gemini: tool-use %q args: %w", tu.Name, err)
				}
				parts = append(parts, &genai.Part{
					FunctionCall: &genai.FunctionCall{
						ID:   tu.CallID,
						Name: tu.Name,
						Args: args,
					},
				})
				callNames[tu.CallID] = tu.Name
			}
			if len(parts) == 0 {
				// Keep the turn addressable even if empty.
				parts = append(parts, &genai.Part{Text: ""})
			}
			out = append(out, &genai.Content{Role: genai.RoleModel, Parts: parts})

		case provider.RoleTool:
			if m.ToolResult == nil {
				return nil, errors.New("gemini: RoleTool message missing ToolResult")
			}
			name := callNames[m.ToolResult.CallID]
			resp := map[string]any{"result": m.ToolResult.Content}
			if m.ToolResult.IsError {
				resp = map[string]any{"error": m.ToolResult.Content}
			}
			out = append(out, &genai.Content{
				Role: genai.RoleUser,
				Parts: []*genai.Part{{
					FunctionResponse: &genai.FunctionResponse{
						ID:       m.ToolResult.CallID,
						Name:     name,
						Response: resp,
					},
				}},
			})

		default:
			return nil, fmt.Errorf("gemini: unknown role %q", string(m.Role))
		}
	}
	return out, nil
}

// decodeToolArgs converts JSON args to map[string]any for
// genai.FunctionCall.Args. Empty args → {}.
func decodeToolArgs(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	if v == nil {
		return map[string]any{}, nil
	}
	return v, nil
}

// convertTool maps a ToolDefinition into a Gemini
// FunctionDeclaration. Malformed schemas fail here, not on the wire.
func convertTool(t provider.ToolDefinition) (*genai.FunctionDeclaration, error) {
	decl := &genai.FunctionDeclaration{
		Name:        t.Name,
		Description: t.Description,
	}
	if len(t.Schema) > 0 {
		var schema genai.Schema
		if err := json.Unmarshal(t.Schema, &schema); err != nil {
			return nil, fmt.Errorf("gemini: tool %q schema: %w", t.Name, err)
		}
		decl.Parameters = &schema
	}
	return decl, nil
}
