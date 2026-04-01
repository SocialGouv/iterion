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
        <h2 className="font-bold text-gray-300">Comments</h2>
        <button
          className="bg-blue-600 hover:bg-blue-700 text-xs px-2 py-1 rounded"
          onClick={handleAdd}
          disabled={!document}
        >
          + New
        </button>
      </div>
      {comments.length === 0 && <p className="text-gray-500 text-xs">No comments defined.</p>}
      {comments.map((comment, i) => (
        <div key={i} className="mb-3 p-2 bg-gray-800 rounded border border-gray-700">
          <div className="flex items-center justify-between mb-1">
            <span className="text-xs text-gray-400">## Comment {i + 1}</span>
            <button
              className="text-red-400 hover:text-red-300 text-xs"
              onClick={() => removeComment(i)}
            >
              Delete
            </button>
          </div>
          <textarea
            className="w-full bg-gray-900 border border-gray-600 rounded px-2 py-1 text-sm text-white focus:border-blue-500 focus:outline-none resize-y"
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
