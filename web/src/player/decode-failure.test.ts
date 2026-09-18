import { describe, expect, it } from "vitest";

import {
  DECODE_FAILURE_CLASSIFICATION,
  decodeFailure,
  isServerDecodeFailure,
} from "./decode-failure";

function responseWithHeader(header: string | null) {
  return { headers: { get: () => header } };
}

function xhrWithHeader(header: string | null) {
  return { getResponseHeader: () => header };
}

describe("isServerDecodeFailure", () => {
  it("detects the verdict from the X-Vio-Decode-Error header on a fetch response", () => {
    expect(
      isServerDecodeFailure({
        response: { code: 422 },
        networkDetails: responseWithHeader("source_decode_failed"),
      }),
    ).toBe(true);
  });

  it("detects the verdict from an xhr's response header", () => {
    expect(
      isServerDecodeFailure({
        response: { code: 422 },
        networkDetails: xhrWithHeader("source_decode_failed"),
      }),
    ).toBe(true);
  });

  it("detects the verdict from the problem code when only the body is readable", () => {
    expect(
      isServerDecodeFailure({
        response: {
          code: 422,
          data: JSON.stringify({
            error: "decode_failed",
            message: "The media source could not be decoded.",
          }),
        },
      }),
    ).toBe(true);
    expect(
      isServerDecodeFailure({
        response: { code: 422, data: { code: "decode_failed" } },
      }),
    ).toBe(true);
  });

  it("uses the HTTP status from the network detail when the loader response omits it", () => {
    expect(
      isServerDecodeFailure({
        networkDetails: {
          status: 422,
          headers: { get: () => "source_decode_failed" },
        },
      }),
    ).toBe(true);
  });

  it("does not treat another 422 verdict as a decode failure", () => {
    expect(
      isServerDecodeFailure({
        response: { code: 422, data: JSON.stringify({ error: "unsupported" }) },
        networkDetails: responseWithHeader("source_preflight_rejected"),
      }),
    ).toBe(false);
  });

  it("does not treat a non-422 status as a decode failure", () => {
    for (const code of [200, 404, 500, 503]) {
      expect(
        isServerDecodeFailure({
          response: { code, data: JSON.stringify({ error: "decode_failed" }) },
          networkDetails: responseWithHeader("source_decode_failed"),
        }),
      ).toBe(false);
    }
  });

  it("ignores an unreadable or malformed carrier", () => {
    expect(isServerDecodeFailure(null)).toBe(false);
    expect(isServerDecodeFailure(undefined)).toBe(false);
    expect(isServerDecodeFailure({ response: { code: 422, data: "not json" } })).toBe(false);
    expect(isServerDecodeFailure({ response: { code: 422, data: "{oops" } })).toBe(false);
  });
});

describe("decodeFailure", () => {
  it("carries the server's decode classification so the replan engages decode recovery", () => {
    expect(decodeFailure()).toEqual({
      classification: DECODE_FAILURE_CLASSIFICATION,
      message: "The server could not decode this release.",
    });
    expect(DECODE_FAILURE_CLASSIFICATION).toBe("decode_error");
  });
});
