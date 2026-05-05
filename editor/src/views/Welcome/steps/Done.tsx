import { Button } from "@/components/ui";

interface Props {
  onFinish: () => void;
  onBack: () => void;
}

export default function Done({ onFinish, onBack }: Props) {
  return (
    <div className="max-w-xl flex flex-col gap-4">
      <h2 className="text-lg font-semibold">You're all set!</h2>
      <p className="text-fg-subtle text-sm">
        You can change any of these settings later from File → Settings (or
        Cmd+,).
      </p>
      <div className="flex gap-3">
        <Button onClick={onBack}>Back</Button>
        <Button onClick={onFinish} variant="primary">
          Open editor
        </Button>
      </div>
    </div>
  );
}
