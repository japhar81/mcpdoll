import { describe, expect, it } from "vitest";
import { toGroupList } from "./identity.tsx";

describe("toGroupList", () => {
  it("splits, trims, and drops empties", () => {
    expect(toGroupList(" eng-platform , sec-research ")).toEqual([
      "eng-platform",
      "sec-research",
    ]);
  });

  it("returns nothing for an empty or comma-only string", () => {
    // An empty string must not become [""] — a group named "" would be sent to
    // the gateway and could match nothing while looking like a real claim.
    expect(toGroupList("")).toEqual([]);
    expect(toGroupList(" , , ")).toEqual([]);
  });
});
