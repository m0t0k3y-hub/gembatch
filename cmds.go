package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

type command struct {
	name    string
	summary string
	args    string
	build   func(a *app, fs *flag.FlagSet) func(context.Context, []string) error
}

var commands []command

func init() {
	commands = []command{
		{"run", "send the same inputs through the regular (synchronous) API right now", "", cmdRun},
		{"create", "upload requests and start a batch job (50% cost, async)", "", cmdCreate},
		{"list", "list recent batch jobs", "", cmdList},
		{"status", "show one job's state and counts", "<job-id>", cmdStatus},
		{"wait", "poll until the job finishes (exit 0 only if SUCCEEDED)", "<job-id>", cmdWait},
		{"results", "download and normalize a finished job's results", "<job-id>", cmdResults},
		{"cancel", "cancel a pending/running job", "<job-id>", cmdCancel},
		{"delete", "delete a job record", "<job-id>", cmdDelete},
		{"usage", "print the full command/flag reference (--json for machines)", "", cmdUsage},
	}
}

func findCommand(name string) *command {
	for i := range commands {
		if commands[i].name == name {
			return &commands[i]
		}
	}
	return nil
}

func oneJobArg(pos []string) (string, error) {
	if len(pos) != 1 {
		return "", usageErr("expected exactly one <job-id> (e.g. batches/abc123 or abc123)")
	}
	return jobID(pos[0]), nil
}

func (a *app) printJob(j *Job) {
	if a.jsonOut {
		a.emit(j.Summary())
		return
	}
	s := j.Summary()
	line := fmt.Sprintf("%s  %s", s.Name, s.State)
	if s.Requests != nil && *s.Requests > 0 {
		line += fmt.Sprintf("  requests=%d succeeded=%d failed=%d pending=%d", *s.Requests, *s.Succeeded, *s.Failed, *s.Pending)
	}
	if s.ResultsFile != "" {
		line += "  results=" + s.ResultsFile
	}
	if s.Error != "" {
		line += "  error=" + s.Error
	}
	a.printf("%s\n", line)
}

func cmdStatus(a *app, fs *flag.FlagSet) func(context.Context, []string) error {
	raw := fs.Bool("raw", false, "print the API's original JSON response")
	return func(ctx context.Context, pos []string) error {
		name, err := oneJobArg(pos)
		if err != nil {
			return err
		}
		c, err := a.client()
		if err != nil {
			return err
		}
		j, err := c.GetJob(ctx, name)
		if err != nil {
			return err
		}
		if *raw {
			a.printf("%s\n", j.Raw)
			return nil
		}
		a.printJob(j)
		return nil
	}
}

func cmdList(a *app, fs *flag.FlagSet) func(context.Context, []string) error {
	limit := fs.Int("limit", 20, "maximum number of jobs to show")
	return func(ctx context.Context, pos []string) error {
		if len(pos) > 0 {
			return usageErr("list takes no positional arguments")
		}
		c, err := a.client()
		if err != nil {
			return err
		}
		jobs, err := c.ListJobs(ctx, *limit)
		if err != nil {
			return err
		}
		if a.jsonOut {
			out := make([]Summary, 0, len(jobs))
			for _, j := range jobs {
				out = append(out, j.Summary())
			}
			a.emit(out)
			return nil
		}
		tw := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tSTATE\tDISPLAY NAME\tCREATED")
		for _, j := range jobs {
			s := j.Summary()
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", s.Name, s.State, s.DisplayName, s.CreateTime)
		}
		return tw.Flush()
	}
}

func cmdWait(a *app, fs *flag.FlagSet) func(context.Context, []string) error {
	interval := fs.Duration("interval", 30*time.Second, "polling interval")
	timeout := fs.Duration("timeout", 0, "give up after this long (0 = wait forever). Exit code 4 on timeout")
	quiet := fs.Bool("quiet", false, "do not print progress to stderr")
	return func(ctx context.Context, pos []string) error {
		name, err := oneJobArg(pos)
		if err != nil {
			return err
		}
		c, err := a.client()
		if err != nil {
			return err
		}
		if *timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, *timeout)
			defer cancel()
		}
		timedOut := func(err error) error {
			if *timeout > 0 && errors.Is(err, context.DeadlineExceeded) {
				return &exitError{exitTimeout, fmt.Errorf("timeout after %s; %s may still be running, run wait again", *timeout, name)}
			}
			return err
		}
		for {
			j, err := c.GetJob(ctx, name)
			if err != nil {
				return timedOut(err)
			}
			if j.Terminal() {
				a.printJob(j)
				if j.HasResults() {
					return nil
				}
				return &exitError{exitJobNotOK, fmt.Errorf("%s ended as %s", j.Name, j.ShortState())}
			}
			if !*quiet {
				s := j.Summary()
				msg := fmt.Sprintf("%s %s %s", time.Now().Format("15:04:05"), s.Name, s.State)
				if s.Requests != nil && *s.Requests > 0 {
					msg += fmt.Sprintf(" (%d/%d done)", *s.Succeeded+*s.Failed, *s.Requests)
				}
				a.logf("%s\n", msg)
			}
			if err := a.sleep(ctx, *interval); err != nil {
				return timedOut(err)
			}
		}
	}
}

