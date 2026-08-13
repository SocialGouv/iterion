import { ChevronDownIcon, ChevronRightIcon } from "@radix-ui/react-icons";

/** Enough to read a short plan; two files plus the answer form still fit. */
export const COLLAPSE_AFTER_LINES = 12;
export const COLLAPSED_MAX_HEIGHT = "16rem";

/** Folded markdown skips react-markdown past this; also the json/text DOM slice. */
export const MARKDOWN_PARSE_BUDGET = 20_000;

/** Do not blob.text() / line-split an attachment larger than this.
 *  Inlined JSON is always pretty-printed; UTF-8 length cannot exceed this. */
export const TEXT_PREVIEW_BYTE_BUDGET = 2_000_000;

export function Toggle({
  open,
  onToggle,
  closedLabel,
}: {
  open: boolean;
  onToggle: () => void;
  closedLabel: string;
}) {
  return (
    <button
      type="button"
      onClick={onToggle}
      aria-expanded={open}
      className="inline-flex items-center gap-0.5 text-micro text-accent-text hover:underline"
    >
      {open ? <ChevronDownIcon /> : <ChevronRightIcon />}
      {open ? "Show less" : closedLabel}
    </button>
  );
}
