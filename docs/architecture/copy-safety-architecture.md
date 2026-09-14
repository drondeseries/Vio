# Copy-Safety and Playback Planning Architecture

This document describes the architectural trade-off and design decisions regarding
copy-safety verification in Silo Server.

## Background

Upstream Silo includes an experimental optimistic copy-safety start path: when playback
begins on a media file whose copy safety is not yet established, it begins optimistic direct
streaming while asynchronously racing a background probe. If the file turns out to be copy-unsafe
(e.g., incompatible audio/video stream parameters that corrupt direct stream clients), the server
aborts the progressive stream and pushes a `plan_invalidated_v1` WebSocket control event to
instruct the client to replan as a transcode.

## Architectural Trade-off in Fork

In this fork, we deliberately choose **deterministic pre-playback capability planning and persistent verdicts**
over optimistic mid-stream races for the following reasons:

1. **Client Compatibility & Protocol Invariants:**
   - Standard ecosystem clients (Apple AVPlayer, ExoPlayer, Jellyfin compat clients like Infuse/Findroid/Kodi)
     cannot reliably handle unexpected mid-stream transport resets and out-of-band `plan_invalidated_v1` signals
     during progressive HTTP streaming.
   - For virtual sources and remote streaming relays, an optimistic race introduces unneeded upstream connections
     and duplicate HTTP byte-range pulls against external debrid/stream providers.

2. **Deterministic Capability Resolution:**
   - Files are probed at ingestion or dynamically upon first virtual resolution, with probe results and copy-safety
     verdicts persisted in the catalog database (`media_files.copy_safety_multi`, `copy_safety_checked_size`, `copy_safety_checked_at`).
   - The capability planner (`PlanPlaybackV3`) deterministically evaluates container, video codec, audio codec,
     and dynamic range against the client's reported device profile before playback starts.
   - If a stream requires audio downmixing, container remuxing, or HDR tone-mapping, Silo selects the appropriate
     remux or transcode route upfront, ensuring 100% startup reliability and zero mid-stream tear-downs.

3. **Resilience & Simplicity:**
   - Eliminating the asynchronous race and complex state machines in `copy_safety_race.go` and `copy_safety_notifier.go`
     reduces concurrency edge-cases, memory pressure, and phantom transcode restarts.
