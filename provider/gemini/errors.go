package gemini

import (
	"errors"

	"google.golang.org/genai"

	"github.com/jerkeyray/starling/provider"
)

// classifyErr wraps an SDK error with one of the provider sentinels
// (ErrRateLimit, ErrAuth, ErrServer, ErrNetwork) so consumers can
// switch on errors.Is without inspecting vendor-specific types.
func classifyErr(err error) error {
	if err == nil {
		return nil
	}
	var apiErr genai.APIError
	if errors.As(err, &apiErr) {
		return provider.WrapHTTPStatus(err, apiErr.Code)
	}
	return provider.ClassifyTransport(err)
}
