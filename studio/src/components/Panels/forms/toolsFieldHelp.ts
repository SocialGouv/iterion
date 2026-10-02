/**
 * The three states of an agent/judge `tools:` list, named.
 *
 * `tools: []` is a VALUE — the author declaring the node has no tools — and
 * an absent `tools:` line is not. The two used to be the same thing, so the
 * field could render one caption; now removing the last chip changes what the
 * node can do, and nothing else on screen says so.
 *
 * Kept out of AgentForm.tsx so it can be tested: that component pulls
 * `@lobehub/ui`'s Tooltip, which fails to resolve `@base-ui/react/merge-props`
 * under vitest in this tree.
 */
export function toolsFieldHelp(tools: string[] | undefined): string {
  if (tools === undefined) {
    return "Undeclared: the node runs with the backend's own toolset (zero tools on claw). Adding a tool declares the list.";
  }
  if (tools.length === 0) {
    return "Declared empty (`tools: []`): this node runs with NO tools. It is a value, not an absence — remove the line in the source editor to go back to undeclared.";
  }
  return "Declared: only these tools. Removing the last one declares an EMPTY list (no tools), not an absent one.";
}
