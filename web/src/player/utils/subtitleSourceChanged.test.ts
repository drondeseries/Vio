import { describe, expect, it } from "vitest";
import { isSubtitleSourceChanged } from "./subtitleSourceChanged";

function response(status: number, json: () => Promise<unknown>): Response {
  return { ok: status >= 200 && status < 300, status, json } as unknown as Response;
}

describe("isSubtitleSourceChanged", () => {
  it("is true for a 409 carrying the subtitle_source_changed error code", async () => {
    const resp = response(409, async () => ({ error: "subtitle_source_changed" }));
    expect(await isSubtitleSourceChanged(resp)).toBe(true);
  });

  it("is false for a 409 carrying a different error code", async () => {
    const resp = response(409, async () => ({ error: "something_else" }));
    expect(await isSubtitleSourceChanged(resp)).toBe(false);
  });

  it("is false for a non-409 status", async () => {
    const resp = response(500, async () => ({ error: "subtitle_source_changed" }));
    expect(await isSubtitleSourceChanged(resp)).toBe(false);
  });

  it("is false when the body is not JSON", async () => {
    const resp = response(409, async () => {
      throw new Error("not json");
    });
    expect(await isSubtitleSourceChanged(resp)).toBe(false);
  });
});
