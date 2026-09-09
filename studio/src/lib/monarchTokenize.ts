// Test-only driver for monaco's REAL Monarch engine: compiles a language
// definition with the compiler the editor uses and tokenizes lines with the
// same tokenizer class, so a highlighting assertion certifies what the editor
// paints, not a re-implementation of its rules. Kept outside the app bundle
// (imported by tests only); the services the tokenizer asks for are the
// minimum it touches on the classic (non-encoded) path.
import type { languages } from "monaco-editor";
// eslint-disable-next-line @typescript-eslint/ban-ts-comment
// @ts-ignore — monaco ships these ESM internals without type declarations
import { compile } from "monaco-editor/editor/standalone/common/monarch/monarchCompile.js";
// eslint-disable-next-line @typescript-eslint/ban-ts-comment
// @ts-ignore
import { MonarchTokenizer } from "monaco-editor/editor/standalone/common/monarch/monarchLexer.js";

export interface LineToken {
  offset: number;
  type: string;
}

export function tokenizeLines(languageId: string, def: languages.IMonarchLanguage, lines: string[]): LineToken[][] {
  const lexer = compile(languageId, def);
  const languageService = {
    languageIdCodec: { encodeLanguageId: () => 1 },
    getLanguageIdByLanguageName: () => null,
    getLanguageIdByMimeType: () => null,
    isRegisteredLanguageId: () => false,
    requestBasicLanguageFeatures: () => undefined,
  };
  const themeService = { getColorTheme: () => ({ tokenTheme: { match: () => 0 } }) };
  const configurationService = {
    getValue: () => 20000,
    onDidChangeConfiguration: () => ({ dispose: () => undefined }),
  };
  const tokenizer = new MonarchTokenizer(languageService, themeService, languageId, lexer, configurationService);
  let state = tokenizer.getInitialState();
  const out: LineToken[][] = [];
  for (const line of lines) {
    const result = tokenizer.tokenize(line, true, state);
    out.push(result.tokens.map((t: { offset: number; type: string }) => ({ offset: t.offset, type: t.type })));
    state = result.endState;
  }
  return out;
}

// typeAt returns the token type covering column `col` of a tokenized line
// ("" for a line the input did not have).
export function typeAt(tokens: LineToken[] | undefined, col: number): string {
  let type = "";
  for (const t of tokens ?? []) {
    if (t.offset <= col) {
      type = t.type;
    }
  }
  return type;
}
