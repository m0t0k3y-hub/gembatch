package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultModel  = "gemini-3.5-flash"
	maxInputBytes = 2 << 30 // Batch API input file limit (2 GB)
)

// reqLine is one line of the batch input JSONL.
type reqLine struct {
	Key     string          `json:"key"`
	Request json.RawMessage `json:"request"`
}

type createOpts struct {
	model, name                  string
	input, prompts, dir          string
	glob, prompt, system, config string
	onlyKeys, saveRequests       string
	limit, concurrency           int
	dryRun                       bool
}

var mediaTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".webp": "image/webp",
	".heic": "image/heic", ".heif": "image/heif", ".gif": "image/gif",
	".pdf": "application/pdf", ".txt": "text/plain", ".md": "text/plain", ".csv": "text/csv",
	".mp3": "audio/mp3", ".wav": "audio/wav", ".flac": "audio/flac", ".m4a": "audio/mp4",
	".mp4": "video/mp4", ".mov": "video/quicktime", ".webm": "video/webm",
}

func mimeFor(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if m, ok := mediaTypes[ext]; ok {
		return m
	}
	return strings.SplitN(mime.TypeByExtension(ext), ";", 2)[0]
}

// bindInputFlags registers the flags that describe *what to send*. They are shared by
// `create` (batch) and `run` (synchronous), so both accept identical inputs.
func bindInputFlags(a *app, fs *flag.FlagSet, o *createOpts) {
	env := func(k, def string) string {
		if v := a.getenv(k); v != "" {
			return v
		}
		return def
	}
	fs.StringVar(&o.model, "model", env("GEMBATCH_MODEL", defaultModel), "model to run (also GEMBATCH_MODEL)")
	fs.StringVar(&o.input, "input", "", "ready-made requests JSONL: one {\"key\":...,\"request\":{GenerateContentRequest}} per line")
	fs.StringVar(&o.prompts, "prompts", "", "text file with one prompt per line (keys: line-00001, ...)")
	fs.StringVar(&o.dir, "dir", "", "directory of images/PDFs/etc; one request per file (key = file name). Requires --prompt")
	fs.StringVar(&o.glob, "glob", "*", "with --dir: only files whose name matches this pattern, e.g. '*.png'")
	fs.StringVar(&o.prompt, "prompt", "", "with --dir: the instruction sent together with every file")
	fs.StringVar(&o.system, "system", "", "system instruction added to every request (--prompts / --dir)")
	fs.StringVar(&o.config, "config", "", "generationConfig as JSON for every request (--prompts / --dir), e.g. '{\"temperature\":0}'")
	fs.StringVar(&o.onlyKeys, "only-keys", "", "file with one key per line; use only these (retry failures)")
	fs.IntVar(&o.limit, "limit", 0, "use only the first N requests (cheap trial run; 0 = all)")
	fs.StringVar(&o.saveRequests, "save-requests", "", "also write the generated requests JSONL to this path")
	fs.BoolVar(&o.dryRun, "dry-run", false, "build the requests but send nothing (no API key needed)")
	fs.IntVar(&o.concurrency, "concurrency", 4, "parallel file uploads (--dir) and, for `run`, parallel API calls")
}

