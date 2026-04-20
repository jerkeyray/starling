package starling

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/jerkeyray/starling/eventlog"
	"github.com/jerkeyray/starling/inspect"
	"github.com/jerkeyray/starling/replay"
)

// InspectCommand returns a CLI-style entrypoint for the Starling
// inspector. Intended for dual-mode binaries: a user's agent binary
// that runs the agent in one mode and serves the inspector (with
// replay wired up) in another, so the same Go code that produced a run
// can replay it.
//
// Shape:
//
//	func main() {
//	    if len(os.Args) > 1 && os.Args[1] == "inspect" {
//	        cmd := starling.InspectCommand(myAgentFactory)
//	        if err := cmd.Run(os.Args[2:]); err != nil {
//	            log.Fatal(err)
//	        }
//	        return
//	    }
//	    // ... normal agent run ...
//	}
//
// factory may be nil: the inspector runs read-only (no Replay button).
// When non-nil, it is invoked once per replay session to construct a
// fresh agent configured equivalently to the original run.
//
// The returned *InspectCmd is safe to configure further via its
// exported fields before calling Run.
func InspectCommand(factory replay.Factory) *InspectCmd {
	return &InspectCmd{
		Factory: factory,
		Name:    "inspect",
		Output:  os.Stderr,
	}
}

// InspectCmd is the handle returned by InspectCommand. Fields may be
// customised between construction and Run; zero values are fine.
type InspectCmd struct {
	// Factory is the replay.Factory wired into inspect.WithReplayer.
	// Nil disables replay (the UI is read-only).
	Factory replay.Factory

	// Name is the program name used in flag error messages and the
	// usage string. Defaults to "inspect".
	Name string

	// Output is where logs and flag errors are written. Defaults to
	// os.Stderr.
	Output io.Writer
}

// Run parses args, opens the log read-only, starts the inspector
// server, and blocks until the process receives SIGINT/SIGTERM or the
// server crashes. Blocking matches the expectation of a CLI subcommand;
// callers that need more control should use inspect.New directly.
//
// args is the subcommand-level argument slice (e.g., os.Args[2:] after
// a "inspect" dispatch). It supports a minimal flag set — the defaults
// match cmd/starling-inspect, so the user experience is identical
// whether they run the standalone binary or their own dual-mode tool.
func (c *InspectCmd) Run(args []string) error {
	if c.Name == "" {
		c.Name = "inspect"
	}
	if c.Output == nil {
		c.Output = os.Stderr
	}

	fs := flag.NewFlagSet(c.Name, flag.ContinueOnError)
	fs.SetOutput(c.Output)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: %s [flags] <db>\n\n", c.Name)
		if c.Factory != nil {
			fmt.Fprintln(fs.Output(), "Local web inspector for Starling SQLite event logs.")
			fmt.Fprintln(fs.Output(), "Replay-from-UI is enabled.")
		} else {
			fmt.Fprintln(fs.Output(), "Read-only web inspector for Starling SQLite event logs.")
		}
		fmt.Fprintln(fs.Output(), "")
		fmt.Fprintln(fs.Output(), "Flags:")
		fs.PrintDefaults()
	}
	addr := fs.String("addr", "127.0.0.1:0",
		"bind address (host:port); port 0 picks a free port. Default binds loopback only.")
	noOpen := fs.Bool("no-open", false, "do not auto-open the browser")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("missing <db> argument")
	}
	dbPath := fs.Arg(0)

	// Always read-only: a user's agent binary might have write access
	// to the same db in another mode, but the inspector path must not.
	// This is the single line that keeps the audit property intact.
	store, err := eventlog.NewSQLite(dbPath, eventlog.WithReadOnly())
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	defer store.Close()

	opts := []inspect.Option{}
	if c.Factory != nil {
		opts = append(opts, inspect.WithReplayer(c.Factory))
	}
	srv, err := inspect.New(store, opts...)
	if err != nil {
		return fmt.Errorf("inspect: %w", err)
	}

	// Bind first so we know the resolved port before logging.
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", *addr, err)
	}
	url := browserURL(listener.Addr())
	logger := log.New(c.Output, "", log.LstdFlags)
	logger.Printf("starling %s listening on %s", c.Name, url)
	logger.Printf("opened %s read-only", dbPath)
	if c.Factory != nil {
		logger.Printf("replay-from-UI enabled")
	}

	httpSrv := &http.Server{
		Handler:           srv,
		ReadHeaderTimeout: 5 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(listener) }()

	if !*noOpen {
		go openBrowser(url, logger)
	}

	// Block on signal or server crash.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case s := <-sig:
		logger.Printf("received %s, shutting down", s)
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve: %w", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

// browserURL normalises wildcard bind addresses ("", "0.0.0.0", "::")
// into "localhost" so the URL is openable. Non-wildcard hosts the user
// explicitly asked for are preserved.
func browserURL(a net.Addr) string {
	host, port, err := net.SplitHostPort(a.String())
	if err != nil {
		return "http://" + a.String()
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// openBrowser opens url using the platform-appropriate command. Failure
// is logged but not fatal — the user can always click the logged URL.
func openBrowser(url string, logger *log.Logger) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", url)
	default: // linux, freebsd, ...
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		logger.Printf("could not open browser (%v); visit %s manually", err, url)
	}
}
