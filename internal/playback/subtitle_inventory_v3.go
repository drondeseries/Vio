package playback

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/virtuallibrary/stream"
)

// Subtitle source classes in the combined-ordinal space.
const (
	SubtitleSourceExternalV3   = "external"
	SubtitleSourceEmbeddedV3   = "embedded"
	SubtitleSourceDownloadedV3 = "downloaded"
)

// Sidecar file extensions the stream handler serves a subtitle track as. The
// extension is part of the published URL, so it belongs to the contract rather
// than to whichever call site happens to build a path.
const (
	SubtitleExtASSV3 = ".ass"
	SubtitleExtPGSV3 = ".sup"
	SubtitleExtVTTV3 = ".vtt"
	// DownloadedSubtitleIDParamV3 pins a generated/downloaded sidecar URL to
	// its stable database row instead of resolving a mutable inventory ordinal.
	DownloadedSubtitleIDParamV3 = "downloaded_subtitle_id"
)

// WebVTT is the format every convert-mode subtitle lands in, so its format
// token and media type travel together with the extension above: a plan that
// advertises one of the three and serves another hands the client an artifact
// its parser rejects.
const (
	SubtitleFormatVTTV3 = "vtt"
	SubtitleMIMEVTTV3   = "text/vtt"
)

// Subtitle delivery classes describing how a track reaches the screen.
const (
	// SubtitleDeliverySidecarV3 marks a track the client can fetch directly
	// from its published URL.
	SubtitleDeliverySidecarV3 = "sidecar"
	// SubtitleDeliveryBurnInOnlyV3 marks a track with no client-fetchable
	// representation: DVD/DVB bitmap streams have no sidecar shape the stream
	// handler can serve, so server-side burn-in is the only route. The track
	// still occupies its combined ordinal and stays selectable; it carries no
	// URL.
	SubtitleDeliveryBurnInOnlyV3 = "burn_in_only"
)

// SubtitleInventoryItemV3 is one selectable subtitle track at its frozen
// combined ordinal.
type SubtitleInventoryItemV3 struct {
	TrackID       string `json:"track_id"`
	CombinedIndex int    `json:"combined_index"`
	Source        string `json:"source"`
	Codec         string `json:"codec,omitempty"`
	Language      string `json:"language,omitempty"`
	Label         string `json:"label,omitempty"`
	Forced        bool   `json:"forced"`
	// Default marks the track the source container flags as its default
	// selection. Only embedded and external tracks can carry it.
	Default         bool   `json:"default"`
	HearingImpaired bool   `json:"hearing_impaired"`
	Delivery        string `json:"delivery"`
	// URL is the session-scoped sidecar URL, present only on
	// SubtitleDeliverySidecarV3 tracks and only once a session exists.
	URL string `json:"url,omitempty"`
	// FontBundleURL is the attachment bundle needed to render an embedded
	// ASS/SSA track with its authored typesetting.
	FontBundleURL string `json:"font_bundle_url,omitempty"`
	// downloadedSubtitleID is deliberately not serialized: clients address the
	// opaque session URL, while the server uses the row ID to make that URL
	// stable across inventory reordering and seek reanchors.
	downloadedSubtitleID int
	// sourceCombinedIndex is the ordinal this entry's track occupies in the
	// un-deduplicated source ranges: externals, then embedded, then downloaded.
	// The published CombinedIndex is dense, so the two differ whenever the
	// de-duplication below suppressed an earlier track. The stream route and
	// the selection resolver need the source ordinal to find the underlying
	// track; it is deliberately not serialized, because a plan restored from
	// JSON recovers the same mapping by re-running the de-duplication.
	sourceCombinedIndex int
	// sourceCombinedIndexSet distinguishes a genuine sourceCombinedIndex of 0
	// from the zero value of an item decoded from JSON.
	sourceCombinedIndexSet bool
}

