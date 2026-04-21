package gemini

import (
	"encoding/json"
	"errors"
	"fmt"

	"google.golang.org/genai"

	"github.com/jerkeyray/starling/provider"
)

// buildParams converts a provider.Request into the (contents, config)
// pair the genai SDK expects. Model / Messages / SystemPrompt are
// always applied; promoted first-class fields (ToolChoice,
// StopSequences, TopK, MaxOutputTokens) are set when non-zero.
// Vendor-only knobs are not threaded through Params today — Gemini's
// knobs (temperature, topP, seed, responseMimeType, thinkingBudget)
// can be promoted in a follow-up once callers actually need them.
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

// convertToolChoice maps the portable ToolChoice string to Gemini's
// FunctionCallingConfig. A specific tool name routes to MODE_ANY with
// AllowedFunctionNames restricted to that one function. OpenAI's
// "required" is translated to "any" for cross-provider ergonomics.
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

// convertMessages translates conversational turns into Gemini's
// Content / Part shape.
//
// Gemini has only two roles on the wire — "user" and "model" — and no
// dedicated "tool" role. Tool results are delivered as user-role
// messages carrying a functionResponse part. The translator walks the
// messages twice-folded: RoleAssistant with ToolUses emits a model
// turn with text + functionCall parts; RoleTool folds into a user
// turn with a single functionResponse part. Looking up the original
// tool name from prior assistant turns is required because Gemini's
// functionResponse requires both name and id to match.
func convertMessages(in []provider.Message) ([]*genai.Content, error) {
	out := make([]*genai.Content, 0, len(in))

	// Map of CallID → Name, populated as we walk forward so a later
	// RoleTool message can resolve its tool name.
	callNames := map[string]string{}

	for _, m := range in {
		switch m.Role {
		case provider.RoleSystem:
			// Anthropic errors on this; Gemini's doc-layer contract is
			// the same (SystemPrompt is the canonical field). Surface
			// the mistake rather than silently drop.
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
				// Keep the turn addressable even if the model produced
				// no text and no tool calls (shouldn't happen, but be
				// lenient).
				parts = append(parts, &genai.Part{Text: ""})
			}
			out = append(out, &genai.Content{Role: genai.RoleModel, Parts: parts})

		case provider.RoleTool:
			if m.ToolResult == nil {
				return nil, errors.New("gemini: RoleTool message missing ToolResult")
			}
			// Name must match the prior functionCall; synthesize from
			// callNames lookup. If unresolvable, leave empty and let
			// the API return a clear 400 rather than silently succeed.
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

// decodeToolArgs converts the model's JSON args back into a
// map[string]any suitable for genai.FunctionCall.Args. Empty args map
// to {} to match the OpenAI and Anthropic adapters.
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

// convertTool maps a provider.ToolDefinition into a Gemini
// FunctionDeclaration. The JSON Schema provided by the caller is
// unmarshaled into the SDK's typed Schema; malformed schemas fail
// here rather than on the wire.
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
