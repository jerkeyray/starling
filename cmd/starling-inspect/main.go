// Command starling-inspect is a local web inspector for Starling
// event logs. It opens a SQLite event log read-only, serves a self-
// contained web UI on localhost, and (by default) opens the user's
// browser pointed at it.
//
// Read-only by construction: the binary opens its SQLite database
// with eventlog.WithReadOnly() and never imports any code path that
// could call EventLog.Append. Inspector cannot mutate audit logs.
//
// Usage:
//
//	starling-inspect runs.db                # localhost, free port, opens browser
//	starling-inspect --addr=:8080 runs.db   # bind explicit port
//	starling-inspect --no-open  runs.db     # don't open browser (SSH / headless)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "starling-inspect:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("starling-inspect", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: starling-inspect [flags] <db>")
		fmt.Fprintln(fs.Output(), "")
		fmt.Fprintln(fs.Output(), "Read-only web inspector for Starling SQLite event logs.")
		fmt.Fprintln(fs.Output(), "")
		fmt.Fprintln(fs.Output(), "Flags:")
		fs.PrintDefaults()
	}
	addr := fs.String("addr", "127.0.0.1:0", "bind address (host:port); port 0 picks a free port. Default binds loopback only.")
	noOpen := fs.Bool("no-open", false, "do not auto-open the browser")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("missing <db> argument")
	}
	dbPath := fs.Arg(0)

	store, err := eventlog.NewSQLite(dbPath, eventlog.WithReadOnly())
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	defer store.Close()

	srv, err := inspect.New(store)
	if err != nil {
		return fmt.Errorf("inspect: %w", err)
	}

	// Bind first so we know the resolved port before logging the URL.
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", *addr, err)
	}
	url := browserURL(listener.Addr())
	log.Printf("starling-inspect listening on %s", url)
	log.Printf("opened %s read-only", dbPath)

	// Serve in a goroutine so main can wait on shutdown signals.
	httpSrv := &http.Server{
		Handler:           srv,
		ReadHeaderTimeout: 5 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(listener) }()

	if !*noOpen {
		go openBrowser(url)
	}

	// Block on either a signal or the server crashing.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case s := <-sig:
		log.Printf("received %s, shutting down", s)
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

// browserURL returns an http URL safe to log and to feed to a
// browser. net.Listen on ":0", ":8080", or "[::]:0" resolves to
// addresses like "[::]:43127" which no browser can open — those
// hostnames mean "every interface" to the kernel, not "this host".
// Normalize them to localhost (which works for both IPv4 and IPv6
// loopback in every modern browser) while preserving non-wildcard
// hosts the user explicitly asked for.
func browserURL(a net.Addr) string {
	host, port, err := net.SplitHostPort(a.String())
	if err != nil {
		// Unparseable; best-effort fall back to the raw form.
		return "http://" + a.String()
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// openBrowser tries to open url in the user's default browser using
// the platform-appropriate command. Failure is logged, not fatal —
// the user can always click the URL printed at startup.
func openBrowser(url string) {
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
		log.Printf("could not open browser (%v); visit %s manually", err, url)
	}
}
