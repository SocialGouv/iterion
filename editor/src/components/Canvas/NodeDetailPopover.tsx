import { useCallback } from "react";
import type { IterDocument, AgentDecl, JudgeDecl, HumanDecl, ToolNodeDecl, JoinDecl, RouterDecl } from "@/api/types";
import { useUIStore } from "@/store/ui";

interface Props {
  nodeId: string;
  document: IterDocument;
  onClose: () => void;
}

export default function NodeDetailPopover({ nodeId, document, onClose }: Props) {
  const setActiveTab = useUIStore((s) => s.setActiveTab);
  const setEditingItem = useUIStore((s) => s.setEditingItem);

  // Find the node declaration
  const agent = document.agents?.find((a) => a.name === nodeId) as AgentDecl | undefined;
  const judge = document.judges?.find((j) => j.name === nodeId) as JudgeDecl | undefined;
  const human = document.humans?.find((h) => h.name === nodeId) as HumanDecl | undefined;
  const tool = document.tools?.find((t) => t.name === nodeId) as ToolNodeDecl | undefined;
  const join = document.joins?.find((j) => j.name === nodeId) as JoinDecl | undefined;
  const router = document.routers?.find((r) => r.name === nodeId) as RouterDecl | undefined;

  const decl = agent || judge || human || tool || join || router;
  if (!decl) return null;

  const kind = agent ? "agent" : judge ? "judge" : human ? "human" : tool ? "tool" : join ? "join" : "router";

  // Resolve schemas
  const inputSchemaName = (agent || judge || human)?.input;
  const outputSchemaName = (agent || judge || human || tool || join)?.output;
  const inputSchema = inputSchemaName ? document.schemas?.find((s) => s.name === inputSchemaName) : undefined;
  const outputSchema = outputSchemaName ? document.schemas?.find((s) => s.name === outputSchemaName) : undefined;

  // Resolve prompts
  const systemPromptName = (agent || judge)?.system ?? (router as RouterDecl)?.system;
  const userPromptName = (agent || judge)?.user ?? (router as RouterDecl)?.user;
  const instructionsName = (human as HumanDecl)?.instructions;
  const systemPrompt = systemPromptName ? document.prompts?.find((p) => p.name === systemPromptName) : undefined;
  const userPrompt = userPromptName ? document.prompts?.find((p) => p.name === userPromptName) : undefined;
  const instructionsPrompt = instructionsName ? document.prompts?.find((p) => p.name === instructionsName) : undefined;

  // Tools
  const tools = (agent || judge)?.tools ?? [];

  // Model / delegate
  const model = (agent || judge)?.model ?? (router as RouterDecl)?.model;
  const delegate = (agent || judge)?.delegate;
  const session = (agent || judge)?.session;

  const navigateTo = useCallback((tab: "properties") => {
    setActiveTab(tab);
    onClose();
  }, [setActiveTab, onClose]);

  return (
    <div className="fixed inset-0 z-50" onClick={onClose}>
      <div
        className="absolute top-1/2 left-1/2 -translate-x-1/2 -translate-y-1/2 bg-gray-850 border border-gray-600 rounded-xl shadow-2xl w-[400px] max-h-[500px] overflow-y-auto"
        style={{ background: "#1a1e2e" }}
        onClick={(e) => e.stopPropagation()}
      >
        {/* Header */}
        <div className="flex items-center justify-between px-4 py-3 border-b border-gray-700">
          <div>
            <div className="font-semibold text-white text-sm">{nodeId}</div>
            <div className="text-xs text-gray-400 flex items-center gap-2">
              <span className="capitalize">{kind}</span>
              {model && <span className="text-gray-500">{model.replace(/\$\{.*?\}/g, "env")}</span>}
              {delegate && <span className="text-blue-400">{delegate}</span>}
            </div>
          </div>
          <button className="text-gray-400 hover:text-white text-lg px-1" onClick={onClose} title="Close (Esc)">
            &times;
          </button>
        </div>

        <div className="px-4 py-3 space-y-3">
          {/* I/O Schemas */}
          {(inputSchema || outputSchema) && (
            <SectionHeader title="Schemas" />
          )}
          {inputSchema && (
            <div className="mb-2">
              <div className="flex items-center justify-between mb-0.5">
                <div className="text-[10px] text-blue-400 uppercase tracking-wider">Input: {inputSchema.name}</div>
                <button className="text-[10px] text-blue-400 hover:text-blue-300" onClick={() => setEditingItem({ kind: "schema", name: inputSchema.name })}>Edit</button>
              </div>
              <div className="grid grid-cols-2 gap-x-2 gap-y-0.5">
                {inputSchema.fields.map((f) => (
                  <div key={f.name} className="text-xs text-gray-300">
                    <span className="text-gray-500">{f.type}</span>{" "}
                    <span>{f.name}</span>
                    {f.enum_values && <span className="text-gray-600 ml-1">[{f.enum_values.join(", ")}]</span>}
                  </div>
                ))}
              </div>
            </div>
          )}
          {outputSchema && (
            <div className="mb-2">
              <div className="flex items-center justify-between mb-0.5">
                <div className="text-[10px] text-green-400 uppercase tracking-wider">Output: {outputSchema.name}</div>
                <button className="text-[10px] text-blue-400 hover:text-blue-300" onClick={() => setEditingItem({ kind: "schema", name: outputSchema.name })}>Edit</button>
              </div>
              <div className="grid grid-cols-2 gap-x-2 gap-y-0.5">
                {outputSchema.fields.map((f) => (
                  <div key={f.name} className="text-xs text-gray-300">
                    <span className="text-gray-500">{f.type}</span>{" "}
                    <span>{f.name}</span>
                    {f.enum_values && <span className="text-gray-600 ml-1">[{f.enum_values.join(", ")}]</span>}
                  </div>
                ))}
              </div>
            </div>
          )}

          {/* Prompts */}
          {(systemPrompt || userPrompt || instructionsPrompt) && (
            <SectionHeader title="Prompts" />
          )}
          {systemPrompt && (
            <PromptPreview label="system" name={systemPrompt.name} body={systemPrompt.body} onEdit={() => setEditingItem({ kind: "prompt", name: systemPrompt.name })} />
          )}
          {userPrompt && (
            <PromptPreview label="user" name={userPrompt.name} body={userPrompt.body} onEdit={() => setEditingItem({ kind: "prompt", name: userPrompt.name })} />
          )}
          {instructionsPrompt && (
            <PromptPreview label="instructions" name={instructionsPrompt.name} body={instructionsPrompt.body} onEdit={() => setEditingItem({ kind: "prompt", name: instructionsPrompt.name })} />
          )}

          {/* Tools */}
          {tools.length > 0 && (
            <Section title="Tools" onEdit={() => navigateTo("properties")}>
              <div className="flex flex-wrap gap-1">
                {tools.map((t) => (
                  <span key={t} className="text-xs bg-gray-700 text-gray-300 px-1.5 py-0.5 rounded">{t}</span>
                ))}
              </div>
            </Section>
          )}

          {/* Config */}
          {(session || (agent || judge)?.tool_max_steps || (agent || judge)?.publish) && (
            <Section title="Config" onEdit={() => navigateTo("properties")}>
              <div className="flex flex-wrap gap-2 text-xs text-gray-400">
                {session && <span>session: <span className="text-gray-300">{session}</span></span>}
                {(agent || judge)?.tool_max_steps && (
                  <span>max steps: <span className="text-gray-300">{(agent || judge)!.tool_max_steps}</span></span>
                )}
                {(agent || judge)?.publish && (
                  <span>publish: <span className="text-gray-300">{(agent || judge)!.publish}</span></span>
                )}
              </div>
            </Section>
          )}

          {/* Join details */}
          {join && (
            <Section title="Join Config" onEdit={() => navigateTo("properties")}>
              <div className="text-xs text-gray-400">
                <span>strategy: <span className="text-gray-300">{join.strategy}</span></span>
                {join.require?.length > 0 && (
                  <div className="mt-1">require: <span className="text-gray-300">{join.require.join(", ")}</span></div>
                )}
              </div>
            </Section>
          )}

          {/* Router details */}
          {router && (
            <Section title="Router Config" onEdit={() => navigateTo("properties")}>
              <div className="text-xs text-gray-400">
                <span>mode: <span className="text-gray-300">{router.mode}</span></span>
                {router.multi && <span className="ml-2">multi: <span className="text-gray-300">true</span></span>}
              </div>
            </Section>
          )}

          {/* Tool command */}
          {tool && (
            <Section title="Command" onEdit={() => navigateTo("properties")}>
              <code className="text-xs text-green-300 bg-gray-800 px-2 py-1 rounded block">{tool.command}</code>
            </Section>
          )}

          {/* Human details */}
          {human && (
            <Section title="Human Config" onEdit={() => navigateTo("properties")}>
              <div className="text-xs text-gray-400">
                <span>mode: <span className="text-gray-300">{human.mode}</span></span>
                {human.min_answers && <span className="ml-2">min answers: <span className="text-gray-300">{human.min_answers}</span></span>}
              </div>
            </Section>
          )}
        </div>
      </div>
    </div>
  );
}

