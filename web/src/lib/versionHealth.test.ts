import { describe, expect, it } from "vitest";

import { deriveVersionHealth } from "./versionHealth";

describe("deriveVersionHealth", () => {
  it("returns no badge while the server has reported no verdict", () => {
    expect(deriveVersionHealth({})).toBeNull();
  });

  it("keeps a server-reported live version clean", () => {
    expect(deriveVersionHealth({ available: true })).toBeNull();
  });

  it("warns for a version the server's liveness check could not resolve", () => {
    expect(deriveVersionHealth({ available: false })).toMatchObject({
      key: "unavailable",
      label: "Will retry on play",
      tone: "warn",
    });
  });

  it("marks a server-failed candidate as failed, over the liveness verdict", () => {
    expect(deriveVersionHealth({ available: false, failed: true })).toMatchObject({
      key: "failed",
      label: "Failed",
      tone: "danger",
    });
  });
});
