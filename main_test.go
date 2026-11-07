package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeAPI is a minimal stand-in for the Gemini batch + file endpoints.
type fakeAPI struct {
	t         *testing.T
	mu        sync.Mutex
	srv       *httptest.Server
	uploads   map[string]string // file name -> uploaded body
	nUpload   int32
	created   map[string]any
	getCalls  int32
	states    []string // states returned by successive GET /batches/job1
	results   string
	resultKey string // "responsesFile" or "dest"
	failGets  int32  // number of initial GETs that answer 503
	genCalls  int32
	retried   int
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{t: t, uploads: map[string]string{}, resultKey: "responsesFile"}
	mux := http.NewServeMux()
	mux.HandleFunc("/upload/v1beta/files", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Goog-Upload-Command") != "start" || r.Header.Get("x-goog-api-key") != "test-key" {
			http.Error(w, "bad start", 400)
			return
		}
		n := atomic.AddInt32(&f.nUpload, 1)
		w.Header().Set("X-Goog-Upload-Url", f.srv.URL+"/upload-session/"+string(rune('a'+n)))
		w.Header().Set("X-Goog-Upload-Status", "active")
	})
	mux.HandleFunc("/upload-session/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Goog-Upload-Command") != "upload, finalize" {
			http.Error(w, "bad finalize", 400)
			return
		}
		b, _ := io.ReadAll(r.Body)
		name := "files/f" + strings.TrimPrefix(r.URL.Path, "/upload-session/")
		f.mu.Lock()
		f.uploads[name] = string(b)
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"file": map[string]any{"name": name, "uri": f.srv.URL + "/v1beta/" + name, "mimeType": "x"}})
	})
	mux.HandleFunc("/v1beta/models/gemini-3.5-flash:batchGenerateContent", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&f.created)
		w.Write([]byte(`{"name":"batches/job1","metadata":{"state":"BATCH_STATE_PENDING","displayName":"x"}}`))
	})
	mux.HandleFunc("/v1beta/batches/job1", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&f.getCalls, 1)
		if n <= f.failGets {
			http.Error(w, `{"error":{"message":"overloaded","status":"UNAVAILABLE"}}`, 503)
			return
		}
		i := int(n - 1 - f.failGets)
		if i >= len(f.states) {
			i = len(f.states) - 1
		}
		st := f.states[i]
		if st != "JOB_STATE_SUCCEEDED" && st != "BATCH_STATE_SUCCEEDED" {
			w.Write([]byte(`{"name":"batches/job1","metadata":{"state":"` + st + `","batchStats":{"requestCount":"4","successfulRequestCount":"1","failedRequestCount":"0","pendingRequestCount":"3"}}}`))
			return
		}
		if f.resultKey == "dest" {
			w.Write([]byte(`{"name":"batches/job1","state":"` + st + `","dest":{"fileName":"files/res1"}}`))
		} else {
			w.Write([]byte(`{"name":"batches/job1","done":true,"metadata":{"state":"` + st + `"},"response":{"responsesFile":"files/res1"}}`))
		}
	})
	mux.HandleFunc("/v1beta/batches/inl", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"name":"batches/inl","done":true,"metadata":{"state":"BATCH_STATE_SUCCEEDED"},"response":{"inlinedResponses":{"inlinedResponses":[{"metadata":{"key":"q1"},"response":{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}}]}}}`))
	})
	mux.HandleFunc("/v1beta/batches/bad", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"name":"batches/bad","state":"JOB_STATE_FAILED","error":{"code":13,"message":"boom"}}`))
	})
	mux.HandleFunc("/v1beta/models/gemini-3.5-flash:generateContent", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body := string(b)
		atomic.AddInt32(&f.genCalls, 1)
		if strings.Contains(body, "FAILME") {
			http.Error(w, `{"error":{"code":400,"message":"bad prompt","status":"INVALID_ARGUMENT"}}`, 400)
			return
		}
		if strings.Contains(body, "RETRYME") {
			f.mu.Lock()
			f.retried++
			first := f.retried == 1
			f.mu.Unlock()
			if first {
				http.Error(w, `{"error":{"message":"slow down","status":"RESOURCE_EXHAUSTED"}}`, 429)
				return
			}
		}
		var req struct {
			Contents []struct {
				Parts []struct{ Text string } `json:"parts"`
			} `json:"contents"`
		}
		json.Unmarshal(b, &req)
		last := req.Contents[0].Parts[len(req.Contents[0].Parts)-1].Text
		json.NewEncoder(w).Encode(map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"parts": []any{map[string]any{"text": "echo:" + last}}}, "finishReason": "STOP"}}})
	})
	mux.HandleFunc("/v1beta/batches/job1:cancel", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "method", 405)
			return
		}
		w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/v1beta/batches", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"operations":[{"name":"batches/job1","metadata":{"state":"BATCH_STATE_RUNNING","displayName":"one"}},{"name":"batches/job2","state":"JOB_STATE_SUCCEEDED"}]}`))
	})
	mux.HandleFunc("/download/v1beta/files/res1:download", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("alt") != "media" {
			http.Error(w, "no alt", 400)
			return
		}
		io.WriteString(w, f.results)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

type harness struct {
	a        *app
	out, err *bytes.Buffer
}

func newHarness(f *fakeAPI) *harness {
	h := &harness{out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	env := map[string]string{"GEMINI_API_KEY": "test-key"}
	if f != nil {
		env["GEMINI_API_BASE_URL"] = f.srv.URL
	}
	h.a = &app{
		stdout: h.out, stderr: h.err,
		getenv: func(k string) string { return env[k] },
		httpc:  &http.Client{Timeout: 10 * time.Second},
		sleep:  func(context.Context, time.Duration) error { return nil },
	}
	return h
}

func (h *harness) run(args ...string) int {
	h.out.Reset()
	h.err.Reset()
	h.a.jsonOut = false
	return h.a.run(context.Background(), args)
}

const resultsJSONL = `{"key":"a.png","response":{"candidates":[{"content":{"parts":[{"text":"Hello "},{"text":"world"}]},"finishReason":"STOP"}]}}
{"key":"b.png","error":{"code":3,"message":"bad image"}}
{"key":"c.png","response":{"promptFeedback":{"blockReason":"SAFETY"}}}
`

func TestDryRunPrompts(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "p.txt")
	os.WriteFile(p, []byte("first\n\nsecond\n"), 0o644)
	out := filepath.Join(dir, "req.jsonl")
	h := newHarness(nil)
	h.a.getenv = func(string) string { return "" } // no API key needed
	if code := h.run("create", "--prompts", p, "--dry-run", "--save-requests", out, "--system", "be brief", "--config", `{"temperature":0}`); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err.String())
	}
	data, _ := os.ReadFile(out)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %s", len(lines), data)
	}
	var l struct {
		Key     string `json:"key"`
		Request struct {
			Contents []struct {
				Parts []struct{ Text string } `json:"parts"`
			} `json:"contents"`
			SystemInstruction struct{ Parts []struct{ Text string } } `json:"systemInstruction"`
			GenerationConfig  map[string]any                          `json:"generationConfig"`
		} `json:"request"`
	}
	json.Unmarshal([]byte(lines[1]), &l)
	if l.Key != "line-00003" { // blank line 2 is skipped but numbering follows the file
		t.Errorf("key = %q", l.Key)
	}
	if l.Request.Contents[0].Parts[0].Text != "second" || l.Request.SystemInstruction.Parts[0].Text != "be brief" || l.Request.GenerationConfig["temperature"] != float64(0) {
		t.Errorf("bad request: %s", lines[1])
	}
}

