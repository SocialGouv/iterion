// Typed, host-owned execution boundary for assistant action requests.
//
// The model may emit an id, intent and JSON arguments. It cannot register an
// action, choose an executor, relax policy, or pass arbitrary request options:
// this module rebuilds every API payload from a closed allow-list of fields.

import * as botsApi from "@/api/bots";
import * as dispatcherApi from "@/api/dispatcher";
import * as nativeApi from "@/api/native";
import * as pipelineApi from "@/api/pipelineBoards";
import * as pluginsApi from "@/api/plugins";
import * as runsApi from "@/api/runs";
import {
  ASSISTANT_ACTIONS,
  type AssistantActionDefinition,
  type AssistantActionId,
  type AssistantActionRequest,
} from "./assistantActions";

export interface ValidatedAssistantActionRequest
  extends AssistantActionRequest {
  definition: AssistantActionDefinition;
  title: string;
  detail: string;
  args: Record<string, unknown>;
}

export interface AssistantActionResult {
  message: string;
  href?: string;
  hrefLabel?: string;
}

type UnknownRecord = Record<string, unknown>;

function record(value: unknown): UnknownRecord | null {
  return value !== null && typeof value === "object" && !Array.isArray(value)
    ? (value as UnknownRecord)
    : null;
}

function text(
  source: UnknownRecord,
  key: string,
  opts: { required?: boolean; max?: number } = {},
): string | undefined {
  const value = source[key];
  if (value === undefined || value === null || value === "") {
    if (opts.required) throw new Error(`Missing ${key}`);
    return undefined;
  }
  if (typeof value !== "string") throw new Error(`${key} must be text`);
  const trimmed = value.trim();
  if (!trimmed && opts.required) throw new Error(`Missing ${key}`);
  const max = opts.max ?? 20_000;
  if (trimmed.length > max) throw new Error(`${key} is too long`);
  return trimmed || undefined;
}

function bool(source: UnknownRecord, key: string): boolean | undefined {
  const value = source[key];
  if (value === undefined) return undefined;
  if (typeof value !== "boolean") throw new Error(`${key} must be true or false`);
  return value;
}

function numberValue(source: UnknownRecord, key: string): number | undefined {
  const value = source[key];
  if (value === undefined) return undefined;
  if (typeof value !== "number" || !Number.isFinite(value)) {
    throw new Error(`${key} must be a finite number`);
  }
  return value;
}

function strings(
  source: UnknownRecord,
  key: string,
  maxItems = 50,
): string[] | undefined {
  const value = source[key];
  if (value === undefined) return undefined;
  if (!Array.isArray(value) || value.length > maxItems) {
    throw new Error(`${key} must be a bounded text list`);
  }
  if (value.some((entry) => typeof entry !== "string")) {
    throw new Error(`${key} must contain only text`);
  }
  return value.map((entry) => entry.trim()).filter(Boolean);
}

function stringMap(
  source: UnknownRecord,
  key: string,
  maxItems = 64,
): Record<string, string> | undefined {
  const value = source[key];
  if (value === undefined) return undefined;
  const valueRecord = record(value);
  if (!valueRecord || Object.keys(valueRecord).length > maxItems) {
    throw new Error(`${key} must be a bounded text map`);
  }
  const out: Record<string, string> = {};
  for (const [name, entry] of Object.entries(valueRecord)) {
    if (!name.trim() || name.length > 128 || typeof entry !== "string") {
      throw new Error(`${key} must contain only bounded text values`);
    }
    if (entry.length > 20_000) throw new Error(`${key}.${name} is too long`);
    out[name] = entry;
  }
  return out;
}

function optional<V>(
  target: UnknownRecord,
  key: string,
  value: V | undefined,
): void {
  if (value !== undefined) target[key] = value;
}

function issueFields(source: UnknownRecord): UnknownRecord {
  const out: UnknownRecord = {};
  optional(out, "title", text(source, "title", { max: 500 }));
  optional(out, "body", text(source, "body"));
  optional(out, "labels", strings(source, "labels"));
  optional(out, "priority", numberValue(source, "priority"));
  optional(out, "assignee", text(source, "assignee", { max: 256 }));
  optional(out, "blockers", strings(source, "blockers"));
  optional(out, "bot", text(source, "bot", { max: 256 }));
  optional(out, "bot_args", stringMap(source, "bot_args"));
  return out;
}

