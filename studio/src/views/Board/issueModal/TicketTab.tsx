import {
  type NativeBoard,
  type NativeIssue,
} from "@/api/native";
import { Checkbox } from "@/components/ui/Checkbox";
import { Combobox } from "@/components/ui/Combobox";
import { Input } from "@/components/ui/Input";
import { MarkdownPreview } from "@/components/ui/MarkdownPreview";
import { Select } from "@/components/ui/Select";
import { TagInput } from "@/components/ui/TagInput";

import { Field } from "./Field";
import { LastRunSection } from "./LastRunSection";
import { PullRequestsSection } from "./PullRequestsSection";

interface TicketTabProps {
  board: NativeBoard;
  initial: NativeIssue | null;
  title: string;
  setTitle: (v: string) => void;
  body: string;
  setBody: (v: string) => void;
  state: string;
  setState: (v: string) => void;
  priority: number;
  setPriority: (v: number) => void;
  labels: string[];
  setLabels: (v: string[]) => void;
  assignee: string;
  setAssignee: (v: string) => void;
  allAssignees: string[];
  fields: Record<string, string>;
  setFields: (v: Record<string, string>) => void;
  // Pre-built repo-first scoping picker/note; IssueModal owns the state
  // (uses useActiveRepo + issue.external) and passes down a ready node.
  // Undefined outside cloud mode (or with zero connected repos).
  repositoryField?: React.ReactNode;
}

export function TicketTab({
  board,
  initial,
  title,
  setTitle,
  body,
  setBody,
  state,
  setState,
  priority,
  setPriority,
  labels,
  setLabels,
  assignee,
  setAssignee,
  allAssignees,
  fields,
  setFields,
  repositoryField,
}: TicketTabProps) {
  return (
    <div className="space-y-3 py-3">
      <Field label="Title" required>
        <Input
          autoFocus
          value={title}
          onChange={(e) => setTitle(e.target.value)}
          size="md"
          required
        />
      </Field>

      <Field label="Body">
        <MarkdownPreview
          value={body}
          onChange={setBody}
          rows={12}
          editorClassName="max-h-[50vh]"
          placeholder="Add context, repro steps, or notes…"
        />
      </Field>

      <div className="grid grid-cols-2 gap-3">
        <Field label="State">
          <Select
            value={state}
            onChange={(e) => setState(e.target.value)}
            size="md"
            disabled={!!initial /* edits go through transition for the audit log */}
          >
            {board.states.map((s) => (
              <option key={s.name} value={s.name}>
                {s.display ?? s.name}
              </option>
            ))}
          </Select>
          {initial && (
            <p className="text-xs text-fg-muted mt-1">
              Drag the card to move between states.
            </p>
          )}
        </Field>
        <Field
          label="Priority"
          help="Higher numbers rank higher — columns sort by priority descending (presets go up to P30). 0 = unprioritized."
        >
          <Input
            type="number"
            value={priority}
            onChange={(e) => setPriority(parseInt(e.target.value || "0", 10))}
            size="md"
            leadingIcon={<span className="text-xs font-medium">P</span>}
            title="Priority — higher numbers sort first; 0 = unprioritized"
          />
        </Field>
      </div>

      <div className="grid grid-cols-2 gap-3">
        <Field label="Labels">
          <TagInput value={labels} onChange={setLabels} placeholder="urgent, infra…" />
        </Field>
        <Field label="Assignee">
          <Combobox
            value={assignee}
            options={allAssignees.map((a) => ({ value: a, label: `@${a}` }))}
            onChange={(v) => setAssignee(v)}
            placeholder="Search or type a name…"
            size="md"
            freeSolo
          />
        </Field>
      </div>

      {repositoryField}

      {initial &&
        (initial.runs?.length || initial.last_run_id || initial.last_workdir) && (
          <LastRunSection
            runID={initial.last_run_id}
            workdir={initial.last_workdir}
            runs={initial.runs}
          />
        )}

      {initial && <PullRequestsSection issue={initial} />}

      {(board.fields ?? []).map((f) => (
        <Field key={f.name} label={(f.display ?? f.name) + ` (${f.type})`}>
          {f.type === "enum" ? (
            <Select
              value={fields[f.name] ?? ""}
              onChange={(e) =>
                setFields({ ...fields, [f.name]: e.target.value })
              }
              size="md"
            >
              <option value="">(unset)</option>
              {(f.enum_values ?? []).map((v) => (
                <option key={v} value={v}>
                  {v}
                </option>
              ))}
            </Select>
          ) : f.type === "bool" ? (
            <label className="inline-flex items-center gap-2">
              <Checkbox
                checked={fields[f.name] === "true"}
                onChange={(e) =>
                  setFields({
                    ...fields,
                    [f.name]: e.target.checked ? "true" : "false",
                  })
                }
              />
              <span className="text-xs text-fg-muted">
                {fields[f.name] === "true" ? "true" : "false"}
              </span>
            </label>
          ) : (
            <Input
              type={
                f.type === "number"
                  ? "number"
                  : f.type === "date"
                    ? "datetime-local"
                    : "text"
              }
              value={fields[f.name] ?? ""}
              onChange={(e) =>
                setFields({ ...fields, [f.name]: e.target.value })
              }
              size="md"
              required={f.required}
            />
          )}
        </Field>
      ))}
    </div>
  );
}