func cmdCancel(a *app, fs *flag.FlagSet) func(context.Context, []string) error {
	return func(ctx context.Context, pos []string) error {
		name, err := oneJobArg(pos)
		if err != nil {
			return err
		}
		c, err := a.client()
		if err != nil {
			return err
		}
		if err := c.CancelJob(ctx, name); err != nil {
			return err
		}
		if a.jsonOut {
			a.emit(map[string]any{"name": name, "cancel_requested": true})
		} else {
			a.printf("cancel requested for %s\n", name)
		}
		return nil
	}
}

func cmdDelete(a *app, fs *flag.FlagSet) func(context.Context, []string) error {
	return func(ctx context.Context, pos []string) error {
		name, err := oneJobArg(pos)
		if err != nil {
			return err
		}
		c, err := a.client()
		if err != nil {
			return err
		}
		if err := c.doJSON(ctx, http.MethodDelete, "/v1beta/"+name, nil, nil); err != nil {
			return err
		}
		if a.jsonOut {
			a.emit(map[string]any{"name": name, "deleted": true})
		} else {
			a.printf("deleted %s\n", name)
		}
		return nil
	}
}

// ---- help / usage ----

const envHelp = `Environment:
  GEMINI_API_KEY        API key (required except for create --dry-run)
  GEMBATCH_MODEL        default model for create/run (default ` + defaultModel + `)
  GEMBATCH_JSON=1       default to --json output
  GEMINI_API_BASE_URL   override API endpoint (testing)

Exit codes: 0 ok | 1 error | 2 bad usage | 3 job not SUCCEEDED, or some run requests failed | 4 wait timed out`

func (a *app) printHelp(w io.Writer) {
	fmt.Fprintln(w, "gembatch - Gemini API from the command line: batch (50% cost, async) and regular (synchronous)")
	fmt.Fprintln(w, "\nUsage: gembatch <command> [flags]\n\nCommands:")
	for _, c := range commands {
		fmt.Fprintf(w, "  %-8s %s\n", c.name, c.summary)
	}
	fmt.Fprintln(w, "\nTry a few items now with the regular API, then submit everything as a batch:")
	fmt.Fprintln(w, "  gembatch run    --dir scans/ --prompt 'Transcribe this ledger page as JSON' --limit 3")
	fmt.Fprintln(w, "\nBatch flow:")
	fmt.Fprintln(w, "  gembatch create --dir scans/ --glob '*.png' --prompt 'Transcribe this ledger page as JSON' --name ledger")
	fmt.Fprintln(w, "  gembatch wait <job-id> --interval 60s")
	fmt.Fprintln(w, "  gembatch results <job-id> --out-dir out/")
	fmt.Fprintln(w, "\nRun 'gembatch <command> -h' for flags, 'gembatch usage' for the full reference.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, envHelp)
}

type flagSpec struct {
	Name    string `json:"name"`
	Default string `json:"default,omitempty"`
	Usage   string `json:"usage"`
}

type commandSpec struct {
	Name    string     `json:"name"`
	Summary string     `json:"summary"`
	Args    string     `json:"args,omitempty"`
	Flags   []flagSpec `json:"flags"`
}

func specFor(c *command) commandSpec {
	tmp := &app{stdout: io.Discard, stderr: io.Discard, getenv: func(string) string { return "" }}
	fs := flag.NewFlagSet(c.name, flag.ContinueOnError)
	fs.BoolVar(&tmp.jsonOut, "json", false, "machine-readable JSON output (also GEMBATCH_JSON=1)")
	c.build(tmp, fs)
	spec := commandSpec{Name: c.name, Summary: c.summary, Args: c.args}
	fs.VisitAll(func(f *flag.Flag) {
		spec.Flags = append(spec.Flags, flagSpec{Name: f.Name, Default: f.DefValue, Usage: f.Usage})
	})
	sort.Slice(spec.Flags, func(i, j int) bool { return spec.Flags[i].Name < spec.Flags[j].Name })
	return spec
}

func (a *app) commandHelp(w io.Writer, c *command, fs *flag.FlagSet) {
	fmt.Fprintf(w, "Usage: gembatch %s %s[flags]\n\n%s\n\nFlags:\n", c.name, strings.TrimSpace(c.args+" "), c.summary)
	fs.SetOutput(w)
	fs.PrintDefaults()
}

func cmdUsage(a *app, fs *flag.FlagSet) func(context.Context, []string) error {
	return func(ctx context.Context, pos []string) error {
		var specs []commandSpec
		for i := range commands {
			specs = append(specs, specFor(&commands[i]))
		}
		if a.jsonOut {
			a.emit(map[string]any{
				"name":     "gembatch",
				"version":  version,
				"commands": specs,
				"env": map[string]string{
					"GEMINI_API_KEY":      "API key",
					"GEMBATCH_MODEL":      "default model",
					"GEMBATCH_JSON":       "1 = default to --json",
					"GEMINI_API_BASE_URL": "override endpoint",
				},
				"exit_codes": map[string]string{"0": "ok", "1": "error", "2": "bad usage", "3": "job not SUCCEEDED, or some run requests failed", "4": "wait timeout"},
			})
			return nil
		}
		a.printHelp(a.stdout)
		for _, s := range specs {
			a.printf("\n%s %s\n  %s\n", s.Name, s.Args, s.Summary)
			for _, f := range s.Flags {
				def := ""
				if f.Default != "" && f.Default != "false" {
					def = " (default " + f.Default + ")"
				}
				a.printf("    --%-12s %s%s\n", f.Name, f.Usage, def)
			}
		}
		return nil
	}
}