function SectionHeader({ title }: { title: string }) {
  return (
    <div className="text-[10px] text-gray-500 uppercase tracking-wider font-medium mb-1">{title}</div>
  );
}

function Section({ title, onEdit, children }: { title: string; onEdit: () => void; children: React.ReactNode }) {
  return (
    <div>
      <div className="flex items-center justify-between mb-1">
        <div className="text-[10px] text-gray-500 uppercase tracking-wider font-medium">{title}</div>
        <button
          className="text-[10px] text-blue-400 hover:text-blue-300"
          onClick={onEdit}
        >
          Edit
        </button>
      </div>
      {children}
    </div>
  );
}

function PromptPreview({ label, name, body, onEdit }: { label: string; name: string; body: string; onEdit?: () => void }) {
  const lines = body.split("\n").slice(0, 3);
  const preview = lines.join("\n");
  const truncated = body.split("\n").length > 3;

  return (
    <div className="mb-2">
      <div className="flex items-center justify-between mb-0.5">
        <div className="text-[10px] text-teal-400">{label}: {name}</div>
        {onEdit && <button className="text-[10px] text-blue-400 hover:text-blue-300" onClick={onEdit}>Edit</button>}
      </div>
      <pre className="text-[10px] text-gray-400 bg-gray-800/50 rounded px-2 py-1 whitespace-pre-wrap max-h-[60px] overflow-hidden">
        {preview}{truncated ? "\n..." : ""}
      </pre>
    </div>
  );
}
