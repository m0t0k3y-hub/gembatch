package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"sync"
	"sync/atomic"
)

// okLine / errLine build result lines in the same shape the Batch API produces,
// so `run` and `results` share one parser and one set of output options.
func okLine(key string, resp json.RawMessage) []byte {
	b, _ := json.Marshal(struct {
		Key      string          `json:"key"`
		Response json.RawMessage `json:"response"`
	}{key, resp})
	return b
}

func errLine(key string, err error) []byte {
	code := 0
	var ae *APIError
	if errors.As(err, &ae) {
		code = ae.Status
	}
	b, _ := json.Marshal(map[string]any{"key": key, "error": map[string]any{"code": code, "message": err.Error()}})
	return b
}

func cmdRun(a *app, fs *flag.FlagSet) func(context.Context, []string) error {
	var o createOpts
	var ro resultOpts
	bindInputFlags(a, fs, &o)
	bindOutputFlags(fs, &ro)
	quiet := fs.Bool("quiet", false, "do not print progress to stderr")

	return func(ctx context.Context, pos []string) error {
		if len(pos) > 0 {
			return usageErr("run takes no positional arguments (got %q)", pos[0])
		}
		if err := ro.validate(); err != nil {
			return err
		}
		lines, err := o.prepare(ctx, a)
		if err != nil {
			return err
		}
		if o.saveRequests != "" || o.dryRun {
			buf, err := encodeLines(lines)
			if err != nil {
				return err
			}
			if o.saveRequests != "" {
				if err := writeFile(o.saveRequests, buf.Bytes()); err != nil {
					return err
				}
			}
			if o.dryRun {
				a.reportDryRun(&o, lines, buf.Len())
				return nil
			}
		}
		c, err := a.client()
		if err != nil {
			return err
		}

		// Fan out with a bounded worker pool; keep results in input order.
		results := make([][]byte, len(lines))
		var (
			wg   sync.WaitGroup
			done int32
			sem  = make(chan struct{}, o.concurrency)
		)
	feed:
		for i, l := range lines {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				break feed
			}
			wg.Add(1)
			go func(i int, l reqLine) {
				defer wg.Done()
				defer func() { <-sem }()
				if ctx.Err() != nil {
					return
				}
				resp, err := c.GenerateContent(ctx, o.model, l.Request)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					results[i] = errLine(l.Key, err)
				} else {
					results[i] = okLine(l.Key, resp)
				}
				n := atomic.AddInt32(&done, 1)
				if !*quiet && (int(n)%10 == 0 || int(n) == len(lines)) {
					a.logf("%d/%d done\n", n, len(lines))
				}
			}(i, l)
		}
		wg.Wait()

		var finished [][]byte
		for _, r := range results {
			if r != nil {
				finished = append(finished, r)
			}
		}
		ok, bad, werr := a.writeResults(finished, &ro)
		if werr != nil {
			return werr
		}
		a.logf("%d succeeded, %d failed (of %d)\n", ok, bad, len(lines))
		if ctx.Err() != nil {
			return fmt.Errorf("interrupted after %d of %d requests: %w", len(finished), len(lines), ctx.Err())
		}
		if bad > 0 {
			if !ro.failedKeys && !ro.failedOnly {
				a.logf("tip: rerun only the failures with --failed-keys > failed.txt and --only-keys failed.txt\n")
			}
			return &exitError{exitJobNotOK, fmt.Errorf("%d of %d requests failed", bad, len(lines))}
		}
		return nil
	}
}
