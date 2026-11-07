---
name: gembatch
description: Run many independent Gemini requests (OCR/transcription of images or PDFs, classification, summarization, translation, eval runs) from the command line with the gembatch CLI, either through the Gemini Batch API at 50% cost (results within 24h) or through the regular synchronous API (`gembatch run`) when results are needed now. Use for tens to thousands of independent prompts or files.
---

# gembatch

`gembatch` sends many independent requests to Gemini and returns normalized results. Two modes take the same inputs and produce the same output format:

- `gembatch run`: regular synchronous API, parallel, results now. Full price.
- `gembatch create` + `wait` + `results`: Batch API, asynchronous, 50% of the price, target turnaround 24 hours (often much faster).

## When to use which

- Need results now, or fewer than ~50 items, or trying a prompt out: `run` (use `--limit 3` first).
- Hundreds or more items and no hurry: batch (`create`).
- A single ad-hoc question does not need this tool.

## Prerequisites

- `gembatch` on PATH (single binary; download from the project's GitHub Releases). Check with `gembatch version`.
- `GEMINI_API_KEY` exported. Never print or log the key.
- Run `gembatch usage --json` once to get the exact flags of the installed version. It is the source of truth.

## Workflow

Always pass `--json` so output is machine-readable (or set `GEMBATCH_JSON=1`).

0. **Trial with the regular API** (cheap, immediate, validates key/model/prompt before a big batch):
   ```
   gembatch run --dir scans/ --glob '*.png' --prompt 'Transcribe this page as JSON' --limit 3 --json
   ```
   `run` takes the same input flags as `create`, plus `--concurrency`, `--out`, `--out-dir`, `--format`. Exit code 3 means some requests failed; see `--failed-keys`.
1. **Dry-run first.** It needs no API key and shows what will be sent.
   ```
   gembatch create --dir scans/ --glob '*.png' --prompt 'Transcribe this page as JSON' --dry-run --json
   ```
2. **Submit.** Exactly one input mode:
   - `--dir DIR --prompt TEXT [--glob '*.png']`: one request per file; files are uploaded and the key is the file name.
   - `--prompts FILE`: one prompt per line; keys are `line-00001`, ...
   - `--input FILE.jsonl`: your own lines `{"key": "...", "request": {GenerateContentRequest}}`; keys must be unique.
   Optional: `--model`, `--system`, `--config '{"temperature":0}'`, `--name`.
   The JSON output contains `job.name` (e.g. `batches/abc123`). Record it; it is the only handle to the job.
3. **Wait without blocking forever.** Use a timeout, and re-run if it returns exit code 4:
   ```
   gembatch wait batches/abc123 --interval 60s --timeout 10m --json
   ```
   For jobs that will take hours, do not hold a session open. Report the job name to the user and check later with `gembatch status <job>`.
4. **Fetch results** once state is `SUCCEEDED`:
   ```
   gembatch results batches/abc123 --out results.jsonl        # {"key","text","error"} per line
   gembatch results batches/abc123 --out-dir out/             # one <key>.txt per success
   ```
5. **Retry failures** instead of resubmitting everything:
   ```
   gembatch results batches/abc123 --failed-keys > failed.txt
   gembatch create --dir scans/ --prompt '...' --only-keys failed.txt --json
   ```

## Exit codes

0 ok, 1 error, 2 bad usage, 3 job ended in a state other than SUCCEEDED (FAILED / CANCELLED / EXPIRED) or some `run` requests failed, 4 `wait` timed out (job may still be running).

## Rules

- Confirm with the user before submitting a large job: the request count and model from `--dry-run` tell the scale.
- A successful job can still contain failed items (blocked content, bad files). Always check the `failed` count and handle those keys.
- Input file limit is 2 GB per job; split larger sets into several jobs. Results are kept for a limited time (about 6 weeks), so download them promptly.
- `cancel` stops a job, `delete` removes its record; use them only when asked.
- If a call fails with HTTP 400 on create, re-check `--model` and that the request JSON is a valid GenerateContentRequest.
