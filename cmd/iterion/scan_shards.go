// Package main: __scan-shards hidden subcommand.
//
// Implements the local-mode shard fan-out described in
// docs/security-bots-distributed.md. Spawns N child `iterion run`
// subprocesses sharing the same --store-dir, each scanning a slice of
// the file list, bounded by --max-concurrency. Polls the store for
// each child until terminal status, then emits an aggregated JSON
// envelope on stdout.
//
// Cloud-mode dispatch (NATS queue publish + parent/child relationship
// persisted via cloudpublisher) is a future swap; the bundle-facing
// surface is the same.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/spf13/cobra"
)

// Dispatch modes for __scan-shards. The local mode runs children as
// subprocesses + polls the shared store; the cloud mode POSTs to the
// iterion server's launch endpoint and lets the runner pool execute
// the children. "auto" picks cloud when ITERION_SERVER_URL is set,
// else local.
const (
	modeAuto  = "auto"
	modeLocal = "local"
	modeCloud = "cloud"
)

// statusMissing is reported when the shared store has no record of a
// dispatched child run by the time the poll loop fires. Not a real
// RunStatus — purely an out-of-band signal in the report JSON.
const statusMissing store.RunStatus = "missing"

var scanShardsOpts struct {
	parentRunID    string
	workflow       string
	filesJSON      string
	shardSize      int
	baseVarsJSON   string
	storeDir       string
	maxConcurrency int
	pollInterval   time.Duration
	timeout        time.Duration
	mode           string
	shardVarName   string
}

var scanShardsCmd = &cobra.Command{
	Use:    "__scan-shards",
	Short:  "Internal: fan out a security scan across N child runs and poll until convergence",
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runScanShards(cmd.Context(), os.Stdout)
	},
}

func init() {
	f := scanShardsCmd.Flags()
	f.StringVar(&scanShardsOpts.parentRunID, "parent-run-id", "", "Parent run id; stamped onto each child run's metadata")
	f.StringVar(&scanShardsOpts.workflow, "workflow", "", "Path to the .bot / .botz / bundle dir to run for each shard")
	f.StringVar(&scanShardsOpts.filesJSON, "files-json", "-", "Path to a JSON array of relative file paths to shard (or '-' for stdin)")
	f.IntVar(&scanShardsOpts.shardSize, "shard-size", 100, "Maximum files per shard (must be > 0)")
	f.StringVar(&scanShardsOpts.baseVarsJSON, "base-vars-json", "{}", "JSON object of --var key=value pairs forwarded to every child run")
	f.StringVar(&scanShardsOpts.storeDir, "store-dir", "", "Iterion store directory shared by parent and all children (required for the polling path)")
	f.IntVar(&scanShardsOpts.maxConcurrency, "max-concurrency", 4, "Maximum number of child runs executing in parallel")
	f.DurationVar(&scanShardsOpts.pollInterval, "poll-interval", 2*time.Second, "How often to poll the store for each child's terminal status")
	f.DurationVar(&scanShardsOpts.timeout, "timeout", 2*time.Hour, "Overall fan-out timeout. A child still running past this is reported as 'timed_out' and the parent returns non-zero.")
	f.StringVar(&scanShardsOpts.mode, "mode", "auto", "Dispatch mode: 'auto' (cloud queue if ITERION_QUEUE_NATS_URL set, else local subprocess), 'local' (force subprocess), 'cloud' (force queue; not yet implemented)")
	f.StringVar(&scanShardsOpts.shardVarName, "shard-var", "security_shard_files", "Workflow variable name receiving the shard's file list (as a JSON array string)")
	rootCmd.AddCommand(scanShardsCmd)
}

// shardPlan is the per-shard intent computed before dispatch.
type shardPlan struct {
	Index int      `json:"shard_index"`
	RunID string   `json:"run_id"`
	Files []string `json:"files"`
	Label string   `json:"shard_label"`
}

// shardResult captures one shard's outcome for the aggregated stdout report.
type shardResult struct {
	Plan     shardPlan       `json:"plan"`
	Status   store.RunStatus `json:"status"`
	Error    string          `json:"error,omitempty"`
	Started  *time.Time      `json:"started_at,omitempty"`
	Finished *time.Time      `json:"finished_at,omitempty"`
}

