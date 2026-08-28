import { DiffEditor } from "@monaco-editor/react";

import type { AssistantAuthoringPreviewFile } from "@/api/assistantAuthoring";
import { Dialog } from "@/components/ui";
import { inferMonacoLanguage } from "@/lib/inferMonacoLanguage";
import { useThemeStore } from "@/store/theme";

export default function AssistantTextDiffDialog({
  file,
  onClose,
}: {
  file: AssistantAuthoringPreviewFile | null;
  onClose: () => void;
}) {
  const theme = useThemeStore((state) => state.resolved);
  const title = file ? `${file.scope}:${file.path}` : "Assistant file change";
  return (
    <Dialog
      open={file !== null}
      onOpenChange={(open) => {
        if (!open) onClose();
      }}
      title={title}
      description="Exact replacement preview — no file has been written"
      widthClass="max-w-[90vw] w-[90vw]"
    >
      <div className="h-[75vh] -mx-4 -my-3">
        {file && (
          <DiffEditor
            theme={theme === "dark" ? "vs-dark" : "vs"}
            language={inferMonacoLanguage(file.path)}
            original={file.before}
            modified={file.after}
            options={{
              readOnly: true,
              renderSideBySide: true,
              ignoreTrimWhitespace: false,
              automaticLayout: true,
              minimap: { enabled: false },
              scrollBeyondLastLine: false,
            }}
          />
        )}
      </div>
    </Dialog>
  );
}