function pipelineFields(source: UnknownRecord): UnknownRecord {
  const out = issueFields(source);
  delete out.assignee;
  return out;
}

function actionDefinition(id: AssistantActionId): AssistantActionDefinition {
  const definition = ASSISTANT_ACTIONS.find((entry) => entry.id === id);
  if (!definition) throw new Error(`Unknown action ${id}`);
  return definition;
}

export function validateAssistantActionRequest(
  request: AssistantActionRequest,
): ValidatedAssistantActionRequest {
  const source = request.args;
  let args: UnknownRecord = {};
  const title = actionDefinition(request.id).label;
  let detail = "";
  const requiredId = (key: string) =>
    text(source, key, { required: true, max: 512 }) as string;

  switch (request.id) {
    case "editor.apply":
    case "editor.save":
      throw new Error("Editor actions use the live editor-session protocol");

    case "board.issue.create": {
      const issue = issueFields(source);
      issue.title = text(source, "title", { required: true, max: 500 });
      optional(issue, "state", text(source, "state", { max: 128 }));
      args = issue;
      detail = `Create “${issue.title}” on the board`;
      break;
    }
    case "board.issue.update": {
      const issueId = requiredId("issue_id");
      const patch = issueFields(source);
      if (Object.keys(patch).length === 0) throw new Error("No issue fields to update");
      args = { issue_id: issueId, patch };
      detail = `Update board issue ${issueId}`;
      break;
    }
    case "board.issue.transition": {
      const issueId = requiredId("issue_id");
      const to = requiredId("to");
      args = { issue_id: issueId, to };
      detail = `Move board issue ${issueId} to ${to}`;
      break;
    }
    case "board.issue.comment": {
      const issueId = requiredId("issue_id");
      const body = text(source, "body", { required: true }) as string;
      args = { issue_id: issueId, body };
      detail = `Comment on board issue ${issueId}: “${body.slice(0, 160)}${body.length > 160 ? "…" : ""}”`;
      break;
    }
    case "board.issue.delete": {
      const issueId = requiredId("issue_id");
      args = { issue_id: issueId };
      detail = `Permanently delete board issue ${issueId}`;
      break;
    }

    case "pipeline.task.create": {
      const task = pipelineFields(source);
      task.title = text(source, "title", { required: true, max: 500 });
      task.bot = requiredId("bot");
      optional(task, "start", bool(source, "start"));
      optional(task, "upsert", bool(source, "upsert"));
      args = task;
      detail = `Create pipeline “${task.title}” with bot ${task.bot}`;
      break;
    }
    case "pipeline.task.update": {
      const taskId = requiredId("task_id");
      const patch = pipelineFields(source);
      if (Object.keys(patch).length === 0) throw new Error("No pipeline fields to update");
      args = { task_id: taskId, patch };
      detail = `Update pipeline task ${taskId}`;
      break;
    }
    case "pipeline.task.ready": {
      const taskId = requiredId("task_id");
      const ready = bool(source, "ready");
      if (ready === undefined) throw new Error("Missing ready");
      args = { task_id: taskId, ready };
      detail = `${ready ? "Mark" : "Unmark"} pipeline task ${taskId} ${ready ? "ready" : "as ready"}`;
      break;
    }
    case "pipeline.task.launch":
    case "pipeline.task.reset":
    case "pipeline.task.close":
    case "pipeline.task.delete": {
      const taskId = requiredId("task_id");
      args = { task_id: taskId };
      const parts = request.id.split(".");
      const verb = parts[parts.length - 1] ?? "execute";
      detail = `${verb.charAt(0).toUpperCase()}${verb.slice(1)} pipeline task ${taskId}`;
      break;
    }

    case "run.launch": {
      const bot = requiredId("bot");
      args = { bot };
      optional(args, "vars", stringMap(source, "vars"));
      optional(args, "preset", text(source, "preset", { max: 256 }));
      detail = `Launch catalog bot ${bot}`;
      break;
    }
    case "run.pause":
    case "run.resume":
    case "run.cancel":
    case "run.delete": {
      const runId = requiredId("run_id");
      args = { run_id: runId };
      const parts = request.id.split(".");
      const verb = parts[parts.length - 1] ?? "execute";
      detail = `${verb.charAt(0).toUpperCase()}${verb.slice(1)} run ${runId}`;
      break;
    }
    case "run.rename": {
      const runId = requiredId("run_id");
      const name = text(source, "name", { required: true, max: 160 }) as string;
      args = { run_id: runId, name };
      detail = `Rename run ${runId} to “${name}”`;
      break;
    }

    case "dispatcher.start":
    case "dispatcher.pause":
    case "dispatcher.resume":
    case "dispatcher.stop":
      detail = actionDefinition(request.id).description;
      break;

    case "bot.create": {
      const slug = text(source, "slug", { required: true, max: 128 }) as string;
      if (!/^[a-z0-9][a-z0-9_-]*$/.test(slug)) throw new Error("Invalid bot slug");
      const instructions = text(source, "instructions", { required: true }) as string;
      args = { slug, instructions };
      for (const key of ["display_name", "icon", "description", "when_to_use", "model", "backend", "permission", "max_duration"] as const) {
        optional(args, key, text(source, key, { max: key === "description" || key === "when_to_use" ? 20_000 : 512 }));
      }
      optional(args, "skills", strings(source, "skills"));
      optional(args, "capabilities", strings(source, "capabilities"));
      optional(args, "worktree", bool(source, "worktree"));
      optional(args, "sandbox", bool(source, "sandbox"));
      optional(args, "max_cost_usd", numberValue(source, "max_cost_usd"));
      detail = `Create bot bundle ${slug}`;
      break;
    }
    case "bot.install": {
      const url = text(source, "url", { required: true, max: 4_096 }) as string;
      args = { url };
      optional(args, "ref", text(source, "ref", { max: 512 }));
      optional(args, "path", text(source, "path", { max: 2_048 }));
      optional(args, "name", text(source, "name", { max: 128 }));
      optional(args, "force", bool(source, "force"));
      detail = `Install bot from ${url}`;
      break;
    }
    case "plugin.enable":
    case "plugin.disable":
    case "plugin.uninstall": {
      const name = requiredId("name");
      args = { name };
      detail = `${request.id.endsWith("enable") ? "Enable" : request.id.endsWith("disable") ? "Disable" : "Uninstall"} plugin ${name}`;
      break;
    }
    case "plugin.install": {
      const sourceValue = text(source, "source", { required: true, max: 4_096 }) as string;
      args = { source: sourceValue };
      detail = `Install plugin from ${sourceValue}`;
      break;
    }
  }

  return {
    ...request,
    args,
    definition: actionDefinition(request.id),
    title,
    detail,
  };
}

