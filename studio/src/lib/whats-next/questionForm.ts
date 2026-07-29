// QuestionForm types — primitives for AskUserQuestion-style human
// interaction inside the WhatsNext chat.
//
// v1 ships the type system + renderer primitives. The form spec is
// declared in `firstClassBots.ts.nodeMap[node].form` for now; future
// rounds will let upstream agent nodes emit a form payload (LLM-fill
// path) and add a `form:` block to the .bot DSL (statically-declared
// path) — both will produce the same FormSpec shape.

export type QuestionKind =
  | "radio"
  | "checkbox"
  | "select"
  | "free_text"
  | "file";

export interface QuestionOption {
  value: string;
  label: string;
  // Optional one-line help text shown next to the option.
  description?: string;
}

// Discriminated union: each kind carries the fields it needs.
export type FormQuestion = {
  // Stable id within the form. It is ALSO the human-node output
  // field name that the answer lands under in the answers object
  // submitted via resumeRun. Pick names that match the workflow's
  // output schema (e.g. "context", "approved", "feedback").
  id: string;
  label: string;
  description?: string;
  // Required questions block the form-level submit. Free-text
  // questions default to required:true unless explicitly opted out;
  // radio defaults to required:true; checkbox / select default to
  // required:false (treat absence as "none of the above").
  required?: boolean;
} & (
  | {
      kind: "radio";
      options: QuestionOption[];
      // When true, surface an "Other" option whose selection reveals
      // a free-text input. Mirrors Claude Code's AskUserQuestion
      // affordance.
      allow_other?: boolean;
      // Pre-selects this option value when the form first mounts.
      // Must match one of `options[].value`. Used by whats-next's
      // smart-default radio (next-likely action from the previous
      // loop) and the dispatch-just-created shortcut. Unset → the
      // form opens with no option selected (operator must pick).
      defaultValue?: string;
    }
  | {
      kind: "checkbox";
      options: QuestionOption[];
      allow_other?: boolean;
      // defaultValues pre-fills the checkbox group when the form
      // first mounts. Used for "validate by exception" UX — render
      // every option pre-ticked so Approve = "create all" and the
      // operator only interacts with items they want to drop. When
      // unset, the initial answer is the empty array (every option
      // unticked).
      defaultValues?: string[];
    }
  | {
      kind: "select";
      options: QuestionOption[];
      placeholder?: string;
    }
  | {
      kind: "free_text";
      placeholder?: string;
      // Default 3. Set to 1 for an Input rather than a Textarea.
      rows?: number;
    }
  | {
      kind: "file";
      // `accept` attribute for the picker (e.g. "audio/*"). Advisory
      // only — a browser hint, never a security control. The server
      // sniffs the real MIME and enforces its own allowlist.
      accept?: string;
      // Human-readable hint under the drop zone (e.g. "MP3 or WAV").
      hint?: string;
    }
);

export interface FormSpec {
  questions: FormQuestion[];
  // Optional override label for the submit button. Default: "Send".
  submitLabel?: string;
  // Rendering mode for the form. "auto" (default) paginates to a
  // wizard with Back/Next/Submit when there are 2+ questions; "flat"
  // always renders every question on one page with a single Submit.
  // Inline chat turns prefer "flat" to keep the operator's mental
  // model "one form, one submit"; the wizard is better suited to
  // /runs/<id>'s richer interaction panels.
  mode?: "auto" | "flat";
}

// FormAnswer maps a question id to its raw value. Single-value
// questions (radio, select, free_text) carry a string; multi-value
// questions (checkbox) carry a string array. The "Other" free-text
// is already merged in by the renderer — no separate `_other` keys
// in the output.
//
// A `file` question also carries a string: the staged upload id the
// renderer received after uploading the picked file. Keeping it a
// plain string is what lets file questions ride the existing
// answer/validation plumbing untouched; the submitting form wraps the
// id in an upload envelope on its way to the server.
export type FormAnswer = Record<string, string | string[]>;

// Sentinel value used internally by QuestionInput to flag the "Other"
// option in radio/checkbox lists. Exported so tests can reference it.
// Never appears in the final FormAnswer (the renderer swaps it for
// the matching free-text content before invoking onSubmit).
export const OTHER_SENTINEL = "__other__";
