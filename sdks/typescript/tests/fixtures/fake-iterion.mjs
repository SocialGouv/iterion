#!/usr/bin/env node
/**
 * Fake `iterion` binary for unit tests.
 *
 * Behaviour is driven by environment variables so each test can
 * cherry-pick a scenario without re-implementing the whole CLI:
 *
 *   FAKE_ITERION_RECORD=/path     append received argv (one JSON line per call) to this file
 *   FAKE_ITERION_STDOUT=...       write this string to stdout (raw)
 *   FAKE_ITERION_STDERR=...       write this string to stderr (raw)
 *   FAKE_ITERION_EXIT=N           exit with this code (default 0)
 *   FAKE_ITERION_DELAY_MS=N       sleep N ms before exiting (lets tests cancel)
 *   FAKE_ITERION_SCENARIO=name    canned scenario; takes precedence over the raw vars above
 *
 * Canned scenarios:
 *   version                   → prints "v0.2.0\n", exits 0
 *   validate-ok               → prints {"file":"…","valid":true,"workflow_name":"demo",…}, exits 0
 *   validate-fail             → prints structured envelope + outer {error} envelope, exits 1
 *                                (mirrors the real CLI's double-write on validation failure)
 *   validate-error-envelope   → prints only {"error":"cannot read file: …"}, exits 1
 *                                (pre-validation invocation failure; no validate fields)
 *   run-finished              → prints {"status":"finished",...}, exits 0
 *   run-paused                → prints {"status":"paused_waiting_human",...}, exits 0
 *   run-failed                → prints structured {status:"failed",error,...} envelope
 *                                FOLLOWED by the outer {"error":"..."} envelope
 *                                (mirrors the real CLI's double-write on failure),
 *                                exits 1, stderr contains "error [BUDGET_EXCEEDED]: …"
 *   run-cancelled             → prints structured {status:"cancelled",...} envelope
 *                                FOLLOWED by the outer {"error":"run cancelled"} envelope
 *                                (mirrors the real CLI's double-write on cancellation),
 *                                exits 1
 *   run-error-envelope        → prints {"error":"parse error: …"}, exits 1
 *                                (pre-engine failure; no status field)
 *   resume-finished           → prints {"status":"finished",...}, exits 0
 *   resume-failed             → prints structured {status:"failed",error,...} envelope
 *                                FOLLOWED by the outer {"error":"..."} envelope, exits 1
 *   resume-error-envelope     → prints {"error":"cannot load run: …"}, exits 1
 *   inspect-list              → prints []
 *   inspect-single            → prints {"run":{...}}
 *   diagram                   → prints {"view":"compact","mermaid":"flowchart\n..."}
 *   report                    → prints {"run_id":"r","output":"path"}
 *
 * The fake never prints to a TTY and never reads stdin (other than
 * closing it).
 */

import { appendFile } from "node:fs/promises";

const argv = process.argv.slice(2);

if (process.env.FAKE_ITERION_RECORD) {
  try {
    await appendFile(
      process.env.FAKE_ITERION_RECORD,
      JSON.stringify({ argv, env: filterEnv(process.env) }) + "\n",
    );
  } catch {
    // best-effort
  }
}

if (process.env.FAKE_ITERION_DELAY_MS) {
  const ms = Number(process.env.FAKE_ITERION_DELAY_MS);
  await new Promise((r) => setTimeout(r, Number.isFinite(ms) ? ms : 0));
}

const scenario = process.env.FAKE_ITERION_SCENARIO;
let { stdout, stderr, exit } = pickOutputs(scenario, argv);

// Allow per-call overrides on top of the scenario.
if (process.env.FAKE_ITERION_STDOUT !== undefined) stdout = process.env.FAKE_ITERION_STDOUT;
if (process.env.FAKE_ITERION_STDERR !== undefined) stderr = process.env.FAKE_ITERION_STDERR;
if (process.env.FAKE_ITERION_EXIT !== undefined) exit = Number(process.env.FAKE_ITERION_EXIT);

if (stdout) process.stdout.write(stdout);
if (stderr) process.stderr.write(stderr);
process.exit(Number.isFinite(exit) ? exit : 0);

// ---- helpers --------------------------------------------------------------

