// A name the .bot syntax reads, as its lexer scans one (pkg/dsl/parser,
// isIdentStart / isIdentPart): a letter or _, then letters, digits and _.
// Letters and digits are Unicode's, as Go's unicode.IsLetter / IsDigit.
// The writer puts every node, loop, schema field, vars field, prompt and
// workflow name down bare, so a name outside this shape — a space, a
// hyphen, a quote — tears the saved file apart.
const DSL_NAME = /^[\p{L}_][\p{L}\p{Nd}_]*$/u;

export function isDslName(name: string): boolean {
  return DSL_NAME.test(name);
}

/**
 * dslNameError is the message a name field shows for a name the .bot syntax
 * does not read, with the spelling that works; null when the name is fine.
 * Empty names are the caller's to word (some fields allow none).
 */
export function dslNameError(name: string): string | null {
  if (name === "" || isDslName(name)) return null;
  return `Use letters, digits and _ only, starting with a letter or _ — e.g. ${suggestDslName(name)}`;
}

/** suggestDslName turns every character a name cannot hold into _. */
export function suggestDslName(name: string): string {
  let out = "";
  for (const ch of name) {
    if (/[\p{L}\p{Nd}_]/u.test(ch)) {
      out += out === "" && /\p{Nd}/u.test(ch) ? `_${ch}` : ch;
    } else {
      out += "_";
    }
  }
  return out || "_";
}