// prepare validates the input flags and builds the request lines.
func (o *createOpts) prepare(ctx context.Context, a *app) ([]reqLine, error) {
	modes := 0
	for _, s := range []string{o.input, o.prompts, o.dir} {
		if s != "" {
			modes++
		}
	}
	if modes != 1 {
		return nil, usageErr("specify exactly one of --input, --prompts, --dir")
	}
	if o.dir != "" && o.prompt == "" {
		return nil, usageErr("--dir requires --prompt")
	}
	if o.limit < 0 {
		return nil, usageErr("--limit must be >= 0")
	}
	var gen map[string]any
	if o.config != "" {
		if err := json.Unmarshal([]byte(o.config), &gen); err != nil {
			return nil, usageErr("--config is not valid JSON object: %v", err)
		}
	}
	keep, err := loadKeyFilter(o.onlyKeys)
	if err != nil {
		return nil, err
	}
	if o.concurrency < 1 {
		o.concurrency = 1
	}

	var lines []reqLine
	switch {
	case o.input != "":
		lines, err = linesFromInput(o.input, keep)
	case o.prompts != "":
		lines, err = linesFromPrompts(o.prompts, o.system, gen, keep)
	default:
		var c *Client
		if !o.dryRun { // only --dir needs the API (file uploads)
			if c, err = a.client(); err != nil {
				return nil, err
			}
		}
		lines, err = a.linesFromDir(ctx, c, o, gen, keep)
	}
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return nil, errors.New("no requests to send (empty input, or --only-keys matched nothing)")
	}
	if o.limit > 0 && len(lines) > o.limit {
		lines = lines[:o.limit]
	}
	return lines, nil
}

