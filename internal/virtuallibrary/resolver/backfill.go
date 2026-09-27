package resolver

// MatchCandidateIdentityForBackfill reports the confident tier on which a
// persisted virtual row may adopt a fresh candidate's durable identity during
// the identity backfill.
//
// It follows the same tier precedence and the same asymmetry as SharedTier —
// a stored stronger tier the candidate cannot corroborate still vetoes a
// weaker coincidence, and a candidate's stronger tier over a name-only row is
// neutral evidence that the row is the same release — with one deliberate
// tightening: the name tier requires corroborating sizes on both sides.
//
// The backfill writes identity a row never carried, so a bare release-name
// coincidence is not enough. SharedTier treats an unknown size as neutral
// because it is deciding whether to keep serving a row the viewer already
// bound; the backfill is deciding whether two rows are the same release at
// all, and a reused release name with no size to corroborate it would invent
// durable identity that later re-matches the wrong release.
//
// The returned tier is "video_hash", "guid", or "release_name", matching the
// dedup-key tier names.
const (
	backfillTierVideoHash  = "video_hash"
	backfillTierGUID       = "guid"
	backfillTierReleaseKey = "release_name"
)

func MatchCandidateIdentityForBackfill(identity, candidate PersistedIdentityTiers) (string, bool) {
	// A stronger stored tier that the candidate contradicts or cannot
	// corroborate is decisive, exactly as in SharedTier: the row recorded that
	// release, so a coincidental weaker tier must not re-identify it.
	if identity.VideoHash != "" && candidate.VideoHash != "" && identity.VideoHash != candidate.VideoHash {
		return "", false
	}
	if identity.VideoHash != "" {
		if candidate.VideoHash == "" {
			return "", false
		}
		return backfillTierVideoHash, true
	}
	if identity.GUID != "" && candidate.GUID != "" && identity.GUID != candidate.GUID {
		return "", false
	}
	if identity.GUID != "" {
		if candidate.GUID == "" {
			return "", false
		}
		return backfillTierGUID, true
	}
	// Name tier: both sides must carry a real size that plausibly describes the
	// same file. An unknown size is not corroboration and is refused here.
	if identity.ReleaseName != "" &&
		candidate.ReleaseName != "" &&
		identity.ReleaseName == candidate.ReleaseName &&
		identity.ReleaseSize > 0 &&
		candidate.ReleaseSize > 0 &&
		releaseSizesAgree(identity.ReleaseSize, candidate.ReleaseSize) {
		return backfillTierReleaseKey, true
	}
	return "", false
}
