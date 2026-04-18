package step

import (
	"sort"

	"github.com/jerkeyray/starling/tool"
)

// Registry maps tool names to Tool implementations for a single run.
// It is the runtime-side lookup used by CallTool; the tool package
// itself stays free of registry concerns so tool authors can define
// tools without importing runtime state.
//
// A Registry is immutable after construction — NewRegistry copies its
// inputs — which means callers can safely share one across goroutines.
type Registry struct {
	tools map[string]tool.Tool
	names []string // cached, alphabetical, matches tools keys
}

// NewRegistry returns a Registry containing the given tools, keyed by
// each tool's Name(). Duplicate names cause the later entry to win;
// callers that care about duplicate detection should check themselves
// before calling.
func NewRegistry(tools ...tool.Tool) *Registry {
	r := &Registry{tools: make(map[string]tool.Tool, len(tools))}
	for _, t := range tools {
		r.tools[t.Name()] = t
	}
	r.names = make([]string, 0, len(r.tools))
	for name := range r.tools {
		r.names = append(r.names, name)
	}
	sort.Strings(r.names)
	return r
}

// Get returns the Tool registered under name, or (nil, false) if not
// found.
func (r *Registry) Get(name string) (tool.Tool, bool) {
	if r == nil {
		return nil, false
	}
	t, ok := r.tools[name]
	return t, ok
}

// Names returns the registered tool names in alphabetical order. The
// slice is a fresh copy; callers may mutate it.
//
// Deterministic ordering matters for RunStarted.ToolRegistryHash: the
// hash is computed over ToolSchemas listed in the order Names returns.
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	out := make([]string, len(r.names))
	copy(out, r.names)
	return out
}
