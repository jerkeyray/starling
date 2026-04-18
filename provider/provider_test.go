package provider_test

import (
	"context"
	"io"
	"testing"

	"github.com/jerkeyray/starling/provider"
)

func TestChunkKind_String(t *testing.T) {
	cases := []struct {
		k    provider.ChunkKind
		want string
	}{
		{provider.ChunkText, "ChunkText"},
		{provider.ChunkReasoning, "ChunkReasoning"},
		{provider.ChunkToolUseStart, "ChunkToolUseStart"},
		{provider.ChunkToolUseDelta, "ChunkToolUseDelta"},
		{provider.ChunkToolUseEnd, "ChunkToolUseEnd"},
		{provider.ChunkUsage, "ChunkUsage"},
		{provider.ChunkEnd, "ChunkEnd"},
		{provider.ChunkKind(0), "ChunkKind(0)"},
		{provider.ChunkKind(99), "ChunkKind(99)"},
	}
	for _, c := range cases {
		if got := c.k.String(); got != c.want {
			t.Errorf("ChunkKind(%d).String() = %q, want %q", uint8(c.k), got, c.want)
		}
	}
}

func TestRole_String(t *testing.T) {
	cases := []struct {
		r    provider.Role
		want string
	}{
		{provider.RoleSystem, "system"},
		{provider.RoleUser, "user"},
		{provider.RoleAssistant, "assistant"},
		{provider.RoleTool, "tool"},
		{provider.Role("unknown-role"), "unknown-role"},
	}
	for _, c := range cases {
		if got := c.r.String(); got != c.want {
			t.Errorf("Role(%q).String() = %q, want %q", string(c.r), got, c.want)
		}
	}
}

// emptyStream is a trivial EventStream that reports EOF immediately. Used
// to satisfy the interface in the shape test.
type emptyStream struct{}

func (emptyStream) Next(context.Context) (provider.StreamChunk, error) {
	return provider.StreamChunk{}, io.EOF
}
func (emptyStream) Close() error { return nil }

// stubProvider satisfies provider.Provider. The test below assigns it to a
// provider.Provider variable; if the interface shape drifts, this file
// fails to compile — which is the point.
type stubProvider struct{}

func (stubProvider) Info() provider.Info {
	return provider.Info{ID: "stub", APIVersion: "v0"}
}

func (stubProvider) Stream(context.Context, *provider.Request) (provider.EventStream, error) {
	return emptyStream{}, nil
}

func TestProvider_InterfaceShape(t *testing.T) {
	var p provider.Provider = stubProvider{}
	info := p.Info()
	if info.ID != "stub" || info.APIVersion != "v0" {
		t.Fatalf("Info() round-trip failed: %+v", info)
	}
	stream, err := p.Stream(context.Background(), &provider.Request{Model: "x"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	if _, err := stream.Next(context.Background()); err != io.EOF {
		t.Fatalf("Next: want io.EOF, got %v", err)
	}
}

func TestStreamChunk_ZeroValue(t *testing.T) {
	var c provider.StreamChunk
	if c.Kind != 0 || c.Text != "" || c.ToolUse != nil || c.Usage != nil ||
		c.StopReason != "" || c.RawResponseHash != nil || c.ProviderReqID != "" {
		t.Fatalf("StreamChunk zero value has non-zero field: %+v", c)
	}
	c.Kind = provider.ChunkText
	if c.Text != "" {
		t.Fatalf("setting Kind should not touch Text")
	}
}
