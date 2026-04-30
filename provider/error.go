package provider

import "fmt"

// Error wraps an error from an LLM provider — stream-open failure or a
// mid-stream error. Provider is the provider ID (e.g. "openai");
// Code carries an HTTP status if the adapter surfaced one, 0 otherwise.
//
// Lives in the provider package so the step package (which sits below
// the root starling package in the import graph) can construct it
// without a cycle. Re-exported as starling.ProviderError via type alias.
type Error struct {
	Provider string
	Code     int
	Err      error
}

// Error implements the error interface, including the provider ID and
// HTTP status code (when non-zero).
func (e *Error) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("starling: provider %q (status %d): %v", e.Provider, e.Code, e.Err)
	}
	return fmt.Sprintf("starling: provider %q: %v", e.Provider, e.Err)
}

// Unwrap returns the underlying provider error so callers can route on
// it with errors.Is / errors.As.
func (e *Error) Unwrap() error { return e.Err }
