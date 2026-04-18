package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
)

// Typed wraps a strongly-typed function as a Tool. The In type's JSON
// Schema is derived via reflection at construction time; if In contains
// unsupported types (maps, interfaces, recursive structs, ...), Typed
// panics — this is programmer error, not runtime error.
//
// Execute recovers panics from fn and returns them wrapped in ErrPanicked.
func Typed[In, Out any](
	name, description string,
	fn func(context.Context, In) (Out, error),
) Tool {
	var zero In
	inType := reflect.TypeOf(zero)
	if inType == nil || inType.Kind() != reflect.Struct {
		panic(fmt.Sprintf("tool.Typed[%s]: In must be a struct (got %v); LLM tool schemas are objects at the top level", name, inType))
	}
	schema := generateSchema(inType)
	return &typedTool[In, Out]{
		name:        name,
		description: description,
		schema:      schema,
		fn:          fn,
	}
}

type typedTool[In, Out any] struct {
	name        string
	description string
	schema      json.RawMessage
	fn          func(context.Context, In) (Out, error)
}

func (t *typedTool[In, Out]) Name() string        { return t.name }
func (t *typedTool[In, Out]) Description() string { return t.description }

// Schema returns a copy of the schema bytes so callers cannot mutate the
// internal state and break schema determinism.
func (t *typedTool[In, Out]) Schema() json.RawMessage {
	out := make(json.RawMessage, len(t.schema))
	copy(out, t.schema)
	return out
}

func (t *typedTool[In, Out]) Execute(ctx context.Context, input json.RawMessage) (out json.RawMessage, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w: %v", ErrPanicked, r)
			out = nil
		}
	}()

	var in In
	// Empty input is accepted as the zero value — many tools have no
	// required fields and the model may send nothing.
	if len(input) > 0 {
		if uerr := json.Unmarshal(input, &in); uerr != nil {
			return nil, fmt.Errorf("tool %q: unmarshal input: %w", t.name, uerr)
		}
	}

	result, ferr := t.fn(ctx, in)
	if ferr != nil {
		return nil, ferr
	}

	b, merr := json.Marshal(result)
	if merr != nil {
		return nil, fmt.Errorf("tool %q: marshal output: %w", t.name, merr)
	}
	return json.RawMessage(b), nil
}
