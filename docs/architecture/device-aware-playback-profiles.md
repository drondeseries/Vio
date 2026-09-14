# Device-Aware Playback Profiles

Silo maintains persistent per-device playback capability profiles to optimize
candidate selection for virtual and alternative media sources.

## Goals and Invariants

1. **Direct play preference.** When ranking multiple candidate streams for a
   virtual media item or alternate versions, Silo prioritizes sources that the
   requesting device can direct-play or direct-stream without transcoding.
2. **Persistent capability profiles.** Jellyfin and native v1 clients submit
   device profiles detailing supported containers, video codecs, audio codecs,
   and dynamic range (HDR10, Dolby Vision). These profiles are stored per
   `(profile_id, client_device_id)` in the user database.
3. **Fallback and compatibility.** When no device profile is recorded for a
   client, Silo falls back to conservative default capabilities and standard
   quality/resolution ranking.
4. **Owner-scoped memoization.** Best-candidate resolution memoization includes
   the client device profile fingerprint, preventing cross-device capability
   poisoning where a desktop client might cache a 4K HEVC stream for an SDR-only
   mobile client.