func TestCreateFromInputUploadsAndSubmits(t *testing.T) {
	f := newFakeAPI(t)
	h := newHarness(f)
	in := filepath.Join(t.TempDir(), "in.jsonl")
	os.WriteFile(in, []byte(`{"key":"q1","request":{"contents":[{"parts":[{"text":"hi"}]}]}}`+"\n"+`{"key":"q2","request":{"contents":[{"parts":[{"text":"yo"}]}]}}`+"\n"), 0o644)
	if code := h.run("create", "--input", in, "--name", "demo", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err.String())
	}
	var res struct {
		Job       Summary `json:"job"`
		InputFile string  `json:"input_file"`
		Requests  int     `json:"requests"`
	}
	if err := json.Unmarshal(h.out.Bytes(), &res); err != nil {
		t.Fatal(err, h.out.String())
	}
	if res.Job.Name != "batches/job1" || res.Job.State != "PENDING" || res.Requests != 2 {
		t.Errorf("unexpected: %+v", res)
	}
	if body := f.uploads[res.InputFile]; strings.Count(body, "\n") != 2 || !strings.Contains(body, `"key":"q2"`) {
		t.Errorf("uploaded body wrong: %q", body)
	}
	batch := f.created["batch"].(map[string]any)
	if batch["display_name"] != "demo" || batch["input_config"].(map[string]any)["file_name"] != res.InputFile {
		t.Errorf("create body wrong: %v", f.created)
	}
}

