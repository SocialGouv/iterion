import { useModelCapabilities } from "@/hooks/useModelCapabilities";
import { useResolvedModel } from "@/hooks/useResolvedModel";
import { effectiveModel } from "@/lib/effectiveModel";
import { modelCapsTooltip } from "@/lib/modelCapsTooltip";

// The capability caption under a model picker: context window, max output,
// published price and where those came from.
//
// One component for all three pickers, because the hard part is not the
// rendering but agreeing on WHICH model is selected — see effectiveModel, and
// in particular the launch form's inherit path, where the selected model is
// the placeholder rather than the input value.
export function ModelCapsCaption({
  override,
  authored,
  className,
}: {
  // What the operator typed into this picker, if anything.
  override?: string;
  // The model the node/bot already declares — possibly an env literal.
  authored?: string;
  className?: string;
}) {
  const resolved = useResolvedModel(authored);
  const spec = effectiveModel(override, authored, resolved);
  const { capabilities } = useModelCapabilities(spec);

  const caption = modelCapsTooltip(capabilities);
  if (!caption) return null;

  return (
    <p
      className={className ?? "mt-1 text-caption text-fg-subtle"}
      data-testid="model-caps-caption"
      title={`${spec} — ${caption}`}
    >
      {caption}
    </p>
  );
}
