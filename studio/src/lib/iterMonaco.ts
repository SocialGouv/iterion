import type { Monaco } from "@/lib/monaco";
import { ITER_LANGUAGE_ID, iterLanguageConfig, iterTokensProvider } from "@/lib/iterLanguage";
import { registerIterCompletionProvider } from "@/lib/iterMonacoCompletion";

/**
 * Teach a Monaco instance iterion's own DSL, then hand back the language id
 * to give an editor over `.bot` text.
 *
 * Every surface that edits `.bot` source calls this from `beforeMount`:
 * `ITER_LANGUAGE_ID` is not registered globally, so a surface that names the
 * id without registering it gets Monaco's silent fall-back to no tokenizer —
 * which looks exactly like plain text, only harder to notice.
 * `inferMonacoLanguage` keeps mapping `.bot` to plaintext for the dialogs
 * that show a file without registering anything.
 *
 * Idempotent: the registration is skipped when the id is already known, and
 * the completion provider guards itself.
 */
export function registerIterLanguage(monaco: Monaco): string {
  if (!monaco.languages.getLanguages().some((l: { id: string }) => l.id === ITER_LANGUAGE_ID)) {
    monaco.languages.register({ id: ITER_LANGUAGE_ID });
    monaco.languages.setLanguageConfiguration(ITER_LANGUAGE_ID, iterLanguageConfig);
    monaco.languages.setMonarchTokensProvider(ITER_LANGUAGE_ID, iterTokensProvider);
  }
  registerIterCompletionProvider(monaco);
  return ITER_LANGUAGE_ID;
}