func TestOnlyKeysFilter(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.jsonl")
	os.WriteFile(in, []byte(`{"key":"q1","request":{"x":1}}`+"\n"+`{"key":"q2","request":{"x":2}}`+"\n"), 0o644)
	keys := filepath.Join(dir, "keys.txt")
	os.WriteFile(keys, []byte("q2\n"), 0o644)
	out := filepath.Join(dir, "o.jsonl")
	h := newHarness(nil)
	if code := h.run("create", "--input", in, "--only-keys", keys, "--dry-run", "--save-requests", out); code != 0 {
		t.Fatal(h.err.String())
	}
	b, _ := os.ReadFile(out)
	if strings.Contains(string(b), "q1") || !strings.Contains(string(b), "q2") {
		t.Errorf("filter failed: %s", b)
	}
}

func TestDuplicateKeysRejected(t *testing.T) {
	in := filepath.Join(t.TempDir(), "in.jsonl")
	os.WriteFile(in, []byte(`{"key":"q","request":{}}`+"\n"+`{"key":"q","request":{}}`+"\n"), 0o644)
	h := newHarness(nil)
	if code := h.run("create", "--input", in, "--dry-run"); code == 0 || !strings.Contains(h.err.String(), "duplicate key") {
		t.Errorf("code=%d err=%s", code, h.err.String())
	}
}

func TestDirModeUploadsMedia(t *testing.T) {
	f := newFakeAPI(t)
	h := newHarness(f)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "b.png"), []byte("PNGB"), 0o644)
	os.WriteFile(filepath.Join(dir, "a.png"), []byte("PNGA"), 0o644)
	os.WriteFile(filepath.Join(dir, "skip.xyz"), []byte("?"), 0o644)
	out := filepath.Join(t.TempDir(), "req.jsonl")
	if code := h.run("create", "--dir", dir, "--glob", "*.png", "--prompt", "transcribe", "--save-requests", out); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err.String())
	}
	b, _ := os.ReadFile(out)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"key":"a.png"`) || !strings.Contains(lines[0], `"mimeType":"image/png"`) || !strings.Contains(lines[0], `"text":"transcribe"`) || !strings.Contains(lines[0], `"fileUri":"`+f.srv.URL) {
		t.Errorf("bad lines: %s", b)
	}
	// 2 media files + 1 requests jsonl
	if n := atomic.LoadInt32(&f.nUpload); n != 3 {
		t.Errorf("uploads = %d, want 3", n)
	}
}

