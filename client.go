package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const defaultBaseURL = "https://generativelanguage.googleapis.com"

// Client is a minimal REST client for the Gemini API (batches + files).
type Client struct {
	BaseURL   string
	APIKey    string
	HTTP      *http.Client
	Sleep     func(context.Context, time.Duration) error
	UserAgent string
}

// APIError is a non-2xx response from the API.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	msg := e.Body
	var env struct {
		Error struct {
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(e.Body), &env) == nil && env.Error.Message != "" {
		msg = env.Error.Message
		if env.Error.Status != "" {
			msg = env.Error.Status + ": " + msg
		}
	}
	return fmt.Sprintf("API error (HTTP %d): %s", e.Status, truncate(msg, 400))
}

// send performs a request. With retry=true it retries network errors, 429 and 5xx
// with exponential backoff; only use that for idempotent calls.
func (c *Client) send(ctx context.Context, method, u string, body []byte, hdr map[string]string, retry bool) (*http.Response, error) {
	attempts := 1
	if retry {
		attempts = 4
	}
	return c.sendN(ctx, method, u, body, hdr, attempts)
}

func (c *Client) sendN(ctx context.Context, method, u string, body []byte, hdr map[string]string, attempts int) (*http.Response, error) {
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			if err := c.Sleep(ctx, time.Duration(1<<(i-1))*time.Second); err != nil {
				return nil, err
			}
		}
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rd)
		if err != nil {
			return nil, err
		}
		req.Header.Set("x-goog-api-key", c.APIKey)
		req.Header.Set("User-Agent", c.UserAgent)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		if resp.StatusCode/100 == 2 {
			return resp, nil
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		lastErr = &APIError{Status: resp.StatusCode, Body: string(b)}
		if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
			return nil, lastErr
		}
	}
	return nil, lastErr
}

func (c *Client) doJSON(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	hdr := map[string]string{}
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = b
		hdr["Content-Type"] = "application/json"
	}
	retry := method == http.MethodGet || method == http.MethodDelete
	resp, err := c.send(ctx, method, c.BaseURL+path, body, hdr, retry)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}

// FileInfo is the subset of the File API resource we need.
type FileInfo struct {
	Name     string `json:"name"`
	URI      string `json:"uri"`
	MimeType string `json:"mimeType"`
}

// UploadFile uploads a local file through the resumable File API.
func (c *Client) UploadFile(ctx context.Context, path, mimeType string) (*FileInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return c.UploadReader(ctx, f, st.Size(), mimeType, baseName(path))
}

