package bedrock

import (
	"errors"

	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/jerkeyray/starling/provider"
)

// classifyErr wraps an SDK error with one of the provider sentinels
// (ErrRateLimit, ErrAuth, ErrServer, ErrNetwork) so consumers can
// switch on errors.Is without inspecting AWS-specific types.
//
// Bedrock is fronted by smithy-go; HTTP responses come back wrapped
// in *smithyhttp.ResponseError, which carries the underlying status.
func classifyErr(err error) error {
	if err == nil {
		return nil
	}
	var respErr *smithyhttp.ResponseError
	if errors.As(err, &respErr) && respErr.Response != nil {
		return provider.WrapHTTPStatus(err, respErr.Response.StatusCode)
	}
	return provider.ClassifyTransport(err)
}
