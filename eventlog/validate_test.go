package eventlog_test

import (
	"errors"
	"testing"

	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/internal/merkle"
)

// ---- fixture builder -----------------------------------------------------
//
// runBuilder hand-crafts a valid run so tamper tests can mutate single
// fields without fighting the agent machinery. Each Add call appends an
// event with correctly computed Seq and PrevHash.
type runBuilder struct {
	runID string
	evs   []event.Event
}

func newBuilder(runID string) *runBuilder {
	return &runBuilder{runID: runID}
}

func (b *runBuilder) add(t *testing.T, kind event.Kind, payload any) {
	t.Helper()
	enc, err := encodeAny(payload)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	ev := event.Event{
		RunID:     b.runID,
		Seq:       uint64(len(b.evs)) + 1,
		Timestamp: int64(len(b.evs)+1) * 1_000,
		Kind:      kind,
		Payload:   enc,
	}
	if len(b.evs) > 0 {
		prev := b.evs[len(b.evs)-1]
		raw, err := event.Marshal(prev)
		if err != nil {
			t.Fatalf("marshal prev: %v", err)
		}
		ev.PrevHash = event.Hash(raw)
	}
	b.evs = append(b.evs, ev)
}

// finish appends a RunCompleted terminal with the correct MerkleRoot.
func (b *runBuilder) finish(t *testing.T) []event.Event {
	t.Helper()
	leaves, err := merkle.EventHashes(b.evs)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	root := merkle.Root(leaves)
	b.add(t, event.KindRunCompleted, event.RunCompleted{
		FinalText:  "done",
		MerkleRoot: root,
		DurationMs: 5,
	})
	return b.evs
}

func encodeAny(p any) (cborRaw, error) {
	// Route through EncodePayload via a type switch so we don't need to
	// expose a generic wrapper publicly.
	switch v := p.(type) {
	case event.RunStarted:
		return event.EncodePayload(v)
	case event.TurnStarted:
		return event.EncodePayload(v)
	case event.AssistantMessageCompleted:
		return event.EncodePayload(v)
	case event.ToolCallScheduled:
		return event.EncodePayload(v)
	case event.ToolCallCompleted:
		return event.EncodePayload(v)
	case event.ToolCallFailed:
		return event.EncodePayload(v)
	case event.BudgetExceeded:
		return event.EncodePayload(v)
	case event.RunCompleted:
		return event.EncodePayload(v)
	case event.RunFailed:
		return event.EncodePayload(v)
	case event.RunCancelled:
		return event.EncodePayload(v)
	}
	return nil, errors.New("unknown payload type")
}

type cborRaw = []byte

// goodRun returns a valid 4-event run.
func goodRun(t *testing.T) []event.Event {
	t.Helper()
	b := newBuilder("run-001")
	b.add(t, event.KindRunStarted, event.RunStarted{
		SchemaVersion: event.SchemaVersion,
		Goal:          "hi",
		ProviderID:    "canned",
		ModelID:       "m",
	})
	b.add(t, event.KindTurnStarted, event.TurnStarted{
		TurnID: "t1", InputTokens: 5,
	})
	b.add(t, event.KindAssistantMessageCompleted, event.AssistantMessageCompleted{
		TurnID: "t1", Text: "ok", InputTokens: 5, OutputTokens: 2,
	})
	return b.finish(t)
}

// reencode is a tamper helper: overwrites a payload at index i.
func reencode(t *testing.T, evs []event.Event, i int, payload any) {
	t.Helper()
	enc, err := encodeAny(payload)
	if err != nil {
		t.Fatalf("reencode: %v", err)
	}
	evs[i].Payload = enc
}

// ---- tests ---------------------------------------------------------------