func baseName(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// UploadReader uploads size bytes from r using the two-step resumable protocol.
func (c *Client) UploadReader(ctx context.Context, r io.Reader, size int64, mimeType, displayName string) (*FileInfo, error) {
	meta, _ := json.Marshal(map[string]any{"file": map[string]any{"display_name": displayName}})
	resp, err := c.send(ctx, http.MethodPost, c.BaseURL+"/upload/v1beta/files", meta, map[string]string{
		"X-Goog-Upload-Protocol":              "resumable",
		"X-Goog-Upload-Command":               "start",
		"X-Goog-Upload-Header-Content-Length": strconv.FormatInt(size, 10),
		"X-Goog-Upload-Header-Content-Type":   mimeType,
		"Content-Type":                        "application/json",
	}, false)
	if err != nil {
		return nil, fmt.Errorf("starting upload of %s: %w", displayName, err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	uploadURL := resp.Header.Get("X-Goog-Upload-Url")
	if uploadURL == "" {
		return nil, fmt.Errorf("starting upload of %s: no x-goog-upload-url header in response", displayName)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, r)
	if err != nil {
		return nil, err
	}
	req.ContentLength = size
	req.Header.Set("X-Goog-Upload-Offset", "0")
	req.Header.Set("X-Goog-Upload-Command", "upload, finalize")
	req.Header.Set("User-Agent", c.UserAgent)
	// Only send the API key to the same host as the API itself.
	if bu, err1 := url.Parse(c.BaseURL); err1 == nil && bu.Host == req.URL.Host {
		req.Header.Set("x-goog-api-key", c.APIKey)
	}
	up, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("uploading %s: %w", displayName, err)
	}
	defer up.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(up.Body, 1<<20))
	if up.StatusCode/100 != 2 {
		return nil, fmt.Errorf("uploading %s: %w", displayName, &APIError{Status: up.StatusCode, Body: string(data)})
	}
	var out struct {
		File FileInfo `json:"file"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.File.Name == "" {
		return nil, fmt.Errorf("uploading %s: unexpected response: %s", displayName, truncate(string(data), 200))
	}
	return &out.File, nil
}

// Download streams a File API resource (e.g. "files/abc123").
func (c *Client) Download(ctx context.Context, name string) (io.ReadCloser, error) {
	u := c.BaseURL + "/download/v1beta/" + strings.TrimPrefix(name, "/") + ":download?alt=media"
	resp, err := c.send(ctx, http.MethodGet, u, nil, nil, true)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (c *Client) CreateBatch(ctx context.Context, model, displayName, inputFile string) (*Job, error) {
	model = "models/" + strings.TrimPrefix(model, "models/")
	in := map[string]any{"batch": map[string]any{
		"display_name": displayName,
		"input_config": map[string]any{"file_name": inputFile},
	}}
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodPost, "/v1beta/"+model+":batchGenerateContent", in, &raw); err != nil {
		return nil, err
	}
	return parseJob(raw)
}

func (c *Client) GetJob(ctx context.Context, name string) (*Job, error) {
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, "/v1beta/"+name, nil, &raw); err != nil {
		return nil, err
	}
	return parseJob(raw)
}

func (c *Client) ListJobs(ctx context.Context, limit int) ([]*Job, error) {
	var jobs []*Job
	token := ""
	for len(jobs) < limit {
		size := limit - len(jobs)
		if size > 100 {
			size = 100
		}
		q := url.Values{"pageSize": {strconv.Itoa(size)}}
		if token != "" {
			q.Set("pageToken", token)
		}
		var page struct {
			Operations    []json.RawMessage `json:"operations"`
			Batches       []json.RawMessage `json:"batches"`
			NextPageToken string            `json:"nextPageToken"`
		}
		if err := c.doJSON(ctx, http.MethodGet, "/v1beta/batches?"+q.Encode(), nil, &page); err != nil {
			return nil, err
		}
		items := append(page.Operations, page.Batches...)
		for _, it := range items {
			j, err := parseJob(it)
			if err != nil {
				return nil, err
			}
			jobs = append(jobs, j)
		}
		if page.NextPageToken == "" || len(items) == 0 {
			break
		}
		token = page.NextPageToken
	}
	if len(jobs) > limit {
		jobs = jobs[:limit]
	}
	return jobs, nil
}

func (c *Client) CancelJob(ctx context.Context, name string) error {
	return c.doJSON(ctx, http.MethodPost, "/v1beta/"+name+":cancel", nil, nil)
}

func (c *Client) DeleteJob(ctx context.Context, name string) error {
	return c.doJSON(ctx, http.MethodDelete, "/v1beta/"+name, nil, nil)
}

// GenerateContent calls the regular (synchronous) generateContent endpoint.
// req is a GenerateContentRequest; the raw GenerateContentResponse is returned.
// Generation has no side effects, so 429/5xx/network errors are retried with backoff.
func (c *Client) GenerateContent(ctx context.Context, model string, req json.RawMessage) (json.RawMessage, error) {
	model = "models/" + strings.TrimPrefix(model, "models/")
	resp, err := c.sendN(ctx, http.MethodPost, c.BaseURL+"/v1beta/"+model+":generateContent", req,
		map[string]string{"Content-Type": "application/json"}, 6)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}