// BuildSubtitleInventoryV3 is the single normative implementation of the
// combined-ordinal ordering rule, and the only place in the server that
// assigns subtitle ordinals.
//
// Ordinals are assigned across three consecutive ranges, in this order:
//
//	[0, len(ExternalSubtitles))                       external sidecar files
//	[len(External), len(External)+len(SubtitleTracks)) embedded container tracks
//	[that, +len(additional))                           downloaded/generated tracks
//
// The published CombinedIndex space is always dense and gap-free: each
// surviving track takes the ordinal of its position in the published list
// (0..n-1), and its TrackID encodes that same ordinal, so a client can map its
// own track list positionally and track_id always agrees with combined_index.
// A duplicate suppressed by the de-duplication below simply does not consume
// an ordinal, and every later surviving track shifts down by one.
//
// Inside the server the underlying tracks still live in the source-space
// ordinals described above; the published ordinal is a dense view of them.
// BuildSubtitleInventoryV3 records the source ordinal on each item (unexported,
// recovered by re-running the same de-duplication when a plan is restored from
// JSON) so the stream handler's pinned identity and the selection resolver can
// translate a published ordinal back to the real track. The pins the scoped URL
// carries — external_subtitle_key, embedded_stream_index, downloaded_subtitle_id
// — name the source track, not the published ordinal, which is why the route
// still resolves the same track after a renumber.
//
// A track that cannot be delivered as a sidecar is still published — with
// SubtitleDeliveryBurnInOnlyV3 and no URL — rather than omitted, so a client
// never has to derive an ordinal by counting what it can render.
//
// Within each range the order is the source order: catalog order for
// externals, container stream order for embedded tracks, and
// ListDownloadedSubtitles' created_at order for downloaded ones. The published
// order is therefore stable for as long as the file's track set is, which is
// what makes the `file:{id}:subtitle:{published ordinal}` identity meaningful.
//
// Entries are collapsed only when they positively identify the same track: the
// same source range, the same non-empty base language (or, for downloaded
// rows, the same positive stable row ID), the same normalized codec, and the
// same forced and hearing-impaired flags. An entry with an unknown language and
// no other positive identity is never suppressed, so several unknown-language
// tracks that merely share a codec or flags are all published. The seen-set
// spans all three ranges, so a duplicate later in the combined list
// de-duplicates against an earlier one. Downloaded entries key on their stable
// row ID, so two distinct downloads in one language are both kept, and a
// downloaded or AI subtitle is never dropped merely because an embedded track
// shares its language (the source range differs).
//
// The returned items carry no URLs; use SubtitleInventoryV3 once a session
// exists.
func BuildSubtitleInventoryV3(file *models.MediaFile, additional []SubtitleInventoryEntryV3) []SubtitleInventoryItemV3 {
	if file == nil {
		return nil
	}
	externalCount := len(file.ExternalSubtitles)
	base := externalCount + len(file.SubtitleTracks)
	items := make([]SubtitleInventoryItemV3, 0, base+len(additional))
	seen := make(map[string]struct{}, base+len(additional))

	for index, sub := range file.ExternalSubtitles {
		if subtitleInventoryDuplicateV3(seen, SubtitleSourceExternalV3, "", sub.Format, sub.Language, sub.Forced, sub.HearingImpaired) {
			continue
		}
		items = append(items, subtitleInventoryItemV3(file.ID, len(items), index, SubtitleSourceExternalV3, sub.Format,
			sub.Language, firstNonEmptySubtitleLabelV3(sub.Title, sub.EmbeddedTitle, filepath.Base(sub.Path), sub.Language),
			sub.Forced, sub.Default, sub.HearingImpaired))
	}
	for index, track := range file.SubtitleTracks {
		if subtitleInventoryDuplicateV3(seen, SubtitleSourceEmbeddedV3, "", track.Codec, track.Language, track.Forced, track.HearingImpaired) {
			continue
		}
		items = append(items, subtitleInventoryItemV3(file.ID, len(items), externalCount+index, SubtitleSourceEmbeddedV3, track.Codec,
			track.Language, firstNonEmptySubtitleLabelV3(track.Title, track.EmbeddedTitle, track.Language),
			track.Forced, track.Default, track.HearingImpaired))
	}
	for index, entry := range additional {
		source := entry.Source
		if source == "" {
			source = SubtitleSourceDownloadedV3
		}
		identity := ""
		if source == SubtitleSourceDownloadedV3 && entry.DownloadedSubtitleID > 0 {
			identity = strconv.Itoa(entry.DownloadedSubtitleID)
		}
		if subtitleInventoryDuplicateV3(seen, source, identity, entry.Codec, entry.Language, entry.Forced, entry.HearingImpaired) {
			continue
		}
		sourceIndex := entry.CombinedIndex
		if sourceIndex <= 0 {
			sourceIndex = base + index
		}
		item := subtitleInventoryItemV3(file.ID, len(items), sourceIndex, source, entry.Codec,
			entry.Language, firstNonEmptySubtitleLabelV3(entry.Label, entry.Language),
			entry.Forced, false, entry.HearingImpaired)
		item.downloadedSubtitleID = entry.DownloadedSubtitleID
		items = append(items, item)
	}
	return items
}

