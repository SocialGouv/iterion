import { FieldLabel } from "@/components/ui/FieldLabel";
import { RequiredPill } from "@/lib/varValidation";

// Field pairs the canonical ui/FieldLabel caption with the control
// below it (the house sibling pattern — see NewTriggerDialog).
export function Field({
  label,
  children,
  required,
  help,
  htmlFor,
}: {
  label: string;
  required?: boolean;
  // Optional help text, surfaced as FieldLabel's `?` affordance.
  help?: string;
  // id of the control below: the label then names it (RGAA 11.1).
  htmlFor?: string;
  children: React.ReactNode;
}) {
  return (
    <div>
      <FieldLabel help={help} htmlFor={htmlFor}>
        {label}
        {required && (
          <>
            {" "}
            <RequiredPill />
          </>
        )}
      </FieldLabel>
      {children}
    </div>
  );
}
