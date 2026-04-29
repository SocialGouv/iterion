import type { ReactNode } from "react";

export default function ToolbarGroup({ children }: { children: ReactNode }) {
  return (
    <>
      <div className="flex items-center gap-1">{children}</div>
      <div className="h-4 w-px bg-border-default mx-1" />
    </>
  );
}
