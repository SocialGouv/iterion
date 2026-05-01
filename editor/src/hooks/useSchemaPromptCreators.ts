import { useCallback } from "react";

import { useDocumentStore } from "@/store/document";
import { defaultPrompt, defaultSchema } from "@/lib/defaults";

/** Returns memoised "Create schema" / "Create prompt" callbacks shared
 *  by every node form. They mint a unique placeholder name (`schema_N` /
 *  `prompt_N`), append the empty declaration to the document, and
 *  return the chosen name so the caller can wire it as the value of a
 *  SelectFieldWithCreate. */
export function useSchemaPromptCreators(): {
  createSchema: () => string;
  createPrompt: () => string;
} {
  const document = useDocumentStore((s) => s.document);
  const addSchema = useDocumentStore((s) => s.addSchema);
  const addPrompt = useDocumentStore((s) => s.addPrompt);

  const createSchema = useCallback(() => {
    const existing = new Set((document?.schemas ?? []).map((s) => s.name));
    let i = 1;
    while (existing.has(`schema_${i}`)) i++;
    const name = `schema_${i}`;
    addSchema(defaultSchema(name));
    return name;
  }, [document, addSchema]);

  const createPrompt = useCallback(() => {
    const existing = new Set((document?.prompts ?? []).map((p) => p.name));
    let i = 1;
    while (existing.has(`prompt_${i}`)) i++;
    const name = `prompt_${i}`;
    addPrompt(defaultPrompt(name));
    return name;
  }, [document, addPrompt]);

  return { createSchema, createPrompt };
}