// scanShardsReport is the JSON envelope emitted on stdout.
type scanShardsReport struct {
	ParentRunID  string        `json:"parent_run_id"`
	Workflow     string        `json:"workflow"`
	Mode         string        `json:"mode"`
	ShardCount   int           `json:"shard_count"`
	ShardSize    int           `json:"shard_size"`
	FilesTotal   int           `json:"files_total"`
	Shards       []shardResult `json:"shards"`
	Errors       []string      `json:"errors,omitempty"`
	DurationSecs float64       `json:"duration_secs"`
}

func runScanShards(ctx context.Context, out io.Writer) error {
	if scanShardsOpts.workflow == "" {
		return fmt.Errorf("__scan-shards: --workflow is required")
	}
	if scanShardsOpts.storeDir == "" {
		return fmt.Errorf("__scan-shards: --store-dir is required (parent and children must share the store)")
	}
	if scanShardsOpts.shardSize <= 0 {
		return fmt.Errorf("__scan-shards: --shard-size must be > 0")
	}
	if scanShardsOpts.maxConcurrency <= 0 {
		return fmt.Errorf("__scan-shards: --max-concurrency must be > 0")
	}

	files, err := readFilesJSON(scanShardsOpts.filesJSON)
	if err != nil {
		return fmt.Errorf("__scan-shards: read files-json: %w", err)
	}
	if len(files) == 0 {
		_ = json.NewEncoder(out).Encode(scanShardsReport{
			ParentRunID: scanShardsOpts.parentRunID,
			Workflow:    scanShardsOpts.workflow,
			Mode:        modeLocal,
			ShardSize:   scanShardsOpts.shardSize,
			Shards:      []shardResult{},
		})
		return nil
	}

	plans := planShards(files, scanShardsOpts.shardSize, scanShardsOpts.parentRunID)
	mode := resolveMode(scanShardsOpts.mode)

	baseVars, err := parseBaseVars(scanShardsOpts.baseVarsJSON)
	if err != nil {
		return fmt.Errorf("__scan-shards: parse --base-vars-json: %w", err)
	}

	ctx, cancel := context.WithDeadline(ctx, time.Now().Add(scanShardsOpts.timeout))
	defer cancel()

	rs, openErr := store.New(scanShardsOpts.storeDir)
	if openErr != nil {
		return fmt.Errorf("__scan-shards: open store: %w", openErr)
	}

	start := time.Now()
	var results []shardResult
	switch mode {
	case modeCloud:
		results = dispatchCloud(ctx, plans, baseVars)
	default:
		results = dispatchLocal(ctx, plans, baseVars)
	}

	awaitTerminal(ctx, rs, results, scanShardsOpts.pollInterval)

	rep := scanShardsReport{
		ParentRunID:  scanShardsOpts.parentRunID,
		Workflow:     scanShardsOpts.workflow,
		Mode:         mode,
		ShardCount:   len(plans),
		ShardSize:    scanShardsOpts.shardSize,
		FilesTotal:   len(files),
		Shards:       results,
		DurationSecs: time.Since(start).Seconds(),
	}
	for _, s := range rep.Shards {
		if s.Status != store.RunStatusFinished {
			rep.Errors = append(rep.Errors, fmt.Sprintf("shard %d (%s) status=%s err=%s", s.Plan.Index, s.Plan.RunID, s.Status, s.Error))
		}
	}
	return json.NewEncoder(out).Encode(rep)
}

func resolveMode(requested string) string {
	switch requested {
	case modeCloud, modeLocal:
		return requested
	default:
		if os.Getenv("ITERION_QUEUE_NATS_URL") != "" || os.Getenv("ITERION_SERVER_URL") != "" {
			return modeCloud
		}
		return modeLocal
	}
}

