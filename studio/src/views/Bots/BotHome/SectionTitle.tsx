// Extracted from BotHome/index.tsx to keep that file focused.
// Small heading shared by the bot-home cards; `flush` drops the padding
// that flush-Card sections otherwise need.

export default function SectionTitle({
  children,
  flush = false,
}: {
  children: React.ReactNode;
  flush?: boolean;
}) {
  return (
    <h2 className={`text-xs font-semibold text-fg-default ${flush ? "" : "px-4 pt-3 pb-1"}`}>
      {children}
    </h2>
  );
}
