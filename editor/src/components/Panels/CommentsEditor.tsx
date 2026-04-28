import { useCallback } from "react";
import { useDocumentStore } from "@/store/document";

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
        <button
          className="bg-accent hover:bg-accent-hover text-xs px-2 py-1 rounded"
          onClick={handleAdd}
          disabled={!document}
        >
          + New
        </button>
      </div>
      {comments.length === 0 && <p className="text-fg-subtle text-xs">No comments defined.</p>}
      {comments.map((comment, i) => (
        <div key={i} className="mb-3 p-2 bg-surface-1 rounded border border-border-default">
          <div className="flex items-center justify-between mb-1">
            <span className="text-xs text-fg-subtle">## Comment {i + 1}</span>
            <button
              className="text-danger hover:text-danger-fg text-xs"
              onClick={() => removeComment(i)}
            >
              Delete
            </button>
          </div>
          <textarea
            className="w-full bg-surface-0 border border-border-strong rounded px-2 py-1 text-sm text-fg-default focus:border-accent focus:outline-none resize-y"
            value={comment.text}
            onChange={(e) => updateComment(i, e.target.value)}
            rows={2}
            placeholder="Comment text..."
          />
        </div>
      ))}
    </div>
  );
}