// awaitTerminal polls the shared run store until every result reaches
// a terminal status or the context expires. Local-mode children are
// already terminal when dispatchLocal returns (cmd.Output blocks), so
// the loop converges on the first tick. Cloud-mode children are still
// queued/running at this point; the loop is the only thing that turns
// "POSTed" into "really done" in the report.
func awaitTerminal(ctx context.Context, rs store.RunStore, results []shardResult, interval time.Duration) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	pending := make(map[int]struct{}, len(results))
	for i := range results {
		pending[i] = struct{}{}
	}
	for len(pending) > 0 {
		for i := range pending {
			r, loadErr := rs.LoadRun(ctx, results[i].Plan.RunID)
			if loadErr != nil {
				// No run document yet. Two cases:
				//   - The shard already failed AT or BEFORE dispatch (cloud-mode
				//     pre-launch errors — bad ITERION_SERVER_URL, unreadable
				//     workflow, request-build / POST / non-2xx — set r.Error
				//     without ever creating a run). No document will EVER appear,
				//     so it is already terminal: report it now rather than poll
				//     until ctx timeout (which turned a handled failure into a
				//     multi-hour hang).
				//   - A freshly-launched cloud shard whose publisher just lags
				//     behind the HTTP response by a few ms (no error). Keep
				//     polling; on context expiry the outer select bails.
				if results[i].Error != "" {
					if results[i].Status == "" {
						results[i].Status = store.RunStatusFailed
					}
					delete(pending, i)
				}
				continue
			}
			if results[i].Started == nil {
				ct := r.CreatedAt
				results[i].Started = &ct
			}
			results[i].Status = r.Status
			if r.Error != "" && results[i].Error == "" {
				results[i].Error = r.Error
			}
			if r.Status.IsTerminal() {
				if r.FinishedAt != nil {
					ft := *r.FinishedAt
					results[i].Finished = &ft
				}
				delete(pending, i)
			}
		}
		if len(pending) == 0 {
			return
		}
		select {
		case <-ctx.Done():
			for i := range pending {
				if results[i].Status == "" {
					results[i].Status = statusMissing
				}
				if results[i].Error == "" {
					results[i].Error = fmt.Sprintf("timed out waiting for shard %s: %v", results[i].Plan.RunID, ctx.Err())
				}
			}
			return
		case <-time.After(interval):
		}
	}
}

func readFilesJSON(path string) ([]string, error) {
	var raw []byte
	var err error
	if path == "-" || path == "" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, nil
	}
	var files []string
	if err := json.Unmarshal(raw, &files); err != nil {
		return nil, fmt.Errorf("files-json must be a JSON array of strings: %w", err)
	}
	// Stable order for determinism.
	sort.Strings(files)
	return files, nil
}

func parseBaseVars(s string) (map[string]string, error) {
	if s == "" {
		return map[string]string{}, nil
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		switch vv := v.(type) {
		case string:
			out[k] = vv
		default:
			b, _ := json.Marshal(vv)
			out[k] = string(b)
		}
	}
	return out, nil
}

// planShards splits the file list into shards of shardSize and assigns
// deterministic run ids derived from
// sha256(parentRunID || shard_index || shard_size || file-list digest).
// Same (parent, file list, shard_size) ⇒ same plan ⇒ idempotent reruns;
// crucially, two scans with the SAME file count but DIFFERENT files (or a
// different shard_size) now get disjoint ids. The prior seed keyed only on
// len(files), so different file lists of equal length collided and the
// no-clobber store (writeRunNew's O_EXCL) rejected the second scan's shards
// with "already exists". Pass a unique --parent-run-id ({{run.id}}) to also
// make re-scans of the SAME file list disjoint.
func planShards(files []string, shardSize int, parentRunID string) []shardPlan {
	// Digest the full file list once (shared across shards); combined with
	// the per-shard index below it yields a unique id per shard while
	// staying deterministic over identical inputs. A NUL separator can't
	// appear in a path, so distinct lists can't alias via concatenation.
	fh := sha256.New()
	for _, f := range files {
		fh.Write([]byte(f))
		fh.Write([]byte{0})
	}
	filesDigest := hex.EncodeToString(fh.Sum(nil)[:8])
	var plans []shardPlan
	for i := 0; i < len(files); i += shardSize {
		end := i + shardSize
		if end > len(files) {
			end = len(files)
		}
		idx := len(plans)
		seed := fmt.Sprintf("%s:%d:%d:%s", parentRunID, idx, shardSize, filesDigest)
		sum := sha256.Sum256([]byte(seed))
		plans = append(plans, shardPlan{
			Index: idx,
			RunID: "shard-" + hex.EncodeToString(sum[:8]),
			Files: append([]string{}, files[i:end]...),
			Label: fmt.Sprintf("files %d-%d (of %d)", i, end-1, len(files)),
		})
	}
	return plans
}

// dispatchShards drives the concurrency-bounded fan-out shared by
// both local and cloud modes. Each plan is dispatched via `do` under
// a semaphore sized by --max-concurrency; results are pre-populated
// and `do` mutates them in place. The result slice is in the same
// order as `plans`.
func dispatchShards(plans []shardPlan, do func(p shardPlan, r *shardResult)) []shardResult {
	results := make([]shardResult, len(plans))
	for i, p := range plans {
		results[i] = shardResult{Plan: p}
	}
	sem := make(chan struct{}, scanShardsOpts.maxConcurrency)
	var wg sync.WaitGroup
	for i := range plans {
		i := i
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			started := time.Now()
			results[i].Started = &started
			do(plans[i], &results[i])
			finished := time.Now()
			results[i].Finished = &finished
		}()
	}
	wg.Wait()
	return results
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}

