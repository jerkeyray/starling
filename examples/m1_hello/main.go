// Command m1_hello is Starling's end-to-end demo. It builds an Agent
// with one tool (current_time), points it at an LLM provider, and —
// depending on the subcommand — either runs the agent or opens the
// inspector with replay wired up.
//
// Two modes:
//
//	# Run the agent; events are written to the SQLite db.
//	OPENAI_API_KEY=sk-... go run ./examples/m1_hello run
//
//	# Open the inspector on the db. Clicking "Replay" on a run
//	# re-executes the recorded turn sequence against this binary's
//	# Agent construction, so recorded and produced events can be
//	# diffed side-by-side.
//	go run ./examples/m1_hello inspect ./runs.db
//
// The two commands share the SAME buildAgent function: that's the
// whole point of dual-mode — the inspector uses your real agent
// factory as its replay factory, so what you debug is what you ran.
//
// Provider selection:
//
//	PROVIDER=anthropic ANTHROPIC_API_KEY=sk-ant-... go run ./examples/m1_hello run
//
// Groq / OpenAI-compatible:
//
//	OPENAI_API_KEY=$GROQ_API_KEY \
//	OPENAI_BASE_URL=https://api.groq.com/openai/v1 \
//	MODEL=llama-3.1-8b-instant \
//	  go run ./examples/m1_hello run
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"time"

	starling "github.com/jerkeyray/starling"
	"github.com/jerkeyray/starling/event"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/provider"
	"github.com/jerkeyray/starling/provider/anthropic"
	"github.com/jerkeyray/starling/provider/openai"
	"github.com/jerkeyray/starling/replay"
	"github.com/jerkeyray/starling/step"
	"github.com/jerkeyray/starling/tool"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// defaultDB is where "run" writes and where "inspect" reads by default.
// Relative path keeps the demo self-contained — run it from any
// workspace and the db lands there.
const defaultDB = "./runs.db"

type clockIn struct{}

type clockOut struct {
	UTC string `json:"utc"`
}

func currentTimeTool() tool.Tool {
	return tool.Typed(
		"current_time",
		"Return the current UTC time in RFC3339.",
		func(ctx context.Context, _ clockIn) (clockOut, error) {
			// step.Now records the timestamp on the original run and
			// replays the recorded value, so the tool is replay-safe.
			return clockOut{UTC: step.Now(ctx).UTC().Format(time.RFC3339)}, nil
		},
	)
}

func main() {
	if err := dispatch(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "m1_hello:", err)
		os.Exit(1)
	}
}

// dispatch is the subcommand router. Default is "run" so older docs
// that say `go run ./examples/m1_hello` still work.
func dispatch(args []string) error {
	if len(args) == 0 {
		return runAgent([]string{})
	}
	switch args[0] {
	case "run":
		return runAgent(args[1:])
	case "inspect":
		return runInspect(args[1:])
	case "replay":
		return runReplay(args[1:])
	case "-h", "--help", "help":
		printUsage()
		return nil
	default:
		printUsage()
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  m1_hello run               # run the agent; events → "+defaultDB)
	fmt.Fprintln(os.Stderr, "  m1_hello inspect [db]      # open the inspector with replay (default db: "+defaultDB+")")
	fmt.Fprintln(os.Stderr, "  m1_hello replay <db> <id>  # headless replay verification (exits nonzero on drift)")
}

// ----------------------------------------------------------------------
// mode: run
// ----------------------------------------------------------------------

func runAgent(_ []string) error {
	shutdownOtel := maybeInstallOtel()
	defer shutdownOtel()

	a, err := buildAgent(context.Background())
	if err != nil {
		return err
	}
	// Close the log on exit (only the run path owns the log; replay
	// builds an in-memory sink of its own).
	defer a.Log.(interface{ Close() error }).Close()

	// Honour Ctrl-C so cancellation lands as RunCancelled in the log.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	res, runErr := a.Run(ctx, "What is the current UTC time? Use the current_time tool, then state the time in one short sentence.")

	if res != nil {
		printResult(res)
		evs, _ := a.Log.Read(context.Background(), res.RunID)
		printEvents(evs)
		if verr := eventlog.Validate(evs); verr != nil {
			fmt.Println("validate: FAIL:", verr)
		} else {
			fmt.Println("validate: ok")
		}
		fmt.Println("")
		fmt.Println("next: go run ./examples/m1_hello inspect " + defaultDB)
	}

	if runErr != nil {
		return fmt.Errorf("agent run: %w", runErr)
	}
	return nil
}

// ----------------------------------------------------------------------
// mode: inspect (dual-mode hook — the point of this example)
// ----------------------------------------------------------------------

// runInspect delegates to starling.InspectCommand with a replay factory
// that builds the SAME Agent the run path built. The inspector opens
// the db read-only; the factory only fires when the user clicks Replay.
func runInspect(args []string) error {
	factory := replay.Factory(func(ctx context.Context) (replay.Agent, error) {
		// Build a fresh Agent per replay session. No shared state: each
		// replay uses its own sink and its own provider handle.
		a, err := buildAgent(ctx)
		if err != nil {
			return nil, err
		}
		// The runtime-produced Agent already implements replay.Agent
		// (via RunReplay) and replay.StreamingAgent (via RunReplayInto)
		// — no adapter needed.
		return a, nil
	})
	cmd := starling.InspectCommand(factory)
	cmd.Name = "m1_hello inspect"
	return cmd.Run(args)
}

// runReplay re-executes a recorded run headlessly and prints whether it
// matches byte-for-byte. Useful for CI / smoke tests.
func runReplay(args []string) error {
	factory := replay.Factory(func(ctx context.Context) (replay.Agent, error) {
		return buildAgent(ctx)
	})
	cmd := starling.ReplayCommand(factory)
	cmd.Name = "m1_hello replay"
	return cmd.Run(args)
}

// ----------------------------------------------------------------------
// shared construction
// ----------------------------------------------------------------------

// buildAgent is the single source of truth for the Agent's
// configuration — same Provider, Tools, Config in run and replay. This
// is the whole thesis of the event-sourced-with-replay design: the
// only way replay stays faithful is if the factory is literally the
// same function that built the original run.
func buildAgent(_ context.Context) (*starling.Agent, error) {
	prov, model, err := buildProvider()
	if err != nil {
		return nil, err
	}

	// Durable SQLite log so `run` leaves a file the inspector can read.
	log, err := eventlog.NewSQLite(defaultDB)
	if err != nil {
		return nil, fmt.Errorf("open log: %w", err)
	}

	cfg := starling.Config{
		Model:    model,
		MaxTurns: 4,
	}
	if os.Getenv("DEBUG") == "1" {
		cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}

	return &starling.Agent{
		Provider: prov,
		Tools:    []tool.Tool{currentTimeTool()},
		Log:      log,
		Config:   cfg,
	}, nil
}

// maybeInstallOtel wires a stdout trace exporter when OTEL=1. Spans are
// printed as pretty JSON to stderr. Returns a shutdown func that
// flushes pending spans; no-op when OTEL is off.
func maybeInstallOtel() func() {
	if os.Getenv("OTEL") != "1" {
		return func() {}
	}
	exp, err := stdouttrace.New(stdouttrace.WithPrettyPrint(), stdouttrace.WithWriter(os.Stderr))
	if err != nil {
		fmt.Fprintln(os.Stderr, "otel stdout exporter:", err)
		return func() {}
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp))
	otel.SetTracerProvider(tp)
	fmt.Fprintln(os.Stderr, "observability: OTEL stdout exporter on")
	return func() { _ = tp.Shutdown(context.Background()) }
}