func encodeLines(lines []reqLine) (*bytes.Buffer, error) {
	var buf bytes.Buffer
	for _, l := range lines {
		b, err := json.Marshal(l)
		if err != nil {
			return nil, err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	return &buf, nil
}

// reportDryRun prints what would be sent and honours --save-requests.
func (a *app) reportDryRun(o *createOpts, lines []reqLine, size int) {
	keys := make([]string, 0, 5)
	for i := 0; i < len(lines) && i < 5; i++ {
		keys = append(keys, lines[i].Key)
	}
	if a.jsonOut {
		a.emit(map[string]any{"dry_run": true, "model": o.model, "requests": len(lines), "bytes": size, "first_keys": keys, "save_requests": o.saveRequests})
		return
	}
	a.printf("dry-run: %d requests, %d bytes, model %s\n", len(lines), size, o.model)
	a.printf("first keys: %s\n", strings.Join(keys, ", "))
	if o.saveRequests != "" {
		a.printf("requests written to %s\n", o.saveRequests)
	}
}

func cmdCreate(a *app, fs *flag.FlagSet) func(context.Context, []string) error {
	var o createOpts
	bindInputFlags(a, fs, &o)
	fs.StringVar(&o.name, "name", "", "display name for the job (default: gembatch-<timestamp>)")

	return func(ctx context.Context, pos []string) error {
		if len(pos) > 0 {
			return usageErr("create takes no positional arguments (got %q)", pos[0])
		}
		lines, err := o.prepare(ctx, a)
		if err != nil {
			return err
		}
		buf, err := encodeLines(lines)
		if err != nil {
			return err
		}
		if buf.Len() > maxInputBytes {
			return fmt.Errorf("input is %d bytes; the Batch API limit is 2 GB. Split it into several jobs", buf.Len())
		}
		if o.saveRequests != "" {
			if err := os.WriteFile(o.saveRequests, buf.Bytes(), 0o644); err != nil {
				return err
			}
		}
		if o.dryRun {
			a.reportDryRun(&o, lines, buf.Len())
			return nil
		}

		c, err := a.client()
		if err != nil {
			return err
		}
		display := o.name
		if display == "" {
			display = "gembatch-" + time.Now().Format("20060102-150405")
		}
		fi, err := c.UploadReader(ctx, bytes.NewReader(buf.Bytes()), int64(buf.Len()), "jsonl", display+".jsonl")
		if err != nil {
			return err
		}
		job, err := c.CreateBatch(ctx, o.model, display, fi.Name)
		if err != nil {
			return err
		}
		if a.jsonOut {
			a.emit(map[string]any{"job": job.Summary(), "input_file": fi.Name, "requests": len(lines)})
		} else {
			a.printf("%s  %s  (%d requests, model %s)\n", job.Name, job.ShortState(), len(lines), o.model)
			a.printf("next: gembatch wait %s && gembatch results %s\n", job.Name, job.Name)
		}
		return nil
	}
}

func loadKeyFilter(path string) (func(string) bool, error) {
	if path == "" {
		return func(string) bool { return true }, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	set := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if k := strings.TrimSpace(sc.Text()); k != "" {
			set[k] = true
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return func(k string) bool { return set[k] }, nil
}

// readLines calls fn for every line without a length limit.
func readLines(path string, fn func(n int, line []byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	n := 0
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			n++
			if e := fn(n, bytes.TrimRight(line, "\r\n")); e != nil {
				return e
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func linesFromInput(path string, keep func(string) bool) ([]reqLine, error) {
	var out []reqLine
	seen := map[string]bool{}
	err := readLines(path, func(n int, line []byte) error {
		if len(bytes.TrimSpace(line)) == 0 {
			return nil
		}
		var l reqLine
		if err := json.Unmarshal(line, &l); err != nil {
			return fmt.Errorf("%s line %d: %w", path, n, err)
		}
		if l.Key == "" || len(l.Request) == 0 {
			return fmt.Errorf("%s line %d: each line needs a non-empty \"key\" and a \"request\"", path, n)
		}
		if seen[l.Key] {
			return fmt.Errorf("%s line %d: duplicate key %q", path, n, l.Key)
		}
		seen[l.Key] = true
		if keep(l.Key) {
			out = append(out, l)
		}
		return nil
	})
	return out, err
}

func makeRequest(parts []map[string]any, system string, gen map[string]any) json.RawMessage {
	r := map[string]any{"contents": []any{map[string]any{"role": "user", "parts": parts}}}
	if system != "" {
		r["systemInstruction"] = map[string]any{"parts": []any{map[string]any{"text": system}}}
	}
	if gen != nil {
		r["generationConfig"] = gen
	}
	b, _ := json.Marshal(r)
	return b
}

func linesFromPrompts(path, system string, gen map[string]any, keep func(string) bool) ([]reqLine, error) {
	var out []reqLine
	err := readLines(path, func(n int, line []byte) error {
		text := strings.TrimSpace(string(line))
		if text == "" {
			return nil
		}
		key := fmt.Sprintf("line-%05d", n)
		if keep(key) {
			out = append(out, reqLine{Key: key, Request: makeRequest([]map[string]any{{"text": text}}, system, gen)})
		}
		return nil
	})
	return out, err
}

func (a *app) linesFromDir(ctx context.Context, c *Client, o *createOpts, gen map[string]any, keep func(string) bool) ([]reqLine, error) {
	if _, err := filepath.Match(o.glob, "x"); err != nil {
		return nil, usageErr("bad --glob pattern: %v", err)
	}
	entries, err := os.ReadDir(o.dir)
	if err != nil {
		return nil, err
	}
	type item struct{ name, path, mime string }
	var items []item
	skipped := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if ok, _ := filepath.Match(o.glob, e.Name()); !ok || !keep(e.Name()) {
			continue
		}
		m := mimeFor(e.Name())
		if m == "" {
			skipped++
			continue
		}
		items = append(items, item{e.Name(), filepath.Join(o.dir, e.Name()), m})
	}
	if skipped > 0 {
		a.logf("gembatch: skipped %d file(s) with unsupported extension\n", skipped)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].name < items[j].name })
	if o.limit > 0 && len(items) > o.limit {
		items = items[:o.limit] // do not upload files we will not use
	}

	lines := make([]reqLine, len(items))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		sem      = make(chan struct{}, o.concurrency)
	)
	for i, it := range items {
		i, it := i, it
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			uri := "dry-run://" + it.name
			if c != nil {
				if ctx.Err() != nil {
					return
				}
				fi, err := c.UploadFile(ctx, it.path, it.mime)
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
						cancel()
					}
					mu.Unlock()
					return
				}
				uri = fi.URI
			}
			parts := []map[string]any{
				{"fileData": map[string]any{"mimeType": it.mime, "fileUri": uri}},
				{"text": o.prompt},
			}
			lines[i] = reqLine{Key: it.name, Request: makeRequest(parts, o.system, gen)}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return lines, nil
}

func writeFile(path string, b []byte) error { return os.WriteFile(path, b, 0o644) }