func TestWaitThenResults(t *testing.T) {
	for _, key := range []string{"responsesFile", "dest"} {
		t.Run(key, func(t *testing.T) {
			f := newFakeAPI(t)
			f.resultKey = key
			f.states = []string{"JOB_STATE_PENDING", "BATCH_STATE_RUNNING", "JOB_STATE_SUCCEEDED"}
			f.results = resultsJSONL
			h := newHarness(f)
			if code := h.run("wait", "job1", "--interval", "1s"); code != 0 {
				t.Fatalf("wait exit %d: %s", code, h.err.String())
			}
			if !strings.Contains(h.out.String(), "SUCCEEDED") || !strings.Contains(h.out.String(), "results=files/res1") {
				t.Errorf("wait output: %s", h.out.String())
			}
			if code := h.run("results", "batches/job1"); code != 0 {
				t.Fatalf("results exit %d: %s", code, h.err.String())
			}
			lines := strings.Split(strings.TrimSpace(h.out.String()), "\n")
			if len(lines) != 3 {
				t.Fatalf("lines: %v", lines)
			}
			var it resultItem
			json.Unmarshal([]byte(lines[0]), &it)
			if it.Key != "a.png" || it.Text != "Hello world" || it.Error != "" {
				t.Errorf("item0: %+v", it)
			}
			json.Unmarshal([]byte(lines[1]), &it)
			if it.Error != "3: bad image" {
				t.Errorf("item1: %+v", it)
			}
			json.Unmarshal([]byte(lines[2]), &it)
			if it.Error != "blocked: SAFETY" {
				t.Errorf("item2: %+v", it)
			}
			if !strings.Contains(h.err.String(), "1 succeeded, 2 failed") {
				t.Errorf("stderr: %s", h.err.String())
			}
		})
	}
}

func TestResultsFailedKeysAndOutDirAndCSV(t *testing.T) {
	f := newFakeAPI(t)
	f.states = []string{"JOB_STATE_SUCCEEDED"}
	f.results = resultsJSONL
	h := newHarness(f)
	if code := h.run("results", "job1", "--failed-keys"); code != 0 || h.out.String() != "b.png\nc.png\n" {
		t.Errorf("failed-keys: code=%d out=%q", code, h.out.String())
	}
	dir := filepath.Join(t.TempDir(), "out")
	if code := h.run("results", "job1", "--out-dir", dir); code != 0 {
		t.Fatal(h.err.String())
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.png.txt")); string(b) != "Hello world" {
		t.Errorf("out-dir file: %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "b.png.txt")); err == nil {
		t.Error("failed item must not produce a file")
	}
	if code := h.run("results", "job1", "--format", "csv"); code != 0 || !strings.HasPrefix(h.out.String(), "key,text,error\n") || !strings.Contains(h.out.String(), "a.png,Hello world,\n") {
		t.Errorf("csv: %q", h.out.String())
	}
	if code := h.run("results", "job1", "--raw"); code != 0 || !strings.Contains(h.out.String(), `"blockReason":"SAFETY"`) {
		t.Errorf("raw: %q", h.out.String())
	}
}

func TestInlinedResults(t *testing.T) {
	f := newFakeAPI(t)
	h := newHarness(f)
	if code := h.run("results", "inl"); code != 0 {
		t.Fatal(h.err.String())
	}
	if !strings.Contains(h.out.String(), `"key":"q1","text":"hi"`) {
		t.Errorf("inline: %s", h.out.String())
	}
}

func TestFailedJobExitCodes(t *testing.T) {
	f := newFakeAPI(t)
	h := newHarness(f)
	if code := h.run("wait", "bad"); code != exitJobNotOK {
		t.Errorf("wait exit = %d", code)
	}
	if code := h.run("results", "bad"); code != exitJobNotOK {
		t.Errorf("results exit = %d", code)
	}
}

func TestWaitTimeoutExitCode(t *testing.T) {
	f := newFakeAPI(t)
	f.states = []string{"JOB_STATE_RUNNING"}
	h := newHarness(f)
	h.a.sleep = func(ctx context.Context, d time.Duration) error {
		<-ctx.Done()
		return ctx.Err()
	}
	if code := h.run("wait", "job1", "--timeout", "50ms", "--quiet"); code != exitTimeout {
		t.Errorf("exit = %d, err=%s", code, h.err.String())
	}
}

func TestGetRetriesOn503(t *testing.T) {
	f := newFakeAPI(t)
	f.states = []string{"JOB_STATE_RUNNING"}
	f.failGets = 2
	h := newHarness(f)
	if code := h.run("status", "job1", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err.String())
	}
	var s Summary
	json.Unmarshal(h.out.Bytes(), &s)
	if s.State != "RUNNING" || s.Pending == nil || *s.Pending != 3 || *s.Requests != 4 {
		t.Errorf("summary: %+v", s)
	}
	if atomic.LoadInt32(&f.getCalls) != 3 {
		t.Errorf("calls = %d", f.getCalls)
	}
}