// buildProvider picks between OpenAI (default) and Anthropic based on
// the PROVIDER env var. Each branch honours its own MODEL env with a
// sensible default.
func buildProvider() (provider.Provider, string, error) {
	switch os.Getenv("PROVIDER") {
	case "anthropic":
		apiKey := os.Getenv("ANTHROPIC_API_KEY")
		if apiKey == "" {
			return nil, "", fmt.Errorf("ANTHROPIC_API_KEY not set")
		}
		model := os.Getenv("MODEL")
		if model == "" {
			model = "claude-sonnet-4-6"
		}
		prov, err := anthropic.New(anthropic.WithAPIKey(apiKey))
		if err != nil {
			return nil, "", fmt.Errorf("anthropic.New: %w", err)
		}
		return prov, model, nil

	default: // "openai" or empty
		apiKey := os.Getenv("OPENAI_API_KEY")
		if apiKey == "" {
			return nil, "", fmt.Errorf("OPENAI_API_KEY not set")
		}
		model := os.Getenv("MODEL")
		if model == "" {
			model = "gpt-4o-mini"
		}
		opts := []openai.Option{openai.WithAPIKey(apiKey)}
		if u := os.Getenv("OPENAI_BASE_URL"); u != "" {
			opts = append(opts, openai.WithBaseURL(u))
		}
		prov, err := openai.New(opts...)
		if err != nil {
			return nil, "", fmt.Errorf("openai.New: %w", err)
		}
		return prov, model, nil
	}
}

func printResult(r *starling.RunResult) {
	fmt.Println("=== RunResult ===")
	fmt.Println("RunID:        ", r.RunID)
	fmt.Println("FinalText:    ", r.FinalText)
	fmt.Println("TerminalKind: ", r.TerminalKind)
	fmt.Println("TurnCount:    ", r.TurnCount)
	fmt.Println("ToolCallCount:", r.ToolCallCount)
	fmt.Println("InputTokens:  ", r.InputTokens)
	fmt.Println("OutputTokens: ", r.OutputTokens)
	fmt.Printf("TotalCostUSD:  %.6f\n", r.TotalCostUSD)
	fmt.Println("Duration:     ", r.Duration)
	fmt.Printf("MerkleRoot:    %x\n", r.MerkleRoot)
}

func printEvents(evs []event.Event) {
	fmt.Println("=== Events ===")
	for _, ev := range evs {
		fmt.Printf("  %3d %s\n", ev.Seq, ev.Kind)
	}
	fmt.Printf("  (%d total)\n", len(evs))
}