// subtitleInventoryDuplicateV3 records an inventory entry's de-duplication key
// and reports whether an equivalent track was already seen.
//
// Suppression requires a positive identity for the track: a recognized,
// non-empty canonical language, or (for downloaded rows) a positive stable row
// ID, passed as identity. An entry with neither is never a de-duplication
// candidate and is never recorded, so two tracks whose language is unknown are
// never collapsed merely because their codec, forced flag or hearing-impaired
// flag coincide. The key pairs that identity with the source range, normalized
// codec and flags, so entries differing in any of those stay distinct.
func subtitleInventoryDuplicateV3(seen map[string]struct{}, source, identity, codec, language string, forced, hearingImpaired bool) bool {
	base := stream.CanonicalLanguageBase(language)
	if base == "" && identity == "" {
		return false
	}
	key := strings.Join([]string{
		source,
		identity,
		normalizeCodecV3(codec),
		base,
		strconv.FormatBool(forced),
		strconv.FormatBool(hearingImpaired),
	}, "\x00")
	if _, ok := seen[key]; ok {
		return true
	}
	seen[key] = struct{}{}
	return false
}

// SubtitleInventoryV3 returns the combined-ordinal inventory with
// session-scoped stream URLs attached to every sidecar-deliverable track.
func SubtitleInventoryV3(sessionID string, file *models.MediaFile, additional []SubtitleInventoryEntryV3) []SubtitleInventoryItemV3 {
	return ScopeSubtitleInventoryV3(sessionID, file, BuildSubtitleInventoryV3(file, additional))
}

// ScopeSubtitleInventoryV3 attaches session URLs to an already planned
// inventory without resolving it again against mutable subtitle repositories.
// This keeps the advertised menu and the planner's selected ordinal on the
// same snapshot.
func ScopeSubtitleInventoryV3(sessionID string, file *models.MediaFile, inventory []SubtitleInventoryItemV3) []SubtitleInventoryItemV3 {
	// Inventory is required by the v3 wire contract and must always encode as
	// an array. Copy into a non-nil slice so an empty inventory remains []
	// instead of becoming JSON null while session URLs are attached.
	items := append([]SubtitleInventoryItemV3{}, inventory...)
	if sessionID == "" || file == nil {
		return items
	}
	// ownPrefix lazily rebuilds the file's external+embedded inventory so a
	// plan decoded from JSON (which drops the unexported source ordinals) can
	// still translate a published position back to the source track the pin
	// names. Downloaded entries always come last, so the file's own entries
	// occupy the same leading positions in both.
	var ownPrefix []SubtitleInventoryItemV3
	sourceIndexAt := func(i int) (int, bool) {
		if items[i].sourceCombinedIndexSet {
			return items[i].sourceCombinedIndex, true
		}
		if ownPrefix == nil {
			ownPrefix = BuildSubtitleInventoryV3(file, nil)
		}
		if i >= 0 && i < len(ownPrefix) && ownPrefix[i].Source == items[i].Source && ownPrefix[i].sourceCombinedIndexSet {
			return ownPrefix[i].sourceCombinedIndex, true
		}
		return 0, false
	}
	for i := range items {
		if items[i].Delivery != SubtitleDeliverySidecarV3 {
			continue
		}
		previousURL := items[i].URL
		items[i].URL = SubtitleStreamURLV3(sessionID, items[i].CombinedIndex, items[i].Codec, file.ID)
		if items[i].Source == SubtitleSourceDownloadedV3 && items[i].downloadedSubtitleID > 0 {
			items[i].URL = DownloadedSubtitleStreamURLV3(
				sessionID,
				items[i].CombinedIndex,
				items[i].Codec,
				file.ID,
				items[i].downloadedSubtitleID,
			)
		}
		switch items[i].Source {
		case SubtitleSourceExternalV3:
			key := subtitleURLIdentityV3(previousURL, ExternalSubtitleKeyParamV3, file.ID)
			if key == "" {
				if source, ok := sourceIndexAt(i); ok && source >= 0 && source < len(file.ExternalSubtitles) {
					key = ExternalSubtitlePathKeyV3(file.ExternalSubtitles[source].Path)
				}
			}
			if key != "" {
				items[i].URL += "&" + ExternalSubtitleKeyParamV3 + "=" + key
			}
		case SubtitleSourceEmbeddedV3:
			items[i].FontBundleURL = SubtitleFontBundleURLV3(sessionID, items[i].CombinedIndex, items[i].Codec, file.ID)
			index := subtitleURLIdentityV3(previousURL, EmbeddedSubtitleStreamIndexParamV3, file.ID)
			if index == "" {
				if source, ok := sourceIndexAt(i); ok {
					ordinal := source - len(file.ExternalSubtitles)
					if ordinal >= 0 && ordinal < len(file.SubtitleTracks) {
						index = strconv.Itoa(file.SubtitleTracks[ordinal].Index)
					}
				}
			}
			if index != "" {
				identity := "&" + EmbeddedSubtitleStreamIndexParamV3 + "=" + index
				items[i].URL += identity
				if items[i].FontBundleURL != "" {
					items[i].FontBundleURL += identity
				}
			}
		}
	}
	return items
}

