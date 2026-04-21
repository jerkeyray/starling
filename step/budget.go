package step

import (
	"unicode/utf8"

	"github.com/jerkeyray/starling/provider"
)

// estimateRequestTokens returns a conservative pre-call token
// estimate for MaxInputTokens gating. Counts runes over prompts /
// messages / tool contents / schemas and divides by 3 (pessimistic
// for English, more so for ASCII JSON). Authoritative counts come
// post-call from ChunkUsage.
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
