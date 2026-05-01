package starling_test

import (
	"bytes"
	"strings"
	"testing"

	starling "github.com/jerkeyray/starling"
)

func TestDoctor_NoArgs_PassesWhenEnvHasOneKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "fake-key-for-test")
	for _, k := range []string{"ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY", "OPENROUTER_API_KEY", "AWS_ACCESS_KEY_ID"} {
		t.Setenv(k, "")
	}
	var buf bytes.Buffer
	cmd := starling.DoctorCommand()
	cmd.Output = &buf
	if err := cmd.Run(nil); err != nil {
		t.Fatalf("Run: %v\noutput:\n%s", err, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "✓ starling version") {
		t.Fatalf("missing version line: %s", out)
	}
	if !strings.Contains(out, "OPENAI_API_KEY") {
		t.Fatalf("missing OPENAI_API_KEY line: %s", out)
	}
}

func TestDoctor_NoEnvKeys_Fails(t *testing.T) {
	for _, k := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY", "OPENROUTER_API_KEY", "AWS_ACCESS_KEY_ID"} {
		t.Setenv(k, "")
	}
	var buf bytes.Buffer
	cmd := starling.DoctorCommand()
	cmd.Output = &buf
	if err := cmd.Run(nil); err == nil {
		t.Fatalf("expected failure with no provider env vars\noutput:\n%s", buf.String())
	}
}
