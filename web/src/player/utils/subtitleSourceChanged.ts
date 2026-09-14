/**
 * True when a subtitle fetch response is the server's `subtitle_source_changed`
 * 409 (a virtual release rotated under this plan). The client must refresh the
 * plan's subtitle inventory; retrying the same URL can never succeed.
 */
export async function isSubtitleSourceChanged(resp: Response): Promise<boolean> {
  if (resp.status !== 409) return false;
  try {
    const body = (await resp.json()) as { error?: string };
    return body?.error === "subtitle_source_changed";
  } catch {
    return false;
  }
}
