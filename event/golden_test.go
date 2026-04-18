package event_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/internal/cborenc"
)

// goldenFixture is a fully-populated, deterministic RunStarted event wrapped
// in an Event envelope. Every field has a fixed value; byte output therefore
// must never change unless the wire format (CBOR tags, field order, encoding
// mode, schema version) changes — which is exactly what we want to detect.
func goldenFixture(t *testing.T) event.Event {
	t.Helper()
	paramsBytes, err := cborenc.Marshal(map[string]any{
		"max_tokens":  int64(1024),
		"temperature": 0.7,
	})
	if err != nil {
		t.Fatalf("params marshal: %v", err)
	}
	rs := event.RunStarted{
		SchemaVersion:    event.SchemaVersion,
		Goal:             "summarize the article",
		ProviderID:       "openai",
		ModelID:          "gpt-4o-mini",
		APIVersion:       "v1",
		ParamsHash:       bytes.Repeat([]byte{0xaa}, 32),
		Params:           cborenc.RawMessage(paramsBytes),
		SystemPromptHash: bytes.Repeat([]byte{0xbb}, 32),
		SystemPrompt:     "You are a helpful assistant.",
		ToolRegistryHash: bytes.Repeat([]byte{0xcc}, 32),
		ToolSchemas: []event.ToolSchemaRef{
			{Name: "fetch", SchemaHash: bytes.Repeat([]byte{0x11}, 32)},
			{Name: "read_file", SchemaHash: bytes.Repeat([]byte{0x22}, 32)},
		},
		Budget: &event.BudgetLimits{
			MaxOutputTokens: 2000,
			MaxUSD:          0.10,
		},
	}
	payload, err := event.EncodePayload(rs)
	if err != nil {
		t.Fatalf("EncodePayload: %v", err)
	}
	return event.Event{
		RunID:     "01HXYZABCDEFGHJKMNPQRSTVWX",
		Seq:       1,
		PrevHash:  nil,
		Timestamp: 1_700_000_000_000_000_000, // fixed unix nanos
		Kind:      event.KindRunStarted,
		Payload:   payload,
	}
}

const goldenPath = "testdata/run_started.golden.cbor"

// TestGolden_WireFormat is the wire-format anchor. If this test fails after
// a code change, the change is altering the bytes Starling writes to disk —
// which would break every existing run log. If the change is intentional,
// bump SchemaVersion and regenerate the fixture with:
//
//	GOLDEN_UPDATE=1 go test ./event -run TestGolden_WireFormat
func TestGolden_WireFormat(t *testing.T) {
	ev := goldenFixture(t)
	got, err := event.Marshal(ev)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	if os.Getenv("GOLDEN_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("wrote %d bytes to %s", len(got), goldenPath)
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (run with GOLDEN_UPDATE=1 to create it): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("wire format changed!\n"+
			"  golden:  %d bytes\n"+
			"  current: %d bytes\n"+
			"If this change is intentional, bump SchemaVersion and regenerate the\n"+
			"fixture with: GOLDEN_UPDATE=1 go test ./event -run TestGolden_WireFormat",
			len(want), len(got))
	}
}

// TestGolden_DecodesCleanly ensures the committed fixture always decodes back
// to the same RunStarted payload — a sanity check that Unmarshal remains
// compatible with the on-disk bytes.
func TestGolden_DecodesCleanly(t *testing.T) {
	data, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Skipf("golden fixture missing: %v", err)
	}
	ev, err := event.Unmarshal(data)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if ev.Kind != event.KindRunStarted {
		t.Fatalf("kind = %s, want RunStarted", ev.Kind)
	}
	rs, err := ev.AsRunStarted()
	if err != nil {
		t.Fatalf("AsRunStarted: %v", err)
	}
	if rs.SchemaVersion != event.SchemaVersion {
		t.Fatalf("schema_version = %d, want %d", rs.SchemaVersion, event.SchemaVersion)
	}
	if rs.ProviderID != "openai" || rs.ModelID != "gpt-4o-mini" {
		t.Fatalf("provider/model mismatch: %s/%s", rs.ProviderID, rs.ModelID)
	}
}