func TestValidate_GoodRun(t *testing.T) {
	if err := eventlog.Validate(goodRun(t)); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidate_Empty(t *testing.T) {
	err := eventlog.Validate(nil)
	if !errors.Is(err, eventlog.ErrLogCorrupt) {
		t.Fatalf("err = %v, want wraps ErrLogCorrupt", err)
	}
}

func TestValidate_SeqGap(t *testing.T) {
	evs := goodRun(t)
	evs[2].Seq = 99
	err := eventlog.Validate(evs)
	if !errors.Is(err, eventlog.ErrLogCorrupt) {
		t.Fatalf("err = %v, want ErrLogCorrupt", err)
	}
}

func TestValidate_PrevHashTampered(t *testing.T) {
	evs := goodRun(t)
	evs[2].PrevHash[0] ^= 0xff
	err := eventlog.Validate(evs)
	if !errors.Is(err, eventlog.ErrLogCorrupt) {
		t.Fatalf("err = %v, want ErrLogCorrupt", err)
	}
}

func TestValidate_PayloadTampered(t *testing.T) {
	evs := goodRun(t)
	// Mutate event[1]'s payload without fixing event[2].PrevHash. The
	// chain check should catch it at index 2.
	reencode(t, evs, 1, event.TurnStarted{TurnID: "evil", InputTokens: 999})
	err := eventlog.Validate(evs)
	if !errors.Is(err, eventlog.ErrLogCorrupt) {
		t.Fatalf("err = %v, want ErrLogCorrupt", err)
	}
}

func TestValidate_RunIDMismatch(t *testing.T) {
	evs := goodRun(t)
	evs[2].RunID = "other-run"
	err := eventlog.Validate(evs)
	if !errors.Is(err, eventlog.ErrLogCorrupt) {
		t.Fatalf("err = %v, want ErrLogCorrupt", err)
	}
}

func TestValidate_NoTerminal(t *testing.T) {
	b := newBuilder("run-002")
	b.add(t, event.KindRunStarted, event.RunStarted{SchemaVersion: event.SchemaVersion})
	b.add(t, event.KindTurnStarted, event.TurnStarted{TurnID: "t1"})
	err := eventlog.Validate(b.evs)
	if !errors.Is(err, eventlog.ErrLogCorrupt) {
		t.Fatalf("err = %v, want ErrLogCorrupt", err)
	}
}

func TestValidate_WrongMerkleRoot(t *testing.T) {
	evs := goodRun(t)
	// Overwrite terminal payload with a zeroed MerkleRoot. Keep the
	// envelope so seq/prev-hash checks still pass (they hash the
	// previous event, not the mutated one).
	last := len(evs) - 1
	reencode(t, evs, last, event.RunCompleted{
		FinalText:  "done",
		MerkleRoot: make([]byte, 32),
		DurationMs: 5,
	})
	err := eventlog.Validate(evs)
	if !errors.Is(err, eventlog.ErrLogCorrupt) {
		t.Fatalf("err = %v, want ErrLogCorrupt", err)
	}
}

// finishWith appends an arbitrary terminal payload with the correct
// MerkleRoot computed over the events appended so far.
func (b *runBuilder) finishWith(t *testing.T, kind event.Kind, payload any) []event.Event {
	t.Helper()
	leaves, err := merkle.EventHashes(b.evs)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	root := merkle.Root(leaves)
	switch p := payload.(type) {
	case event.RunCompleted:
		p.MerkleRoot = root
		b.add(t, kind, p)
	case event.RunFailed:
		p.MerkleRoot = root
		b.add(t, kind, p)
	case event.RunCancelled:
		p.MerkleRoot = root
		b.add(t, kind, p)
	default:
		t.Fatalf("finishWith: unsupported terminal payload %T", payload)
	}
	return b.evs
}

func TestValidate_FirstEventNotRunStarted(t *testing.T) {
	b := newBuilder("run-100")
	b.add(t, event.KindTurnStarted, event.TurnStarted{TurnID: "t1"})
	b.add(t, event.KindAssistantMessageCompleted, event.AssistantMessageCompleted{TurnID: "t1"})
	evs := b.finish(t)
	err := eventlog.Validate(evs)
	if !errors.Is(err, eventlog.ErrLogCorrupt) {
		t.Fatalf("err = %v, want ErrLogCorrupt", err)
	}
}

func TestValidate_UnsupportedSchemaVersion(t *testing.T) {
	b := newBuilder("run-101")
	b.add(t, event.KindRunStarted, event.RunStarted{SchemaVersion: 99})
	b.add(t, event.KindTurnStarted, event.TurnStarted{TurnID: "t1"})
	b.add(t, event.KindAssistantMessageCompleted, event.AssistantMessageCompleted{TurnID: "t1"})
	evs := b.finish(t)
	err := eventlog.Validate(evs)
	if !errors.Is(err, eventlog.ErrLogCorrupt) {
		t.Fatalf("err = %v, want ErrLogCorrupt", err)
	}
}

func TestValidate_TurnNotClosed(t *testing.T) {
	b := newBuilder("run-102")
	b.add(t, event.KindRunStarted, event.RunStarted{SchemaVersion: event.SchemaVersion})
	b.add(t, event.KindTurnStarted, event.TurnStarted{TurnID: "t1"})
	// Open another turn without closing the first.
	b.add(t, event.KindTurnStarted, event.TurnStarted{TurnID: "t2"})
	b.add(t, event.KindAssistantMessageCompleted, event.AssistantMessageCompleted{TurnID: "t2"})
	evs := b.finish(t)
	err := eventlog.Validate(evs)
	if !errors.Is(err, eventlog.ErrLogCorrupt) {
		t.Fatalf("err = %v, want ErrLogCorrupt", err)
	}
}

func TestValidate_TurnIDMismatch(t *testing.T) {
	b := newBuilder("run-103")
	b.add(t, event.KindRunStarted, event.RunStarted{SchemaVersion: event.SchemaVersion})
	b.add(t, event.KindTurnStarted, event.TurnStarted{TurnID: "t1"})
	b.add(t, event.KindAssistantMessageCompleted, event.AssistantMessageCompleted{TurnID: "wrong"})
	evs := b.finish(t)
	err := eventlog.Validate(evs)
	if !errors.Is(err, eventlog.ErrLogCorrupt) {
		t.Fatalf("err = %v, want ErrLogCorrupt", err)
	}
}

func TestValidate_OpenTurnAtRunCompleted(t *testing.T) {
	b := newBuilder("run-104")
	b.add(t, event.KindRunStarted, event.RunStarted{SchemaVersion: event.SchemaVersion})
	b.add(t, event.KindTurnStarted, event.TurnStarted{TurnID: "t1"})
	evs := b.finish(t) // RunCompleted with open turn
	err := eventlog.Validate(evs)
	if !errors.Is(err, eventlog.ErrLogCorrupt) {
		t.Fatalf("err = %v, want ErrLogCorrupt", err)
	}
}

func TestValidate_OpenTurnAtRunFailed_OK(t *testing.T) {
	b := newBuilder("run-105")
	b.add(t, event.KindRunStarted, event.RunStarted{SchemaVersion: event.SchemaVersion})
	b.add(t, event.KindTurnStarted, event.TurnStarted{TurnID: "t1"})
	evs := b.finishWith(t, event.KindRunFailed, event.RunFailed{Error: "boom", ErrorType: "internal"})
	if err := eventlog.Validate(evs); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidate_OrphanToolCompleted(t *testing.T) {
	b := newBuilder("run-106")
	b.add(t, event.KindRunStarted, event.RunStarted{SchemaVersion: event.SchemaVersion})
	b.add(t, event.KindTurnStarted, event.TurnStarted{TurnID: "t1"})
	b.add(t, event.KindAssistantMessageCompleted, event.AssistantMessageCompleted{TurnID: "t1"})
	b.add(t, event.KindToolCallCompleted, event.ToolCallCompleted{CallID: "c1", Attempt: 1})
	evs := b.finish(t)
	err := eventlog.Validate(evs)
	if !errors.Is(err, eventlog.ErrLogCorrupt) {
		t.Fatalf("err = %v, want ErrLogCorrupt", err)
	}
}

func TestValidate_DuplicateToolOutcome(t *testing.T) {
	b := newBuilder("run-107")
	b.add(t, event.KindRunStarted, event.RunStarted{SchemaVersion: event.SchemaVersion})
	b.add(t, event.KindTurnStarted, event.TurnStarted{TurnID: "t1"})
	b.add(t, event.KindAssistantMessageCompleted, event.AssistantMessageCompleted{TurnID: "t1"})
	b.add(t, event.KindToolCallScheduled, event.ToolCallScheduled{CallID: "c1", TurnID: "t1", ToolName: "x", Attempt: 1})
	b.add(t, event.KindToolCallCompleted, event.ToolCallCompleted{CallID: "c1", Attempt: 1})
	b.add(t, event.KindToolCallCompleted, event.ToolCallCompleted{CallID: "c1", Attempt: 1})
	evs := b.finish(t)
	err := eventlog.Validate(evs)
	if !errors.Is(err, eventlog.ErrLogCorrupt) {
		t.Fatalf("err = %v, want ErrLogCorrupt", err)
	}
}

func TestValidate_OpenToolAtRunCompleted(t *testing.T) {
	b := newBuilder("run-108")
	b.add(t, event.KindRunStarted, event.RunStarted{SchemaVersion: event.SchemaVersion})
	b.add(t, event.KindTurnStarted, event.TurnStarted{TurnID: "t1"})
	b.add(t, event.KindAssistantMessageCompleted, event.AssistantMessageCompleted{TurnID: "t1"})
	b.add(t, event.KindToolCallScheduled, event.ToolCallScheduled{CallID: "c1", TurnID: "t1", ToolName: "x", Attempt: 1})
	evs := b.finish(t)
	err := eventlog.Validate(evs)
	if !errors.Is(err, eventlog.ErrLogCorrupt) {
		t.Fatalf("err = %v, want ErrLogCorrupt", err)
	}
}

func TestValidate_ToolRetrySharedCallID_OK(t *testing.T) {
	b := newBuilder("run-109")
	b.add(t, event.KindRunStarted, event.RunStarted{SchemaVersion: event.SchemaVersion})
	b.add(t, event.KindTurnStarted, event.TurnStarted{TurnID: "t1"})
	b.add(t, event.KindAssistantMessageCompleted, event.AssistantMessageCompleted{TurnID: "t1"})
	b.add(t, event.KindToolCallScheduled, event.ToolCallScheduled{CallID: "c1", TurnID: "t1", ToolName: "x", Attempt: 1})
	b.add(t, event.KindToolCallFailed, event.ToolCallFailed{CallID: "c1", Attempt: 1, ErrorType: "timeout", Error: "t/o"})
	b.add(t, event.KindToolCallScheduled, event.ToolCallScheduled{CallID: "c1", TurnID: "t1", ToolName: "x", Attempt: 2})
	b.add(t, event.KindToolCallCompleted, event.ToolCallCompleted{CallID: "c1", Attempt: 2})
	evs := b.finish(t)
	if err := eventlog.Validate(evs); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidate_TerminalBeforeEnd(t *testing.T) {
	b := newBuilder("run-003")
	b.add(t, event.KindRunStarted, event.RunStarted{SchemaVersion: event.SchemaVersion})
	// Stuff a terminal in the middle.
	b.add(t, event.KindRunCompleted, event.RunCompleted{FinalText: "early"})
	b.add(t, event.KindTurnStarted, event.TurnStarted{TurnID: "t1"})
	// Real terminal at the end.
	leaves, _ := merkle.EventHashes(b.evs)
	root := merkle.Root(leaves)
	b.add(t, event.KindRunCompleted, event.RunCompleted{MerkleRoot: root})
	err := eventlog.Validate(b.evs)
	if !errors.Is(err, eventlog.ErrLogCorrupt) {
		t.Fatalf("err = %v, want ErrLogCorrupt", err)
	}
}
