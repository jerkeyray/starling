// Package openai implements the Provider interface against the OpenAI
// Chat Completions API. The same adapter serves every OpenAI-compatible
// endpoint (Groq, Together, OpenRouter, Ollama, vLLM, LM Studio, Azure
// OpenAI, ...) via WithBaseURL.
package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"

	oai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/shared"

	"github.com/jerkeyray/starling/internal/cborenc"
	"github.com/jerkeyray/starling/provider"
)

// New constructs a Provider that talks to OpenAI (or any OpenAI-compatible
// endpoint when WithBaseURL is set).
func New(opts ...Option) (provider.Provider, error) {
	cfg := config{
		providerID: "openai",
		apiVersion: "v1",
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	var clientOpts []option.RequestOption
	if cfg.apiKey != "" {
		clientOpts = append(clientOpts, option.WithAPIKey(cfg.apiKey))
	}
	if cfg.baseURL != "" {
		clientOpts = append(clientOpts, option.WithBaseURL(cfg.baseURL))
	}
	if cfg.httpClient != nil {
		clientOpts = append(clientOpts, option.WithHTTPClient(cfg.httpClient))
	}
	if cfg.organization != "" {
		clientOpts = append(clientOpts, option.WithOrganization(cfg.organization))
	}

	client := oai.NewClient(clientOpts...)
	return &openaiProvider{client: &client, cfg: cfg}, nil
}

// Option configures the OpenAI provider.
type Option func(*config)

type config struct {
	apiKey       string
	baseURL      string
	httpClient   *http.Client
	organization string
	providerID   string
	apiVersion   string
}

// WithAPIKey sets the API key used for authentication.
func WithAPIKey(key string) Option { return func(c *config) { c.apiKey = key } }

// WithBaseURL overrides the API base URL. Set this to point at an
// OpenAI-compatible endpoint (Groq, Together, OpenRouter, Ollama, ...).
func WithBaseURL(url string) Option { return func(c *config) { c.baseURL = url } }

// WithHTTPClient supplies a custom *http.Client (useful for timeouts,
// proxies, or custom transports).
func WithHTTPClient(c *http.Client) Option {
	return func(cfg *config) { cfg.httpClient = c }
}

// WithOrganization sets the OpenAI-Organization header. Ignored by
// compatibility backends that don't implement it.
func WithOrganization(org string) Option { return func(c *config) { c.organization = org } }

// WithProviderID overrides the Info().ID string. Useful when a compat
// backend (e.g. Groq, Ollama) wants to identify itself distinctly in the
// event log. Defaults to "openai".
func WithProviderID(id string) Option { return func(c *config) { c.providerID = id } }

// WithAPIVersion overrides the Info().APIVersion string. Defaults to "v1".
func WithAPIVersion(v string) Option { return func(c *config) { c.apiVersion = v } }

type openaiProvider struct {
	client *oai.Client
	cfg    config
}

func (p *openaiProvider) Info() provider.Info {
	return provider.Info{ID: p.cfg.providerID, APIVersion: p.cfg.apiVersion}
}

func (p *openaiProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{
		Tools:         true,
		ToolChoice:    true,
		Reasoning:     true,
		StopSequences: true,
		RequestID:     true,
	}
}

func (p *openaiProvider) Stream(ctx context.Context, req *provider.Request) (provider.EventStream, error) {
	if req == nil {
		return nil, errors.New("openai: nil Request")
	}

	params, err := buildParams(req)
	if err != nil {
		return nil, err
	}

	extraOpts, err := paramsFromCBOR(req.Params)
	if err != nil {
		return nil, fmt.Errorf("openai: decode Request.Params: %w", err)
	}

	httpResp := new(*http.Response)
	reqOpts := append(extraOpts, option.WithResponseInto(httpResp))

	sdk := p.client.Chat.Completions.NewStreaming(ctx, params, reqOpts...)
	return newOpenAIStream(sdk, httpResp), nil
}

// buildParams converts a provider.Request into an oai.ChatCompletionNewParams.
// Model/Messages/Tools/StreamOptions are always set by this function;
// caller-supplied Params layer on top as option.WithJSONSet.
func buildParams(req *provider.Request) (oai.ChatCompletionNewParams, error) {
	params := oai.ChatCompletionNewParams{
		Model: shared.ChatModel(req.Model),
		StreamOptions: oai.ChatCompletionStreamOptionsParam{
			IncludeUsage: oai.Bool(true),
		},
	}

	// SystemPrompt prepends as the first system message; any caller-
	// provided RoleSystem messages still flow through unchanged.
	if req.SystemPrompt != "" {
		params.Messages = append(params.Messages, oai.SystemMessage(req.SystemPrompt))
	}
	for _, m := range req.Messages {
		msg, err := convertMessage(m)
		if err != nil {
			return params, err
		}
		params.Messages = append(params.Messages, msg)
	}

	if len(req.Tools) > 0 {
		params.Tools = make([]oai.ChatCompletionToolUnionParam, 0, len(req.Tools))
		for _, t := range req.Tools {
			tool, err := convertTool(t)
			if err != nil {
				return params, err
			}
			params.Tools = append(params.Tools, tool)
		}
	}

	// Promoted first-class fields. TopK has no OpenAI equivalent and is
	// intentionally ignored; MaxOutputTokens uses max_completion_tokens
	// since it's the forward-compatible field for o-series / GPT-5 and
	// also accepted by chat models. Callers who need legacy max_tokens
	// can still route through Params.
	if req.MaxOutputTokens > 0 {
		params.MaxCompletionTokens = param.NewOpt(int64(req.MaxOutputTokens))
	}
	if len(req.StopSequences) > 0 {
		if len(req.StopSequences) == 1 {
			params.Stop = oai.ChatCompletionNewParamsStopUnion{
				OfString: param.NewOpt(req.StopSequences[0]),
			}
		} else {
			params.Stop = oai.ChatCompletionNewParamsStopUnion{
				OfStringArray: req.StopSequences,
			}
		}
	}
	if tc := req.ToolChoice; tc != "" {
		switch tc {
		case "auto", "none":
			params.ToolChoice = oai.ChatCompletionToolChoiceOptionUnionParam{
				OfAuto: param.NewOpt(tc),
			}
		case "any":
			// Anthropic uses "any" to mean "must call a tool"; OpenAI's
			// equivalent is "required". Translate for portability.
			params.ToolChoice = oai.ChatCompletionToolChoiceOptionUnionParam{
				OfAuto: param.NewOpt("required"),
			}
		case "required":
			params.ToolChoice = oai.ChatCompletionToolChoiceOptionUnionParam{
				OfAuto: param.NewOpt("required"),
			}
		default:
			// Specific tool name: pin a function choice.
			params.ToolChoice = oai.ChatCompletionToolChoiceOptionUnionParam{
				OfFunctionToolChoice: &oai.ChatCompletionNamedToolChoiceParam{
					Function: oai.ChatCompletionNamedToolChoiceFunctionParam{Name: tc},
				},
			}
		}
	}

	return params, nil
}

func convertMessage(m provider.Message) (oai.ChatCompletionMessageParamUnion, error) {
	switch m.Role {
	case provider.RoleSystem:
		return oai.SystemMessage(m.Content), nil

	case provider.RoleUser:
		return oai.UserMessage(m.Content), nil

	case provider.RoleAssistant:
		if len(m.ToolUses) == 0 {
			return oai.AssistantMessage(m.Content), nil
		}
		assistant := oai.ChatCompletionAssistantMessageParam{}
		if m.Content != "" {
			assistant.Content.OfString = param.NewOpt(m.Content)
		}
		assistant.ToolCalls = make([]oai.ChatCompletionMessageToolCallUnionParam, 0, len(m.ToolUses))
		for _, tu := range m.ToolUses {
			args := string(tu.Args)
			if args == "" {
				args = "{}"
			}
			assistant.ToolCalls = append(assistant.ToolCalls, oai.ChatCompletionMessageToolCallUnionParam{
				OfFunction: &oai.ChatCompletionMessageFunctionToolCallParam{
					ID: tu.CallID,
					Function: oai.ChatCompletionMessageFunctionToolCallFunctionParam{
						Name:      tu.Name,
						Arguments: args,
					},
				},
			})
		}
		return oai.ChatCompletionMessageParamUnion{OfAssistant: &assistant}, nil

	case provider.RoleTool:
		if m.ToolResult == nil {
			return oai.ChatCompletionMessageParamUnion{}, errors.New("openai: RoleTool message missing ToolResult")
		}
		content := m.ToolResult.Content
		// OpenAI tool messages have no first-class error marker; prefix
		// error results so the model sees a signal. The prefix is lossy
		// (a tool that legitimately returns text starting with "error: "
		// is indistinguishable from a failure on round-trip); acceptable
		// for M1 because tool failures rarely cycle back through the
		// provider. Refined in M2 via a structured error channel.
		if m.ToolResult.IsError {
			content = "error: " + content
		}
		return oai.ToolMessage(content, m.ToolResult.CallID), nil
	}

	return oai.ChatCompletionMessageParamUnion{}, fmt.Errorf("openai: unknown role %q", string(m.Role))
}

func convertTool(t provider.ToolDefinition) (oai.ChatCompletionToolUnionParam, error) {
	fn := shared.FunctionDefinitionParam{Name: t.Name}
	if t.Description != "" {
		fn.Description = param.NewOpt(t.Description)
	}
	if len(t.Schema) > 0 {
		var params oai.FunctionParameters
		if err := json.Unmarshal(t.Schema, &params); err != nil {
			return oai.ChatCompletionToolUnionParam{}, fmt.Errorf("openai: tool %q schema: %w", t.Name, err)
		}
		fn.Parameters = params
	}
	return oai.ChatCompletionToolUnionParam{
		OfFunction: &oai.ChatCompletionFunctionToolParam{Function: fn},
	}, nil
}

// paramsFromCBOR decodes req.Params (CBOR) into []option.RequestOption by
// applying each top-level key via option.WithJSONSet. Callers must not use
// Params to override model/messages/tools/stream/stream_options — those are
// set by buildParams. Attempts to override are silently dropped.
func paramsFromCBOR(raw cborenc.RawMessage) ([]option.RequestOption, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var m map[string]any
	if err := cborenc.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	reserved := map[string]struct{}{
		"model":                 {},
		"messages":              {},
		"tools":                 {},
		"stream":                {},
		"stream_options":        {},
		"tool_choice":           {},
		"stop":                  {},
		"max_completion_tokens": {},
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
