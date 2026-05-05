import ApiKeysEditor from "./ApiKeysEditor";

export default function ApiKeysTab() {
  return (
    <div className="flex flex-col gap-3 p-4">
      <p className="text-xs text-fg-subtle">
        Stored in your OS keychain. Existing shell environment variables of
        the same name take precedence — Iterion never overwrites them.
      </p>
      <ApiKeysEditor />
    </div>
  );
}