export async function executeAssistantAction(
  request: ValidatedAssistantActionRequest,
  context: { assistantRunId?: string | null } = {},
): Promise<AssistantActionResult> {
  const args = request.args as UnknownRecord;
  const id = (key: string) => args[key] as string;
  switch (request.id) {
    case "editor.apply":
    case "editor.save":
      throw new Error("Editor actions require a live editor session");
    case "board.issue.create": {
      const issue = await nativeApi.createIssue(args as unknown as nativeApi.NativeIssueCreate);
      if (context.assistantRunId && issue.state === "ready") {
        await runsApi.addWatch(context.assistantRunId, issue.id).catch(() => undefined);
      }
      return { message: `Created board issue ${issue.id}`, href: "/board", hrefLabel: "Open board" };
    }
    case "board.issue.update": {
      await nativeApi.patchIssue(id("issue_id"), args.patch as nativeApi.NativeIssuePatch);
      return { message: `Updated board issue ${id("issue_id")}`, href: "/board", hrefLabel: "Open board" };
    }
    case "board.issue.transition":
      await nativeApi.transitionIssue(id("issue_id"), id("to"));
      if (context.assistantRunId && id("to") === "ready") {
        await runsApi.addWatch(context.assistantRunId, id("issue_id")).catch(() => undefined);
      }
      return { message: `Moved board issue ${id("issue_id")} to ${id("to")}` };
    case "board.issue.comment":
      await nativeApi.commentIssue(id("issue_id"), id("body"));
      return { message: `Commented on board issue ${id("issue_id")}` };
    case "board.issue.delete":
      await nativeApi.deleteIssue(id("issue_id"));
      return { message: `Deleted board issue ${id("issue_id")}` };
    case "pipeline.task.create": {
      const issue = await pipelineApi.createPipelineTask(args as unknown as pipelineApi.CreatePipelineTaskInput);
      if (context.assistantRunId && args.start === true) {
        await runsApi.addWatch(context.assistantRunId, issue.id).catch(() => undefined);
      }
      return { message: `Created pipeline task ${issue.id}`, href: `/pipelines/cards/issue/${encodeURIComponent(issue.id)}`, hrefLabel: "Open pipeline" };
    }
    case "pipeline.task.update":
      await pipelineApi.updatePipelineTask(id("task_id"), args.patch as pipelineApi.PipelineTaskPatch);
      return { message: `Updated pipeline task ${id("task_id")}` };
    case "pipeline.task.ready":
      await pipelineApi.markPipelineTaskReady(id("task_id"), args.ready as boolean);
      return { message: `${args.ready ? "Marked" : "Unmarked"} pipeline task ${id("task_id")} ${args.ready ? "ready" : "as ready"}` };
    case "pipeline.task.launch":
      await pipelineApi.launchPipelineTask(id("task_id"));
      return { message: `Launched pipeline task ${id("task_id")}` };
    case "pipeline.task.reset":
      await pipelineApi.resetPipelineTask(id("task_id"));
      return { message: `Reset pipeline task ${id("task_id")}` };
    case "pipeline.task.close":
      await pipelineApi.closePipelineTask(id("task_id"));
      return { message: `Closed pipeline task ${id("task_id")}` };
    case "pipeline.task.delete":
      await pipelineApi.deletePipelineTask(id("task_id"));
      return { message: `Deleted pipeline task ${id("task_id")}` };
    case "run.launch": {
      const bot = await botsApi.getBot(id("bot"));
      const launched = await runsApi.createRun({
        file_path: bot.path,
        bot_id: bot.name,
        ...(args.vars ? { vars: args.vars as Record<string, string> } : {}),
        ...(args.preset ? { preset: id("preset") } : {}),
      });
      return { message: `Launched ${bot.display_name || bot.name}`, href: `/runs/${encodeURIComponent(launched.run_id)}`, hrefLabel: "Open run" };
    }
    case "run.pause":
      await runsApi.pauseRun(id("run_id"));
      return { message: `Paused run ${id("run_id")}` };
    case "run.resume":
      await runsApi.resumeRun(id("run_id"));
      return { message: `Resumed run ${id("run_id")}`, href: `/runs/${encodeURIComponent(id("run_id"))}`, hrefLabel: "Open run" };
    case "run.cancel":
      await runsApi.cancelRun(id("run_id"));
      return { message: `Cancelled run ${id("run_id")}` };
    case "run.rename":
      await runsApi.renameRun(id("run_id"), id("name"));
      return { message: `Renamed run ${id("run_id")}` };
    case "run.delete":
      await runsApi.deleteRun(id("run_id"));
      return { message: `Deleted run ${id("run_id")}` };
    case "dispatcher.start":
      await dispatcherApi.start();
      return { message: "Started the dispatcher" };
    case "dispatcher.pause":
      await dispatcherApi.pause();
      return { message: "Paused the dispatcher" };
    case "dispatcher.resume":
      await dispatcherApi.resume();
      return { message: "Resumed the dispatcher" };
    case "dispatcher.stop":
      await dispatcherApi.stop();
      return { message: "Stopped the dispatcher" };
    case "bot.create": {
      const bot = await botsApi.createBot(args as unknown as botsApi.BotCreateSpec);
      return { message: `Created bot ${bot.display_name || bot.name}`, href: `/editor?file=${encodeURIComponent(bot.rel_path ? `${bot.rel_path}/main.bot` : bot.path)}`, hrefLabel: "Open bot" };
    }
    case "bot.install": {
      const installed = await botsApi.installBot(args as unknown as botsApi.InstallBotRequest);
      return { message: `Installed bot ${installed.name}`, href: `/bots`, hrefLabel: "Open bots" };
    }
    case "plugin.enable":
      await pluginsApi.setPluginEnabled(id("name"), true);
      return { message: `Enabled plugin ${id("name")}` };
    case "plugin.disable":
      await pluginsApi.setPluginEnabled(id("name"), false);
      return { message: `Disabled plugin ${id("name")}` };
    case "plugin.install": {
      const installed = await pluginsApi.installPlugin(id("source"));
      return { message: `Installed plugin ${installed.name}`, href: "/plugins", hrefLabel: "Open plugins" };
    }
    case "plugin.uninstall":
      await pluginsApi.uninstallPlugin(id("name"));
      return { message: `Uninstalled plugin ${id("name")}` };
  }
}
