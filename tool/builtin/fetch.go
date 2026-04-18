package builtin

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/jerkeyray/starling/tool"
)

// fetchMaxBytes caps the response body at 1 MiB. Oversize bodies are
// truncated to this limit, not errored — documented behavior.
const fetchMaxBytes = 1 << 20

// fetchTimeout bounds the total round-trip of a single Fetch call.
const fetchTimeout = 15 * time.Second

// FetchInput is the JSON-schema-describing input type for the Fetch tool.
type FetchInput struct {
	URL string `json:"url" jsonschema:"description=Absolute URL to GET."`
}

// FetchOutput is the JSON-schema-describing output type for the Fetch tool.
type FetchOutput struct {
	Status int    `json:"status"`
	Body   string `json:"body"`
}

// Fetch returns a tool that performs an HTTP GET against the given URL and
// returns status code + body (capped at 1 MiB). No retries, no redirect
// customization; callers that need those should wrap this tool.
func Fetch() tool.Tool {
	return tool.Typed[FetchInput, FetchOutput](
		"fetch",
		"Perform an HTTP GET against an absolute URL and return status + body (body capped at 1 MiB).",
		func(ctx context.Context, in FetchInput) (FetchOutput, error) {
			if in.URL == "" {
				return FetchOutput{}, fmt.Errorf("fetch: URL is required")
			}
			ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
			defer cancel()

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, in.URL, nil)
			if err != nil {
				return FetchOutput{}, fmt.Errorf("fetch: build request: %w", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return FetchOutput{}, fmt.Errorf("fetch: do request: %w", err)
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(io.LimitReader(resp.Body, fetchMaxBytes))
			if err != nil {
				return FetchOutput{}, fmt.Errorf("fetch: read body: %w", err)
			}
			return FetchOutput{Status: resp.StatusCode, Body: string(body)}, nil
		},
	)
}
