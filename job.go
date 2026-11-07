package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// flexInt accepts both JSON numbers and quoted int64 (protobuf JSON style).
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("bad integer %q", s)
	}
	*f = flexInt(n)
	return nil
}

type Stats struct {
	Requests  flexInt `json:"requestCount"`
	Succeeded flexInt `json:"successfulRequestCount"`
	Failed    flexInt `json:"failedRequestCount"`
	Pending   flexInt `json:"pendingRequestCount"`
}

type jobOutput struct {
	ResponsesFile    string          `json:"responsesFile"`
	FileName         string          `json:"fileName"`
	InlinedResponses json.RawMessage `json:"inlinedResponses"`
}

func (o *jobOutput) inlined() []json.RawMessage {
	if o == nil || len(o.InlinedResponses) == 0 {
		return nil
	}
	var arr []json.RawMessage
	if json.Unmarshal(o.InlinedResponses, &arr) == nil {
		return arr
	}
	var obj struct {
		InlinedResponses []json.RawMessage `json:"inlinedResponses"`
	}
	if json.Unmarshal(o.InlinedResponses, &obj) == nil {
		return obj.InlinedResponses
	}
	return nil
}

type jobMeta struct {
	State       string     `json:"state"`
	DisplayName string     `json:"displayName"`
	Model       string     `json:"model"`
	CreateTime  string     `json:"createTime"`
	EndTime     string     `json:"endTime"`
	BatchStats  *Stats     `json:"batchStats"`
	Output      *jobOutput `json:"output"`
}

type apiStatus struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Job is tolerant of the different shapes the batch resource is documented with
// (long-running-operation style with metadata.state, or flat state / dest).
type Job struct {
	Name        string     `json:"name"`
	Done        bool       `json:"done"`
	State       string     `json:"state"`
	DisplayName string     `json:"displayName"`
	Model       string     `json:"model"`
	CreateTime  string     `json:"createTime"`
	Metadata    *jobMeta   `json:"metadata"`
	Response    *jobOutput `json:"response"`
	Output      *jobOutput `json:"output"`
	Dest        *jobOutput `json:"dest"`
	BatchStats  *Stats     `json:"batchStats"`
	Error       *apiStatus `json:"error"`

	Raw json.RawMessage `json:"-"`
}

func parseJob(raw []byte) (*Job, error) {
	var j Job
	if err := json.Unmarshal(raw, &j); err != nil {
		return nil, fmt.Errorf("decoding batch job: %w", err)
	}
	j.Raw = append(json.RawMessage(nil), raw...)
	return &j, nil
}

func normState(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	if i := strings.LastIndex(s, "STATE_"); i >= 0 {
		s = s[i+len("STATE_"):]
	}
	if s == "CANCELED" {
		s = "CANCELLED"
	}
	return s
}

// ShortState returns PENDING, RUNNING, SUCCEEDED, FAILED, CANCELLED, EXPIRED, ...
func (j *Job) ShortState() string {
	s := ""
	if j.Metadata != nil {
		s = j.Metadata.State
	}
	if s == "" {
		s = j.State
	}
	if s = normState(s); s != "" {
		return s
	}
	if j.Done {
		if j.Error != nil {
			return "FAILED"
		}
		return "SUCCEEDED"
	}
	return "UNKNOWN"
}

func (j *Job) Terminal() bool {
	switch j.ShortState() {
	case "SUCCEEDED", "PARTIALLY_SUCCEEDED", "FAILED", "CANCELLED", "EXPIRED":
		return true
	}
	return false
}

func (j *Job) HasResults() bool {
	s := j.ShortState()
	return s == "SUCCEEDED" || s == "PARTIALLY_SUCCEEDED"
}

func (j *Job) outputs() []*jobOutput {
	outs := []*jobOutput{j.Response, j.Output, j.Dest}
	if j.Metadata != nil {
		outs = append(outs, j.Metadata.Output)
	}
	return outs
}

// ResultsFile is the File API name of the results JSONL, if any.
func (j *Job) ResultsFile() string {
	for _, o := range j.outputs() {
		if o == nil {
			continue
		}
		if o.ResponsesFile != "" {
			return o.ResponsesFile
		}
		if o.FileName != "" {
			return o.FileName
		}
	}
	return ""
}

// InlinedResponses returns inline results (when the job was created with inline requests).
func (j *Job) InlinedResponses() []json.RawMessage {
	for _, o := range j.outputs() {
		if r := o.inlined(); len(r) > 0 {
			return r
		}
	}
	return nil
}

func (j *Job) stats() *Stats {
	if j.BatchStats != nil {
		return j.BatchStats
	}
	if j.Metadata != nil {
		return j.Metadata.BatchStats
	}
	return nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// Summary is the stable, normalized shape printed by --json.
type Summary struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name,omitempty"`
	Model       string `json:"model,omitempty"`
	State       string `json:"state"`
	Done        bool   `json:"done"`
	CreateTime  string `json:"create_time,omitempty"`
	Requests    *int64 `json:"requests,omitempty"`
	Succeeded   *int64 `json:"succeeded,omitempty"`
	Failed      *int64 `json:"failed,omitempty"`
	Pending     *int64 `json:"pending,omitempty"`
	ResultsFile string `json:"results_file,omitempty"`
	Error       string `json:"error,omitempty"`
}

func i64p(f flexInt) *int64 { v := int64(f); return &v }

func (j *Job) Summary() Summary {
	s := Summary{
		Name:        j.Name,
		DisplayName: firstNonEmpty(j.DisplayName, metaStr(j, func(m *jobMeta) string { return m.DisplayName })),
		Model:       firstNonEmpty(j.Model, metaStr(j, func(m *jobMeta) string { return m.Model })),
		State:       j.ShortState(),
		Done:        j.Terminal(),
		CreateTime:  firstNonEmpty(j.CreateTime, metaStr(j, func(m *jobMeta) string { return m.CreateTime })),
		ResultsFile: j.ResultsFile(),
	}
	if st := j.stats(); st != nil {
		s.Requests, s.Succeeded, s.Failed, s.Pending = i64p(st.Requests), i64p(st.Succeeded), i64p(st.Failed), i64p(st.Pending)
	}
	if j.Error != nil {
		s.Error = j.Error.Message
	}
	return s
}

func metaStr(j *Job, f func(*jobMeta) string) string {
	if j.Metadata == nil {
		return ""
	}
	return f(j.Metadata)
}

// jobID accepts "123" or "batches/123".
func jobID(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "batches/") {
		return s
	}
	return "batches/" + s
}
