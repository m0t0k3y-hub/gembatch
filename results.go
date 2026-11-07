package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// resultItem is the normalized form of one result line.
type resultItem struct {
	Key          string `json:"key"`
	Text         string `json:"text"`
	FinishReason string `json:"finish_reason,omitempty"`
	Error        string `json:"error,omitempty"`
}

func parseResultLine(raw []byte) (resultItem, error) {
	var l struct {
		Key      string `json:"key"`
		Metadata struct {
			Key string `json:"key"`
		} `json:"metadata"`
		Response json.RawMessage `json:"response"`
		Error    json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &l); err != nil {
		return resultItem{}, err
	}
	it := resultItem{Key: firstNonEmpty(l.Key, l.Metadata.Key)}
	if len(l.Error) > 0 && string(l.Error) != "null" {
		it.Error = errorText(l.Error)
		return it, nil
	}
	if len(l.Response) == 0 || string(l.Response) == "null" {
		it.Error = "no response in result line"
		return it, nil
	}
	var r struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text    string `json:"text"`
					Thought bool   `json:"thought"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		PromptFeedback struct {
			BlockReason string `json:"blockReason"`
		} `json:"promptFeedback"`
	}
	if err := json.Unmarshal(l.Response, &r); err != nil {
		it.Error = "unreadable response: " + err.Error()
		return it, nil
	}
	if len(r.Candidates) == 0 {
		if r.PromptFeedback.BlockReason != "" {
			it.Error = "blocked: " + r.PromptFeedback.BlockReason
		} else {
			it.Error = "response has no candidates"
		}
		return it, nil
	}
	c := r.Candidates[0]
	var sb strings.Builder
	for _, p := range c.Content.Parts {
		if !p.Thought {
			sb.WriteString(p.Text)
		}
	}
	it.Text = sb.String()
	if c.FinishReason != "" && c.FinishReason != "STOP" {
		it.FinishReason = c.FinishReason
	}
	return it, nil
}

func errorText(raw json.RawMessage) string {
	var e struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Message != "" {
		if e.Code != 0 {
			return fmt.Sprintf("%d: %s", e.Code, e.Message)
		}
		return e.Message
	}
	return strings.Trim(string(raw), `"`)
}

// resultOpts describes how results are written; shared by `results` and `run`.
type resultOpts struct {
	out, outDir, format         string
	raw, failedOnly, failedKeys bool
}

func bindOutputFlags(fs *flag.FlagSet, ro *resultOpts) {
	fs.StringVar(&ro.out, "out", "", "write results to this file instead of stdout")
	fs.StringVar(&ro.outDir, "out-dir", "", "write one <key>.txt per successful result into this directory")
	fs.StringVar(&ro.format, "format", "jsonl", "stdout/--out format: jsonl | csv | text")
	fs.BoolVar(&ro.raw, "raw", false, "emit the API's original result lines unchanged")
	fs.BoolVar(&ro.failedOnly, "failed-only", false, "emit only failed items")
	fs.BoolVar(&ro.failedKeys, "failed-keys", false, "print only the keys of failed items, one per line (feed to --only-keys)")
}

func (ro *resultOpts) validate() error {
	if ro.format != "jsonl" && ro.format != "csv" && ro.format != "text" {
		return usageErr("--format must be jsonl, csv or text")
	}
	return nil
}

// writeResults normalizes and writes raw result lines. It returns how many items
// succeeded and failed.
func (a *app) writeResults(lines [][]byte, ro *resultOpts) (ok, bad int, err error) {
	w := a.stdout
	if ro.out != "" {
		f, err := os.Create(ro.out)
		if err != nil {
			return 0, 0, err
		}
		defer f.Close()
		w = f
	}
	if ro.outDir != "" {
		if err := os.MkdirAll(ro.outDir, 0o755); err != nil {
			return 0, 0, err
		}
	}

	var cw *csv.Writer
	if ro.format == "csv" && !ro.raw && !ro.failedKeys {
		cw = csv.NewWriter(w)
		cw.Write([]string{"key", "text", "error"})
	}

	written := 0
	for _, line := range lines {
		it, perr := parseResultLine(line)
		if perr != nil {
			it = resultItem{Error: "unparseable result line: " + perr.Error()}
		}
		failed := it.Error != ""
		if failed {
			bad++
		} else {
			ok++
		}
		if ro.failedOnly && !failed {
			continue
		}
		if ro.outDir != "" && !failed {
			p := filepath.Join(ro.outDir, filepath.Base(it.Key)+".txt")
			if err := os.WriteFile(p, []byte(it.Text), 0o644); err != nil {
				return ok, bad, err
			}
			written++
			continue
		}
		switch {
		case ro.failedKeys:
			if failed {
				fmt.Fprintln(w, it.Key)
			}
		case ro.outDir != "":
			// failures with --out-dir are counted and reported on stderr
		case ro.raw:
			w.Write(append(append([]byte(nil), line...), '\n'))
		case cw != nil:
			cw.Write([]string{it.Key, it.Text, it.Error})
		case ro.format == "text":
			fmt.Fprintf(w, "### %s\n", it.Key)
			if failed {
				fmt.Fprintf(w, "[error] %s\n\n", it.Error)
			} else {
				fmt.Fprintf(w, "%s\n\n", it.Text)
			}
		default:
			b, _ := json.Marshal(it)
			w.Write(append(b, '\n'))
		}
	}
	if cw != nil {
		cw.Flush()
	}
	if ro.outDir != "" {
		if a.jsonOut {
			a.emit(map[string]any{"succeeded": ok, "failed": bad, "written": written, "dir": ro.outDir})
		} else {
			a.printf("wrote %d file(s) to %s\n", written, ro.outDir)
		}
	}
	return ok, bad, nil
}

func cmdResults(a *app, fs *flag.FlagSet) func(context.Context, []string) error {
	var ro resultOpts
	bindOutputFlags(fs, &ro)

	return func(ctx context.Context, pos []string) error {
		name, err := oneJobArg(pos)
		if err != nil {
			return err
		}
		if err := ro.validate(); err != nil {
			return err
		}
		c, err := a.client()
		if err != nil {
			return err
		}
		job, err := c.GetJob(ctx, name)
		if err != nil {
			return err
		}
		if !job.HasResults() {
			return &exitError{exitJobNotOK, fmt.Errorf("%s is %s, results are not available", job.Name, job.ShortState())}
		}

		var lines [][]byte
		if file := job.ResultsFile(); file != "" {
			body, err := c.Download(ctx, file)
			if err != nil {
				return err
			}
			defer body.Close()
			r := bufio.NewReaderSize(body, 1<<20)
			for {
				l, err := r.ReadBytes('\n')
				if t := bytes.TrimSpace(l); len(t) > 0 {
					lines = append(lines, append([]byte(nil), t...))
				}
				if err == io.EOF {
					break
				}
				if err != nil {
					return err
				}
			}
		} else if inl := job.InlinedResponses(); len(inl) > 0 {
			for _, l := range inl {
				lines = append(lines, []byte(l))
			}
		} else {
			return fmt.Errorf("%s succeeded but has no results file or inline responses", job.Name)
		}

		ok, bad, err := a.writeResults(lines, &ro)
		if err != nil {
			return err
		}
		a.logf("%d succeeded, %d failed (of %d)\n", ok, bad, len(lines))
		if bad > 0 && !ro.failedKeys && !ro.failedOnly {
			a.logf("tip: gembatch results %s --failed-keys > failed.txt  then  gembatch create ... --only-keys failed.txt\n", job.Name)
		}
		return nil
	}
}
