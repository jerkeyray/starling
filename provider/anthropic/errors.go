package anthropic

import (
	"errors"

	anth "github.com/anthropics/anthropic-sdk-go"

	"github.com/jerkeyray/starling/provider"
)

// classifyErr wraps an SDK error with one of the provider sentinels
// (ErrRateLimit, ErrAuth, ErrServer, ErrNetwork) so consumers can
// switch on errors.Is without inspecting vendor-specific types.
//
// Errors that don't carry an HTTP status fall through ClassifyTransport
// to catch DNS/connection failures.
func classifyErr(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *anth.Error
	if errors.As(err, &apiErr) {
		return provider.WrapHTTPStatus(err, apiErr.StatusCode)
	}
	return provider.ClassifyTransport(err)
}
