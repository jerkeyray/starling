package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"

	starling "github.com/jerkeyray/starling"
	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/replay"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func main() {
	if err := dispatch(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "incident_triage:", err)
		os.Exit(1)
	}
}

func dispatch(args []string) error {
	if len(args) == 0 {
		return runOnce(nil)
	}
	switch args[0] {
	case "run":
		return runOnce(args[1:])
	case "inspect":
		return runInspect(args[1:])
	case "replay":
		return runReplay(args[1:])
	case "resume":
		return runResume(args[1:])
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  incident_triage run                     # run the canned scenario")
	fmt.Fprintln(os.Stderr, "  incident_triage inspect [db]            # open inspector with replay wired")
	fmt.Fprintln(os.Stderr, "  incident_triage replay <db> <runID>     # headless replay verification")
	fmt.Fprintln(os.Stderr, "  incident_triage resume <db> <runID>     # resume a crashed run")
}

// ----------------------------------------------------------------------
// run
// ----------------------------------------------------------------------

func runOnce(_ []string) error {
	shutdown := maybeOTel()
	defer shutdown()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	reg := prometheus.NewRegistry()
	metrics := starling.NewMetrics(reg)
	stopMetricsHTTP := maybeServeMetrics(reg)
	defer stopMetricsHTTP()

	a, err := buildAgent(ctx, buildOpts{
		dbPath:    defaultDB,
		useCanned: true,
		notify:    func(_, _ string) error { return nil },
		metrics:   metrics,
	})
	if err != nil {
		return err
	}
	defer a.Log.(interface{ Close() error }).Close()

	res, runErr := a.Run(ctx, "checkout-api error_rate spiked at 14:00 UTC; triage and escalate if necessary.")
	if res != nil {
		fmt.Println(summarizeRun(res))
		fmt.Println("final:", res.FinalText)
		if vErr := loadValidate(a.Log, res.RunID); vErr != nil {
			fmt.Println("validate: FAIL:", vErr)
		} else {
			fmt.Println("validate: ok")
		}
		fmt.Println("\nnext: incident_triage inspect " + defaultDB)
	}
	return runErr
}

// ----------------------------------------------------------------------
// inspect
// ----------------------------------------------------------------------

func runInspect(args []string) error {
	factory := replay.Factory(func(ctx context.Context) (replay.Agent, error) {
		// Use canned + no-op notify on replay so re-execution stays
		// deterministic and never pages a real channel.
		return buildAgent(ctx, buildOpts{
			dbPath:    defaultDB,
			useCanned: true,
			notify:    func(_, _ string) error { return nil },
		})
	})
	cmd := starling.InspectCommand(factory)
	cmd.Name = "incident_triage inspect"
	return cmd.Run(args)
}

// ----------------------------------------------------------------------
// replay (headless)
// ----------------------------------------------------------------------

func runReplay(args []string) error {
	factory := replay.Factory(func(ctx context.Context) (replay.Agent, error) {
		return buildAgent(ctx, buildOpts{
			dbPath:    defaultDB,
			useCanned: true,
			notify:    func(_, _ string) error { return nil },
		})
	})
	cmd := starling.ReplayCommand(factory)
	cmd.Name = "incident_triage replay"
	return cmd.Run(args)
}

// ----------------------------------------------------------------------
// resume
// ----------------------------------------------------------------------

func runResume(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: incident_triage resume <db> <runID>")
	}
	dbPath, runID := args[0], args[1]

	a, err := buildAgent(context.Background(), buildOpts{
		dbPath:    dbPath,
		useCanned: true,
		notify:    func(_, _ string) error { return nil },
	})
	if err != nil {
		return err
	}
	defer a.Log.(interface{ Close() error }).Close()

	res, err := a.Resume(context.Background(), runID, "")
	if err != nil {
		return err
	}
	fmt.Println("resumed:", summarizeRun(res))
	return nil
}

// ----------------------------------------------------------------------
// otel
// ----------------------------------------------------------------------

func maybeOTel() func() {
	if os.Getenv("OTEL") != "1" {
		return func() {}
	}
	exp, err := stdouttrace.New(stdouttrace.WithPrettyPrint(), stdouttrace.WithoutTimestamps())
	if err != nil {
		fmt.Fprintln(os.Stderr, "otel:", err)
		return func() {}
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp))
	otel.SetTracerProvider(tp)
	return func() { _ = tp.Shutdown(context.Background()) }
}

// maybeServeMetrics exposes the Prometheus registry on $METRICS_ADDR
// (e.g. :9090). Disabled when METRICS_ADDR is empty.
func maybeServeMetrics(reg prometheus.Gatherer) func() {
	addr := os.Getenv("METRICS_ADDR")
	if addr == "" {
		return func() {}
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", starling.MetricsHandler(reg))
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintln(os.Stderr, "metrics server:", err)
		}
	}()
	return func() { _ = srv.Shutdown(context.Background()) }
}

// silence unused-import linter when only the type assertion in
// runResume references eventlog.
var _ = eventlog.NewInMemory
