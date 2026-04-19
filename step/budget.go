package step

import (
	"unicode/utf8"

	"github.com/jerkeyray/starling/provider"
)

// estimateRequestTokens returns a conservative (over-counting) estimate
// of the input tokens req will cost. It is the pre-call guardrail input
// to BudgetConfig.MaxInputTokens; the authoritative post-call count
// always comes from ChunkUsage.
//
// We deliberately avoid pulling in a real tokenizer (tiktoken and its
// encoder tables weigh ~10 MB). Instead we count runes over every user-
// visible input channel — system prompt, message contents, tool-result
// contents, tool schema JSON — and divide by 3 (rounded up). Three
// characters per token is roughly pessimistic for English and becomes
// more pessimistic for ASCII-heavy JSON; that bias is deliberate, so
// the gate errs on the side of rejecting.
//
// Fields not counted: role strings (constant overhead, bounded), tool
// definition names/descriptions (minor relative to schema bytes), and
// provider-specific Params (opaque to us).
func estimateRequestTokens(req *provider.Request) int64 {
	if req == nil {
		return 0
	}
	var runes int64
	runes += int64(utf8.RuneCountInString(req.SystemPrompt))
	for _, m := range req.Messages {
		runes += int64(utf8.RuneCountInString(m.Content))
		if m.ToolResult != nil {
			runes += int64(utf8.RuneCountInString(m.ToolResult.Content))
		}
		for _, tu := range m.ToolUses {
			runes += int64(len(tu.Args))
		}
	}
	for _, td := range req.Tools {
		runes += int64(len(td.Schema))
	}
	return (runes + 2) / 3
}
