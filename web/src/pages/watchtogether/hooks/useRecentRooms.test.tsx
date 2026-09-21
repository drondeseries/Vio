import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, renderHook } from "@testing-library/react";
import {
  RECENT_ROOMS_KEY,
  forgetRecentRoom,
  markRecentRoomEnded,
  rememberRecentRoom,
  useRecentRooms,
} from "./useRecentRooms";

const auth = { user: { id: 1 }, profile: { id: "p1" } };
vi.mock("@/hooks/useAuth", () => ({ useOptionalAuth: () => auth }));

const room = (id: string, code = "ROOM") => ({ room_id: id, code, self_role: "host" as const });

beforeEach(() => {
  localStorage.clear();
  auth.user = { id: 1 };
  auth.profile = { id: "p1" };
});
afterEach(cleanup);

describe("useRecentRooms", () => {
  it("remembers newest first, dedupes by room, caps at eight, and updates hooks live", () => {
    const { result } = renderHook(() => useRecentRooms());
    expect(result.current).toEqual([]);
    act(() => {
      for (let i = 0; i < 10; i++) {
        rememberRecentRoom({
          room: room(`r${i}`, `C${i}`),
          token: `t${i}`,
          userId: 1,
          profileId: "p1",
          title: `T${i}`,
        });
      }
      rememberRecentRoom({ room: room("r5", "C5"), token: "t5b", userId: 1, profileId: "p1" });
    });
    expect(result.current.map((r) => r.room_id)).toEqual([
      "r5",
      "r9",
      "r8",
      "r7",
      "r6",
      "r4",
      "r3",
      "r2",
    ]);
    expect(result.current[0]).toMatchObject({ token: "t5b", title: "T5", ended: false });
  });

  it("filters by account and profile and prunes entries older than a day", () => {
    rememberRecentRoom({ room: room("mine"), token: "t", userId: 1, profileId: "p1" });
    rememberRecentRoom({ room: room("theirs"), token: "t", userId: 2, profileId: "p1" });
    rememberRecentRoom({ room: room("other-profile"), token: "t", userId: 1, profileId: "p2" });
    const stale = {
      ...JSON.parse(localStorage.getItem(RECENT_ROOMS_KEY)!)[0],
      room_id: "stale",
      last_seen_at: new Date(Date.now() - 25 * 3600 * 1000).toISOString(),
    };
    localStorage.setItem(
      RECENT_ROOMS_KEY,
      JSON.stringify([...JSON.parse(localStorage.getItem(RECENT_ROOMS_KEY)!), stale]),
    );
    const { result } = renderHook(() => useRecentRooms());
    expect(result.current.map((r) => r.room_id)).toEqual(["mine"]);
  });

  it("marks ended and forgets", () => {
    rememberRecentRoom({ room: room("a"), token: "t", userId: 1, profileId: "p1" });
    rememberRecentRoom({ room: room("b"), token: "t", userId: 1, profileId: "p1" });
    const { result } = renderHook(() => useRecentRooms());
    act(() => markRecentRoomEnded("a"));
    expect(result.current.find((r) => r.room_id === "a")?.ended).toBe(true);
    act(() => forgetRecentRoom("b"));
    expect(result.current.map((r) => r.room_id)).toEqual(["a"]);
  });

  it("returns nothing when signed out and survives corrupt storage", () => {
    localStorage.setItem(RECENT_ROOMS_KEY, "{not json");
    auth.user = null as never;
    const { result } = renderHook(() => useRecentRooms());
    expect(result.current).toEqual([]);
  });
});
