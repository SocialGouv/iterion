// @vitest-environment jsdom

import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { ReviewInstructions } from "./ReviewInstructions";
import { aiReviewContent, buildReviewBrief } from "./reviewBrief";

afterEach(cleanup);

describe("ReviewInstructions", () => {
  it("renders exactly the AI-provided points and keeps the full criteria collapsed", () => {
    const detail = Array.from(
      { length: 35 },
      () => "Vérifiez chaque dépendance technique et chaque identifiant interne.",
    ).join(" ");
    const source = `# Validation du plan — town-proof-core-loop-v1\n\n${detail}`;
    const points = [
      "Comparez le concept avec la scène cible.",
      "Confirmez que chaque étape produit un résultat jouable.",
      "Signalez uniquement les écarts qui empêchent l’approbation.",
    ] as const;

    render(
      <ReviewInstructions
        instructions={source}
        reviewBrief={{ version: 1, source: "ai", points: [...points] }}
      />,
    );

    expect(screen.getByText("Consignes de la revue IA")).toBeTruthy();
    expect(screen.getByRole("heading", { name: "Validation du plan" })).toBeTruthy();
    const list = screen.getByRole("list");
    expect(within(list).getAllByRole("listitem")).toHaveLength(3);
    for (const point of points) {
      expect(within(list).getByText(point)).toBeTruthy();
    }
    expect(
      screen.queryByText("Consultez les fichiers présentés ci-dessus."),
    ).toBeNull();
    const details = screen.getByText(/Afficher les critères détaillés/).closest("details");
    expect(details?.hasAttribute("open")).toBe(false);
  });

  it("uses the legacy AI detail verbatim as one indivisible point", () => {
    const legacy =
      "Le plan couvre la boucle principale. Les dépendances sont cohérentes.";

    render(
      <ReviewInstructions
        instructions="# Validation du plan\n\nApprouvez le résultat."
        questions={{ ai_review_detail: legacy }}
      />,
    );

    expect(screen.getByText("Synthèse de la revue IA")).toBeTruthy();
    expect(screen.getByText(legacy)).toBeTruthy();
    expect(screen.queryByRole("list")).toBeNull();
  });

  it("shows an honest fallback and the original instructions when AI content is absent", () => {
    const instructions =
      "# Pick a direction\n\nWhich option best matches the goal?";

    render(<ReviewInstructions instructions={instructions} />);

    expect(
      screen.getByText("No AI summary was provided for this review."),
    ).toBeTruthy();
    expect(screen.getByText("Which option best matches the goal?")).toBeTruthy();
    expect(screen.queryByRole("list")).toBeNull();
    expect(screen.queryByText(/Show detailed criteria/)).toBeNull();
  });

  it("renders AI points as plain text instead of interpreting markdown or HTML", () => {
    const point = "**Inspectez** <script>danger()</script>";

    render(
      <ReviewInstructions
        instructions="# Validation\n\nConsultez puis approuvez."
        reviewBrief={{ version: 1, source: "ai", points: [point] }}
      />,
    );

    const list = screen.getByRole("list");
    expect(within(list).getByText(point)).toBeTruthy();
    expect(list.querySelector("strong")).toBeNull();
    expect(list.querySelector("script")).toBeNull();
  });

  it("reports stable title, language, and word-count metadata", () => {
    const brief = buildReviewBrief(
      "# Validation visuelle — release-candidate-v2\n\nConsultez le rendu puis approuvez.",
    );
    expect(brief.title).toBe("Validation visuelle");
    expect(brief.french).toBe(true);
    expect(brief.wordCount).toBe(5);
  });
});

describe("aiReviewContent", () => {
  it("prefers the typed brief over the legacy question", () => {
    expect(
      aiReviewContent(
        { version: 1, source: "ai", points: ["Point structuré"] },
        { ai_review_detail: "Ancien détail" },
      ),
    ).toEqual({ kind: "brief", points: ["Point structuré"] });
  });

  it("rejects empty legacy details without fabricating a point", () => {
    expect(aiReviewContent(undefined, { ai_review_detail: "   " })).toBeNull();
    expect(aiReviewContent(undefined, undefined)).toBeNull();
  });
});
