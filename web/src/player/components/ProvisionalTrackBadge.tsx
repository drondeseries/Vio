/**
 * A small, non-interactive marker for a track menu whose list is still the
 * server's declared inventory rather than probe evidence. It is deliberately
 * the same shape and weight as the per-track codec badges so it reads as a
 * qualifier on the menu, not a new control.
 */
export function ProvisionalTrackBadge() {
  return (
    <span
      className="inline-flex items-center rounded border border-white/25 px-1.5 py-[1px] text-[9.5px] leading-4 font-semibold tracking-wide whitespace-nowrap text-white/60 uppercase"
      title="This track list comes from metadata and has not been confirmed against the file yet."
    >
      Unverified
    </span>
  );
}