function pickOutputs(scenario, argv) {
  switch (scenario) {
    case "version":
      return { stdout: "v0.2.0-test\n", stderr: "", exit: 0 };
    case "validate-ok":
      return {
        stdout:
          JSON.stringify({
            file: argv[argv.indexOf("validate") + 1] ?? "demo.iter",
            valid: true,
            workflow_name: "demo",
            node_count: 3,
            edge_count: 2,
          }) + "\n",
        stderr: "",
        exit: 0,
      };
    case "validate-fail":
      // Mirrors the real CLI's double-write: structured envelope from
      // pkg/cli/validate.go followed by the outer {"error":"validation failed"}
      // envelope from cmd/iterion/main.go.
      return {
        stdout:
          JSON.stringify({
            file: argv[argv.indexOf("validate") + 1] ?? "bad.iter",
            valid: false,
            parse_diagnostics: [
              "bad.iter:1:14: error [E002]: expected :, got Newline",
              "bad.iter:2:1: error [E002]: expected INDENT, got EOF",
            ],
          }) +
          "\n" +
          JSON.stringify({ error: "validation failed" }) +
          "\n",
        stderr: "",
        exit: 1,
      };
    case "validate-error-envelope":
      return {
        stdout:
          JSON.stringify({
            error: `cannot read file: ${argv[argv.indexOf("validate") + 1] ?? "missing.iter"}`,
          }) + "\n",
        stderr: "",
        exit: 1,
      };
    case "run-finished":
      return {
        stdout:
          JSON.stringify({
            run_id: "run_1",
            workflow: "demo",
            store: ".iterion",
            status: "finished",
          }) + "\n",
        stderr: "info: starting demo\n",
        exit: 0,
      };
    case "run-paused":
      return {
        stdout:
          JSON.stringify({
            run_id: "run_1",
            workflow: "demo",
            store: ".iterion",
            status: "paused_waiting_human",
            file: argv[argv.indexOf("run") + 1] ?? "",
            interaction_id: "run_1_review",
            node_id: "review",
            questions: { summary: "is this ok?" },
          }) + "\n",
        stderr: "",
        exit: 0,
      };
    case "run-failed":
      // Real CLI double-write: pkg/cli/run.go emits the structured failed
      // envelope and `return err`s, then cmd/iterion/main.go writes a second
      // bare {"error":"…"} envelope on top. The SDK must parse the FIRST
      // envelope so the typed status / error field survives.
      return {
        stdout:
          JSON.stringify({
            run_id: "run_1",
            workflow: "demo",
            store: ".iterion",
            status: "failed",
            error: "budget cap",
          }) +
          "\n" +
          JSON.stringify({ error: "budget cap" }) +
          "\n",
        stderr:
          "error [BUDGET_EXCEEDED]: budget cap\n  node: planner\n  hint: increase max_tokens\n",
        exit: 1,
      };
    case "run-cancelled":
      // Same double-write pattern as run-failed (pkg/cli/run.go:248-256
      // returns ErrRunCancelled after emitting the structured envelope).
      return {
        stdout:
          JSON.stringify({
            run_id: "run_1",
            workflow: "demo",
            store: ".iterion",
            status: "cancelled",
          }) +
          "\n" +
          JSON.stringify({ error: "run cancelled" }) +
          "\n",
        stderr: "",
        exit: 1,
      };
    case "run-error-envelope":
      // Pre-engine failure (parse error in .iter, missing file, etc.):
      // the CLI bails out before assigning a status and the global error
      // handler in cmd/iterion/main.go writes only the bare error envelope.
      return {
        stdout:
          JSON.stringify({
            error: "parse error: /tmp/bad.iter:2:3: error [E012]: unknown agent property 'prompt'",
          }) + "\n",
        stderr: "",
        exit: 1,
      };
    case "resume-finished":
      return {
        stdout:
          JSON.stringify({
            run_id: "run_1",
            workflow: "demo",
            status: "finished",
          }) + "\n",
        stderr: "",
        exit: 0,
      };
    case "resume-failed":
      // pkg/cli/resume.go:182-191 mirrors the run-failed double-write.
      return {
        stdout:
          JSON.stringify({
            run_id: "run_1",
            workflow: "demo",
            status: "failed",
            error: "execution failed at node planner",
          }) +
          "\n" +
          JSON.stringify({ error: "execution failed at node planner" }) +
          "\n",
        stderr:
          "error [EXECUTION_FAILED]: execution failed at node planner\n  node: planner\n",
        exit: 1,
      };
    case "resume-error-envelope":
      return {
        stdout:
          JSON.stringify({
            error: "cannot load run: store: load run non_existent: no such file or directory",
          }) + "\n",
        stderr: "",
        exit: 1,
      };
    case "inspect-list":
      return { stdout: JSON.stringify([]) + "\n", stderr: "", exit: 0 };
    case "inspect-single":
      return {
        stdout:
          JSON.stringify({
            run: {
              format_version: 1,
              id: "run_1",
              workflow_name: "demo",
              status: "finished",
              created_at: "2026-01-01T00:00:00Z",
              updated_at: "2026-01-01T00:00:01Z",
            },
          }) + "\n",
        stderr: "",
        exit: 0,
      };
    case "diagram":
      return {
        stdout:
          JSON.stringify({ view: "compact", mermaid: "flowchart TD\n  A --> B\n" }) + "\n",
        stderr: "",
        exit: 0,
      };
    case "report":
      return {
        stdout: JSON.stringify({ run_id: "run_1", output: "/tmp/report.md" }) + "\n",
        stderr: "",
        exit: 0,
      };
    default:
      return { stdout: "", stderr: "", exit: 0 };
  }
}

function filterEnv(env) {
  const out = {};
  for (const [k, v] of Object.entries(env)) {
    if (k.startsWith("FAKE_ITERION_") || k === "ITERION_BIN" || k.startsWith("ITERION_")) {
      out[k] = v;
    }
  }
  return out;
}