// dispatchLocal spawns child `iterion run` subprocesses sharing the
// parent's --store-dir. cmd.Output blocks per shard, so children are
// guaranteed terminal when this returns — the downstream awaitTerminal
// poll converges on the first tick.
func dispatchLocal(ctx context.Context, plans []shardPlan, baseVars map[string]string) []shardResult {
	exePath, err := os.Executable()
	if err != nil {
		exePath = os.Args[0]
	}
	exePath, _ = filepath.Abs(exePath)

	return dispatchShards(plans, func(p shardPlan, r *shardResult) {
		shardJSON, _ := json.Marshal(p.Files)
		args := []string{
			"run", scanShardsOpts.workflow,
			"--run-id", p.RunID,
			"--store-dir", scanShardsOpts.storeDir,
			"--no-interactive",
			"--var", scanShardsOpts.shardVarName + "=" + string(shardJSON),
			"--var", "parent_run_id=" + scanShardsOpts.parentRunID,
			"--var", fmt.Sprintf("shard_index=%d", p.Index),
		}
		for k, v := range baseVars {
			args = append(args, "--var", k+"="+v)
		}
		cmd := exec.CommandContext(ctx, exePath, args...)
		cmd.Env = append(os.Environ(),
			"ITERION_PARENT_RUN_ID="+scanShardsOpts.parentRunID,
			fmt.Sprintf("ITERION_SHARD_INDEX=%d", p.Index),
			fmt.Sprintf("ITERION_SHARD_COUNT=%d", len(plans)),
		)
		cmd.Stderr = os.Stderr
		out, runErr := cmd.Output()
		if runErr != nil {
			r.Error = fmt.Sprintf("%v: %s", runErr, truncate(string(out), 512))
		}
	})
}

// dispatchCloud POSTs each shard to the iterion server's launch
// endpoint and returns once the server has accepted the request — the
// child runs continue executing inside the runner pool. The terminal
// status is collected by awaitTerminal polling the shared store.
//
// Required env:
//   - ITERION_SERVER_URL : base URL of the iterion server
//
// Optional env:
//   - ITERION_SERVER_TOKEN : Bearer token, when the server gates
//     the launch endpoint behind SSO
func dispatchCloud(ctx context.Context, plans []shardPlan, baseVars map[string]string) []shardResult {
	serverURL := strings.TrimRight(os.Getenv("ITERION_SERVER_URL"), "/")
	if serverURL == "" {
		// Fail every shard with the same message so the report makes the misconfiguration obvious.
		return dispatchShards(plans, func(_ shardPlan, r *shardResult) {
			r.Error = "ITERION_SERVER_URL not set; cloud mode requires the server endpoint"
		})
	}
	token := os.Getenv("ITERION_SERVER_TOKEN")

	src, srcErr := os.ReadFile(scanShardsOpts.workflow)
	if srcErr != nil {
		return dispatchShards(plans, func(_ shardPlan, r *shardResult) {
			r.Error = fmt.Sprintf("read workflow source: %v", srcErr)
		})
	}

	return dispatchShards(plans, func(p shardPlan, r *shardResult) {
		shardJSON, _ := json.Marshal(p.Files)
		vars := map[string]string{
			scanShardsOpts.shardVarName: string(shardJSON),
			"parent_run_id":             scanShardsOpts.parentRunID,
			"shard_index":               fmt.Sprintf("%d", p.Index),
		}
		for k, v := range baseVars {
			vars[k] = v
		}
		body, _ := json.Marshal(map[string]any{
			"file_path":     scanShardsOpts.workflow,
			"source":        string(src),
			"run_id":        p.RunID,
			"vars":          vars,
			"parent_run_id": scanShardsOpts.parentRunID,
			"shard_index":   p.Index,
			"shard_count":   len(plans),
			"shard_label":   p.Label,
		})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, serverURL+"/api/v1/runs/launch", bytes.NewReader(body))
		if err != nil {
			r.Error = fmt.Sprintf("build launch request: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			r.Error = fmt.Sprintf("POST /runs/launch: %v", err)
			return
		}
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 300 {
			r.Error = fmt.Sprintf("server returned %d: %s", resp.StatusCode, truncate(string(respBody), 512))
		}
	})
}