// SubtitleInventoryOwnSourceIndexV3 maps a published combined ordinal to the
// source-space ordinal of the file's own external or embedded track it names.
// It reports false for an out-of-range ordinal or one that addresses a
// downloaded entry (whose identity is a stable row ID, not a source slot). It
// is the accessor a boundary that still indexes file.ExternalSubtitles or
// file.SubtitleTracks directly must use to translate a dense published ordinal
// back to the array slot its identity and extraction pins are built from.
func SubtitleInventoryOwnSourceIndexV3(file *models.MediaFile, publishedIndex int) (int, bool) {
	if file == nil || publishedIndex < 0 {
		return 0, false
	}
	own := BuildSubtitleInventoryV3(file, nil)
	if publishedIndex >= len(own) {
		return 0, false
	}
	item := own[publishedIndex]
	if !item.sourceCombinedIndexSet {
		return 0, false
	}
	return item.sourceCombinedIndex, true
}

// SubtitleInventoryOwnPublishedIndexV3 is the inverse of
// SubtitleInventoryOwnSourceIndexV3: it maps a source-space ordinal of the
// file's own external or embedded track to the dense published combined ordinal
// the client sees. It reports false for an out-of-range source ordinal, a
// downloaded entry, or a track the de-duplication suppressed (which has no
// published ordinal of its own). A caller that finds an equivalent target track
// in the source arrays must translate it through this before writing an ordinal
// a plan or client will interpret as published.
func SubtitleInventoryOwnPublishedIndexV3(file *models.MediaFile, sourceIndex int) (int, bool) {
	if file == nil || sourceIndex < 0 {
		return 0, false
	}
	for _, item := range BuildSubtitleInventoryV3(file, nil) {
		if item.sourceCombinedIndexSet && item.sourceCombinedIndex == sourceIndex {
			return item.CombinedIndex, true
		}
	}
	return 0, false
}

// subtitleInventorySourceIndexV3 maps a published combined ordinal to the
// source-space ordinal of the track it names, including downloaded entries. It
// rebuilds the same de-duplicated inventory the plan published, so the mapping
// matches the client's view exactly.
func subtitleInventorySourceIndexV3(file *models.MediaFile, additional []SubtitleInventoryEntryV3, publishedIndex int) (int, bool) {
	for _, item := range BuildSubtitleInventoryV3(file, additional) {
		if item.CombinedIndex == publishedIndex {
			return item.sourceCombinedIndex, item.sourceCombinedIndexSet
		}
	}
	return 0, false
}

// SubtitleInventoryNeedsDownloadedIdentityV3 reports whether an inventory was
// decoded from its wire form and therefore lost the server-only row IDs needed
// to mint stable downloaded-subtitle URLs. Freshly planned inventories retain
// the IDs in memory; durable JSON plans are rebuilt from the repository.
func SubtitleInventoryNeedsDownloadedIdentityV3(items []SubtitleInventoryItemV3) bool {
	for _, item := range items {
		if item.Source == SubtitleSourceDownloadedV3 && item.downloadedSubtitleID <= 0 {
			return true
		}
	}
	return false
}

