// Extracted from BotBuilder/index.tsx to keep that file focused.
// Small heading shared by the builder's form-section cards.

export default function SectionTitle({ children }: { children: React.ReactNode }) {
  return <h2 className="text-xs font-semibold text-fg-default">{children}</h2>;
}
