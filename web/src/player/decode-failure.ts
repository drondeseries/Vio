import type { FailureV3 } from "./protocol-v3";

/**
 * Machine-readable decode verdict the server sets on a media response whose
 * running decoder rejected the source. Recovery keys on this header (or the
 * matching problem code when only the body is readable), never on message text.
 */
export const DECODE_ERROR_HEADER = "X-Vio-Decode-Error";
export const SOURCE_DECODE_FAILED_CODE = "source_decode_failed";
/** Problem code carried in the 422 body's problem document. */
export const DECODE_FAILED_PROBLEM_CODE = "decode_failed";
/**
 * Client failure classification for a rejected source. It must stay in the
 * server's decode taxonomy (`decode_error` / `decoder_failure`) so the replan
 * engages the server's decode-recovery handling instead of demoting the route
 * as an unknown failure.
 */
export const DECODE_FAILURE_CLASSIFICATION = "decode_error";

/**
 * The subset of hls.js's `ErrorData` this classifier reads. It is structural so
 * the classifier can be unit-tested without loading hls.js.
 */
export interface DecodeErrorCarrier {
  /** hls.js's `LoaderResponse`; `code` is the HTTP status. */
  response?: { code?: number; data?: unknown } | null;
  /**
   * The loader-specific network detail hls.js passes through: a `Response`
   * under its fetch loader, an `XMLHttpRequest` under its xhr loader. Both
   * expose the verdict header synchronously.
   */
  networkDetails?: unknown;
}

/**
 * Reports whether a fatal hls.js network error is the server's decode verdict
 * rather than a network failure. The HTTP 422 is necessary but not sufficient:
 * the manifest route reuses 422 for executor errors, so the
 * `X-Vio-Decode-Error` header (or the `decode_failed` problem code when only
 * the body is available) is the authority.
 */
export function isServerDecodeFailure(carrier: DecodeErrorCarrier | null | undefined): boolean {
  if (!carrier || httpStatus(carrier) !== 422) return false;
  if (decodeVerdictHeader(carrier.networkDetails) === SOURCE_DECODE_FAILED_CODE) {
    return true;
  }
  return problemCode(carrier.response?.data) === DECODE_FAILED_PROBLEM_CODE;
}

/**
 * The failure handed to the existing `failure_recovery` replan when the server
 * rejected the source. The message is diagnostic; the user-facing copy comes
 * from the replacement plan's own terminal.
 */
export function decodeFailure(): FailureV3 {
  return {
    classification: DECODE_FAILURE_CLASSIFICATION,
    message: "The server could not decode this release.",
  };
}

function httpStatus(carrier: DecodeErrorCarrier): number | undefined {
  const fromResponse = carrier.response?.code;
  if (typeof fromResponse === "number") return fromResponse;
  const details = carrier.networkDetails as { status?: unknown } | null | undefined;
  return typeof details?.status === "number" ? details.status : undefined;
}

function decodeVerdictHeader(networkDetails: unknown): string | null {
  if (!networkDetails || typeof networkDetails !== "object") return null;
  const details = networkDetails as {
    headers?: { get?: (name: string) => string | null };
    getResponseHeader?: (name: string) => string | null;
  };
  if (typeof details.headers?.get === "function") {
    return details.headers.get(DECODE_ERROR_HEADER);
  }
  if (typeof details.getResponseHeader === "function") {
    return details.getResponseHeader(DECODE_ERROR_HEADER);
  }
  return null;
}

function problemCode(data: unknown): string | undefined {
  let parsed = data;
  if (typeof parsed === "string") {
    if (parsed.trim() === "") return undefined;
    try {
      parsed = JSON.parse(parsed);
    } catch {
      return undefined;
    }
  }
  if (!parsed || typeof parsed !== "object") return undefined;
  const envelope = parsed as { error?: unknown; code?: unknown };
  if (typeof envelope.error === "string") return envelope.error;
  if (typeof envelope.code === "string") return envelope.code;
  return undefined;
}