func TestListCancelUsage(t *testing.T) {
	f := newFakeAPI(t)
	h := newHarness(f)
	if code := h.run("list", "--json"); code != 0 {
		t.Fatal(h.err.String())
	}
	var arr []Summary
	if err := json.Unmarshal(h.out.Bytes(), &arr); err != nil || len(arr) != 2 || arr[0].State != "RUNNING" || arr[1].State != "SUCCEEDED" {
		t.Errorf("list: %v %s", err, h.out.String())
	}
	if code := h.run("cancel", "job1"); code != 0 || !strings.Contains(h.out.String(), "cancel requested") {
		t.Errorf("cancel: %d %s %s", code, h.out.String(), h.err.String())
	}
	if code := h.run("usage", "--json"); code != 0 {
		t.Fatal(h.err.String())
	}
	var u struct {
		Commands []commandSpec `json:"commands"`
	}
	if err := json.Unmarshal(h.out.Bytes(), &u); err != nil || len(u.Commands) != len(commands) {
		t.Fatalf("usage json: %v", err)
	}
	found := false
	for _, c := range u.Commands {
		if c.Name == "create" {
			for _, fl := range c.Flags {
				if fl.Name == "dry-run" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Error("usage --json should list create --dry-run")
	}
}

func TestUsageErrorsAndMissingKey(t *testing.T) {
	h := newHarness(nil)
	if code := h.run("create"); code != exitUsage {
		t.Errorf("no mode: %d", code)
	}
	if code := h.run("create", "--dir", "x"); code != exitUsage {
		t.Errorf("dir without prompt: %d", code)
	}
	if code := h.run("nope"); code != exitUsage {
		t.Errorf("unknown: %d", code)
	}
	if code := h.run("status"); code != exitUsage {
		t.Errorf("status without id: %d", code)
	}
	h.a.getenv = func(string) string { return "" }
	if code := h.run("status", "x"); code != exitErr || !strings.Contains(h.err.String(), "GEMINI_API_KEY") {
		t.Errorf("missing key: %d %s", code, h.err.String())
	}
}

func TestStateNormalization(t *testing.T) {
	for in, want := range map[string]string{
		"JOB_STATE_SUCCEEDED": "SUCCEEDED", "BATCH_STATE_RUNNING": "RUNNING", "JOB_STATE_CANCELED": "CANCELLED", "pending": "PENDING",
	} {
		if got := normState(in); got != want {
			t.Errorf("normState(%q) = %q, want %q", in, got, want)
		}
	}
	if jobID("123") != "batches/123" || jobID("batches/9") != "batches/9" {
		t.Error("jobID")
	}
}

func TestRunPromptsOrderAndFailures(t *testing.T) {
	f := newFakeAPI(t)
	h := newHarness(f)
	p := filepath.Join(t.TempDir(), "p.txt")
	os.WriteFile(p, []byte("alpha\nFAILME\ngamma\ndelta\n"), 0o644)
	code := h.run("run", "--prompts", p, "--concurrency", "3", "--quiet")
	if code != exitJobNotOK {
		t.Fatalf("exit %d, want %d: %s", code, exitJobNotOK, h.err.String())
	}
	lines := strings.Split(strings.TrimSpace(h.out.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("lines: %v", lines)
	}
	want := []string{"line-00001", "line-00002", "line-00003", "line-00004"}
	for i, l := range lines {
		var it resultItem
		json.Unmarshal([]byte(l), &it)
		if it.Key != want[i] {
			t.Errorf("order: line %d key %q", i, it.Key)
		}
		if i == 1 && !strings.Contains(it.Error, "400") {
			t.Errorf("failure line: %+v", it)
		}
		if i == 2 && it.Text != "echo:gamma" {
			t.Errorf("text: %+v", it)
		}
	}
	if !strings.Contains(h.err.String(), "3 succeeded, 1 failed") || !strings.Contains(h.err.String(), "1 of 4 requests failed") {
		t.Errorf("stderr: %s", h.err.String())
	}
	// failed keys feed --only-keys
	if code := h.run("run", "--prompts", p, "--failed-keys", "--quiet"); code != exitJobNotOK || h.out.String() != "line-00002\n" {
		t.Errorf("failed-keys: %d %q", code, h.out.String())
	}
}

func TestRunRetriesOn429(t *testing.T) {
	f := newFakeAPI(t)
	h := newHarness(f)
	p := filepath.Join(t.TempDir(), "p.txt")
	os.WriteFile(p, []byte("RETRYME\n"), 0o644)
	if code := h.run("run", "--prompts", p, "--quiet"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err.String())
	}
	if !strings.Contains(h.out.String(), `"text":"echo:RETRYME"`) || atomic.LoadInt32(&f.genCalls) != 2 {
		t.Errorf("out=%s calls=%d", h.out.String(), f.genCalls)
	}
}

func TestRunLimitAndDryRun(t *testing.T) {
	f := newFakeAPI(t)
	h := newHarness(f)
	p := filepath.Join(t.TempDir(), "p.txt")
	os.WriteFile(p, []byte("a\nb\nc\n"), 0o644)
	if code := h.run("run", "--prompts", p, "--limit", "2", "--quiet"); code != 0 {
		t.Fatal(h.err.String())
	}
	if n := atomic.LoadInt32(&f.genCalls); n != 2 {
		t.Errorf("calls = %d, want 2", n)
	}
	before := atomic.LoadInt32(&f.genCalls)
	if code := h.run("run", "--prompts", p, "--dry-run"); code != 0 || atomic.LoadInt32(&f.genCalls) != before {
		t.Errorf("dry-run must not call the API (code %d)", code)
	}
}

func TestRunDirWithOutDir(t *testing.T) {
	f := newFakeAPI(t)
	h := newHarness(f)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.png"), []byte("A"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.png"), []byte("B"), 0o644)
	out := filepath.Join(t.TempDir(), "o")
	if code := h.run("run", "--dir", dir, "--prompt", "read it", "--out-dir", out, "--quiet"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.err.String())
	}
	if b, _ := os.ReadFile(filepath.Join(out, "a.png.txt")); string(b) != "echo:read it" {
		t.Errorf("a.png.txt = %q", b)
	}
	if n := atomic.LoadInt32(&f.nUpload); n != 2 {
		t.Errorf("uploads = %d, want 2 (no jsonl upload for run)", n)
	}
}

func TestRunCancelledReturnsPartial(t *testing.T) {
	f := newFakeAPI(t)
	h := newHarness(f)
	p := filepath.Join(t.TempDir(), "p.txt")
	os.WriteFile(p, []byte("a\nb\n"), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.out.Reset()
	h.err.Reset()
	if code := h.a.run(ctx, []string{"run", "--prompts", p, "--quiet"}); code != 130 {
		t.Errorf("exit = %d, want 130 (%s)", code, h.err.String())
	}
}
