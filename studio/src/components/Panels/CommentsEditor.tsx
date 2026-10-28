import { useCallback } from "react";
import { useDocumentStore } from "@/store/document";
import { Button } from "@/components/ui/Button";
import { Textarea } from "@/components/ui/Textarea";

export default function CommentsEditor() {
  const document = useDocumentStore((s) => s.document);
  const addComment = useDocumentStore((s) => s.addComment);
  const removeComment = useDocumentStore((s) => s.removeComment);
  const updateComment = useDocumentStore((s) => s.updateComment);

  const comments = document?.comments ?? [];

  const handleAdd = useCallback(() => {
    addComment({ text: "" });
  }, [addComment]);

  return (
    <div className="p-3 text-sm">
      <div className="flex items-center justify-between mb-3">
        <h2 className="font-bold text-fg-muted">Comments</h2>
        <Button
          variant="primary"
          size="sm"
          onClick={handleAdd}
          disabled={!document}
        >
          + New
        </Button>
      </div>
      {comments.length === 0 && <p className="text-fg-subtle text-xs">No comments defined.</p>}
      {comments.map((comment, i) => (
        <div key={i} className="mb-3 p-2 bg-surface-1 rounded border border-border-default">
          <div className="flex items-center justify-between mb-1">
            <span className="text-xs text-fg-subtle">## Comment {i + 1}</span>
            {/* Sibling cards repeat the visible "Delete"; the comment
                position is what a screen reader needs to tell them apart. */}
            <Button
              variant="ghost"
              size="sm"
              className="text-danger hover:text-danger-fg"
              aria-label={`Delete comment ${i + 1}`}
              onClick={() => removeComment(i)}
            >
              Delete
            </Button>
          </div>
          {/* The placeholder is not a name: the card's position names the
              field, matching the "## Comment N" header above it. */}
          <Textarea
            value={comment.text}
            onChange={(e) => updateComment(i, e.target.value)}
            rows={2}
            placeholder="Comment text..."
            aria-label={`Comment ${i + 1} text`}
          />
        </div>
      ))}
    </div>
  );
}
