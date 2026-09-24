// @vitest-environment jsdom

import { fireEvent, renderHook } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import { KEYBOARD_SKIP_SECONDS, useKeyboardShortcuts } from "./useKeyboardShortcuts";

function renderShortcuts() {
  const video = { currentTime: 100, duration: 300, volume: 1, muted: false } as HTMLVideoElement;
  const videoRef = { current: video };
  const containerRef = { current: null };
  const skip = { back: vi.fn(), forward: vi.fn() };
  renderHook(() => useKeyboardShortcuts(videoRef, containerRef, vi.fn(), skip, vi.fn(), undefined));
  return { skip, video };
}

describe("useKeyboardShortcuts", () => {
  it("delegates arrow keys to the profile skip intervals", () => {
    expect(KEYBOARD_SKIP_SECONDS).toBe(10);

    const { skip } = renderShortcuts();

    fireEvent.keyDown(document, { key: "ArrowLeft" });
    expect(skip.back).toHaveBeenCalledTimes(1);

    fireEvent.keyDown(document, { key: "ArrowRight" });
    expect(skip.forward).toHaveBeenCalledTimes(1);
  });

  it("toggles captions on C", () => {
    const toggleCaptions = vi.fn();
    const video = { currentTime: 5, duration: 300, volume: 1, muted: false } as HTMLVideoElement;
    renderHook(() =>
      useKeyboardShortcuts(
        { current: video },
        { current: null },
        vi.fn(),
        { back: vi.fn(), forward: vi.fn() },
        toggleCaptions,
      ),
    );

    fireEvent.keyDown(document, { key: "c" });
    expect(toggleCaptions).toHaveBeenCalledTimes(1);
  });
});