// SubtitleInventoryItemAtV3 finds the inventory entry at a published combined
// ordinal.
func SubtitleInventoryItemAtV3(items []SubtitleInventoryItemV3, combinedIndex int) (SubtitleInventoryItemV3, bool) {
	for _, item := range items {
		if item.CombinedIndex == combinedIndex {
			return item, true
		}
	}
	return SubtitleInventoryItemV3{}, false
}

// SubtitleURLExtV3 returns the sidecar file extension the stream handler serves
// a subtitle codec as.
func SubtitleURLExtV3(codec string) string {
	switch {
	case IsASS(codec):
		return SubtitleExtASSV3
	case IsPGS(codec):
		return SubtitleExtPGSV3
	}
	return SubtitleExtVTTV3
}

// SubtitleStreamURLV3 builds the session-scoped sidecar URL for a published
// combined ordinal. The ordinal in the path is the dense published one the
// client selected, not the container's stream index; the scoping pass appends
// an identity pin (external_subtitle_key, embedded_stream_index or
// downloaded_subtitle_id) that names the underlying source track, and the
// stream handler resolves the pin back to it.
func SubtitleStreamURLV3(sessionID string, combinedIndex int, codec string, fileID int) string {
	return fmt.Sprintf("/stream/%s/subtitles/%d%s?file_id=%d", sessionID, combinedIndex, SubtitleURLExtV3(codec), fileID)
}

// DownloadedSubtitleStreamURLV3 binds the public combined ordinal to the
// stable downloaded-subtitle row selected when the plan was accepted.
func DownloadedSubtitleStreamURLV3(sessionID string, combinedIndex int, codec string, fileID, downloadedSubtitleID int) string {
	return SubtitleStreamURLV3(sessionID, combinedIndex, codec, fileID) +
		"&" + DownloadedSubtitleIDParamV3 + "=" + strconv.Itoa(downloadedSubtitleID)
}

// SubtitleFontBundleURLV3 builds the attachment-bundle URL for an embedded
// ASS/SSA track, or "" for any other codec.
func SubtitleFontBundleURLV3(sessionID string, combinedIndex int, codec string, fileID int) string {
	if !IsASS(codec) {
		return ""
	}
	return fmt.Sprintf("/stream/%s/subtitles/%d/fonts?file_id=%d", sessionID, combinedIndex, fileID)
}

// subtitleInventoryItemV3 builds one published entry. publishedIndex is the
// dense combined ordinal the client sees; sourceIndex is the ordinal the track
// occupies in the un-deduplicated source ranges and is retained for the route
// and selection mapping.
func subtitleInventoryItemV3(fileID, publishedIndex, sourceIndex int, source, codec, language, label string, forced, isDefault, hearingImpaired bool) SubtitleInventoryItemV3 {
	return SubtitleInventoryItemV3{
		TrackID:                TrackIDV3(fileID, "subtitle", publishedIndex),
		CombinedIndex:          publishedIndex,
		Source:                 source,
		Codec:                  codec,
		Language:               language,
		Label:                  label,
		Forced:                 forced,
		Default:                isDefault,
		HearingImpaired:        hearingImpaired,
		Delivery:               subtitleDeliveryClassV3(source, codec),
		sourceCombinedIndex:    sourceIndex,
		sourceCombinedIndexSet: true,
	}
}

// subtitleDeliveryClassV3 mirrors what the stream handler can actually serve:
// text tracks convert to WebVTT/ASS, and an embedded PGS track extracts
// losslessly to a .sup elementary stream. Every other bitmap shape — DVD/DVB
// embedded, and any bitmap arriving as an external or downloaded file — has no
// sidecar representation, so it is burn-in only.
func subtitleDeliveryClassV3(source, codec string) string {
	switch {
	case isTextSubtitleV3(codec):
		return SubtitleDeliverySidecarV3
	case source == SubtitleSourceEmbeddedV3 && IsPGS(codec):
		return SubtitleDeliverySidecarV3
	}
	return SubtitleDeliveryBurnInOnlyV3
}

func firstNonEmptySubtitleLabelV3(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
