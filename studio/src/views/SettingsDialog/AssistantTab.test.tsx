// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { readAssistantActionPolicy } from "@/lib/chatDock/assistantActions";

import AssistantTab from "./AssistantTab";

beforeEach(() => window.localStorage.clear());
afterEach(cleanup);

describe("Assistant settings actions", () => {
  it("lists host-owned actions and persists their policies independently", () => {
    render(<AssistantTab />);

    const apply = screen.getByRole("combobox", {
      name: "Apply changes to the open bot policy",
    });
    const save = screen.getByRole("combobox", {
      name: "Save the open bot policy",
    });
    expect((apply as HTMLSelectElement).value).toBe("ask");
    expect((save as HTMLSelectElement).value).toBe("ask");

    fireEvent.change(apply, { target: { value: "explicit" } });
    expect(readAssistantActionPolicy("editor.apply")).toBe("explicit");
    expect(readAssistantActionPolicy("editor.save")).toBe("ask");
  });
});
