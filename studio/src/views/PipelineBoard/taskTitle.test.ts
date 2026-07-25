import { describe, expect, it } from "vitest";

import {
  compactPipelineTaskTitle,
  derivePipelineTaskTitle,
  PIPELINE_TASK_TITLE_MAX_LENGTH,
} from "./taskTitle";

describe("pipeline task titles", () => {
  it("keeps short primary values readable", () => {
    expect(derivePipelineTaskTitle(["Boudicca", "Episode 1"], "Fallback")).toBe(
      "Boudicca · Episode 1",
    );
  });

  it("uses the first meaningful line of a multiline brief", () => {
    const title = derivePipelineTaskTitle(
      [
        "Je travaille sur une idée de produit.\n\n## Vision générale\n" +
          "Une description très détaillée. ".repeat(20),
      ],
      "Ida",
    );

    expect(title).toBe("Je travaille sur une idée de produit.");
  });

  it("uses the bot label when primary values are blank", () => {
    expect(derivePipelineTaskTitle(["  ", "\n"], "App concept")).toBe(
      "App concept",
    );
  });

  it("also bounds an explicit title", () => {
    const title = compactPipelineTaskTitle("é".repeat(100));
    expect(Array.from(title)).toHaveLength(PIPELINE_TASK_TITLE_MAX_LENGTH);
    expect(title.endsWith("…")).toBe(true);
  });
});
