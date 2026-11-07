// gembatch is a small CLI for the Gemini Batch API (50% cost, async, 24h target).
//
// It is dependency-free (standard library only) so it builds into a single binary.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"
)

var version = "dev"

// Exit codes. Agents can branch on these without parsing output.
const (
	exitOK       = 0
	exitErr      = 1 // runtime / API / network error
	exitUsage    = 2 // bad flags or arguments
	exitJobNotOK = 3 // job reached a terminal state other than SUCCEEDED
	exitTimeout  = 4 // `wait --timeout` elapsed while the job was still running
)

type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string { return e.err.Error() }
func (e *exitError) Unwrap() error { return e.err }

func usageErr(format string, a ...any) error {
	return &exitError{exitUsage, fmt.Errorf(format, a...)}
}

// app carries everything a command needs, so tests can inject fakes.
type app struct {
	stdout, stderr io.Writer
	getenv         func(string) string
	httpc          *http.Client
	sleep          func(context.Context, time.Duration) error
	jsonOut        bool
}

func realSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	a := &app{
		stdout: os.Stdout,
		stderr: os.Stderr,
		getenv: os.Getenv,
		httpc:  &http.Client{},
		sleep:  realSleep,
	}
	os.Exit(a.run(ctx, os.Args[1:]))
}

func (a *app) client() (*Client, error) {
	key := a.getenv("GEMINI_API_KEY")
	if key == "" {
		key = a.getenv("GOOGLE_API_KEY")
	}
	if key == "" {
		return nil, errors.New("GEMINI_API_KEY is not set (create a key in Google AI Studio and export it)")
	}
	base := a.getenv("GEMINI_API_BASE_URL")
	if base == "" {
		base = defaultBaseURL
	}
	return &Client{
		BaseURL:   strings.TrimRight(base, "/"),
		APIKey:    key,
		HTTP:      a.httpc,
		Sleep:     a.sleep,
		UserAgent: "gembatch/" + version,
	}, nil
}

func (a *app) emit(v any) {
	enc := json.NewEncoder(a.stdout)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func (a *app) printf(format string, args ...any) { fmt.Fprintf(a.stdout, format, args...) }
func (a *app) logf(format string, args ...any)   { fmt.Fprintf(a.stderr, format, args...) }

func (a *app) run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		a.printHelp(a.stderr)
		return exitUsage
	}
	switch args[0] {
	case "-h", "--help", "help":
		a.printHelp(a.stdout)
		return exitOK
	case "version", "--version":
		a.printf("gembatch %s\n", version)
		return exitOK
	case "--usage":
		args = []string{"usage"}
	}

	cmd := findCommand(args[0])
	if cmd == nil {
		a.logf("gembatch: unknown command %q\n\n", args[0])
		a.printHelp(a.stderr)
		return exitUsage
	}

	fs := flag.NewFlagSet("gembatch "+cmd.name, flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	fs.BoolVar(&a.jsonOut, "json", a.getenv("GEMBATCH_JSON") == "1", "machine-readable JSON output (also GEMBATCH_JSON=1)")
	runFn := cmd.build(a, fs)
	fs.Usage = func() { a.commandHelp(a.stderr, cmd, fs) }

	pos, err := parseInterspersed(fs, args[1:])
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		return exitUsage
	}
	return a.report(runFn(ctx, pos))
}

func (a *app) report(err error) int {
	if err == nil {
		return exitOK
	}
	code := exitErr
	var ee *exitError
	if errors.As(err, &ee) {
		code = ee.code
	}
	if errors.Is(err, context.Canceled) {
		code = 130
	}
	if a.jsonOut {
		b, _ := json.Marshal(map[string]any{"error": err.Error(), "exit_code": code})
		fmt.Fprintln(a.stderr, string(b))
	} else {
		fmt.Fprintf(a.stderr, "gembatch: %v\n", err)
	}
	return code
}

// parseInterspersed lets flags appear before or after positional arguments
// (the standard flag package stops at the first positional).
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
