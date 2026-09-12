package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/Silo-Server/silo-server/internal/catalog"
)

type ReleaseOverrideStore interface {
	ListNeedsMetadata(context.Context, int, int, int) ([]catalog.ReleaseMetadataEntry, error)
	RetryNeedsMetadata(context.Context, int, catalog.ReleaseIdentity) (int64, error)
	Read(context.Context, int, catalog.ReleaseIdentity, int64, int) ([]catalog.ReleaseOverride, error)
	Mutate(context.Context, int, catalog.ReleaseOverrideMutation, bool) (catalog.ReleaseOverride, error)
}

func (h *RequestsHandler) HandleReleaseOverrides(w http.ResponseWriter, r *http.Request) {
	viewer, ok := requestViewer(w, r, false)
	if !ok {
		return
	}
	if !viewer.IsAdmin {
		writeReleaseOverrideError(w, catalog.ErrReleaseOverrideForbidden)
		return
	}
	if h.ReleaseOverrides == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "Release override storage is unavailable")
		return
	}
	if r.Method == http.MethodGet {
		q := r.URL.Query()
		parse := func(key string) (int64, error) {
			if q.Get(key) == "" {
				return 0, nil
			}
			return strconv.ParseInt(q.Get(key), 10, 32)
		}
		season, errS := parse("season_number")
		episode, errE := parse("episode_number")
		var before int64
		var errB error
		if q.Get("before_revision") != "" {
			before, errB = strconv.ParseInt(q.Get("before_revision"), 10, 64)
		}
		limit, errL := parse("limit")
		if q.Get("limit") == "" {
			limit = 1
		}
		if errS != nil || errE != nil || errB != nil || errL != nil {
			writeReleaseOverrideError(w, catalog.ErrInvalidReleaseOverride)
			return
		}
		id := catalog.ReleaseIdentity{MediaType: q.Get("media_type"), Provider: q.Get("provider"), ProviderID: q.Get("provider_id"), SeasonNumber: int(season), EpisodeNumber: int(episode)}
		entries, err := h.ReleaseOverrides.Read(r.Context(), viewer.UserID, id, before, int(limit))
		if err != nil {
			writeReleaseOverrideError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, struct {
			Entries []catalog.ReleaseOverride `json:"entries"`
		}{Entries: entries})
		return
	}
	if r.Method != http.MethodPut && r.Method != http.MethodDelete {
		w.Header().Set("Allow", "GET, PUT, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Unsupported method")
		return
	}
	var mutation catalog.ReleaseOverrideMutation
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&mutation); err != nil {
		writeReleaseOverrideError(w, catalog.ErrInvalidReleaseOverride)
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeReleaseOverrideError(w, catalog.ErrInvalidReleaseOverride)
		return
	}
	result, err := h.ReleaseOverrides.Mutate(r.Context(), viewer.UserID, mutation, r.Method == http.MethodDelete)
	if err != nil {
		writeReleaseOverrideError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *RequestsHandler) HandleNeedsReleaseMetadata(w http.ResponseWriter, r *http.Request) {
	viewer, ok := requestViewer(w, r, false)
	if !ok {
		return
	}
	if !viewer.IsAdmin {
		writeReleaseOverrideError(w, catalog.ErrReleaseOverrideForbidden)
		return
	}
	if h.ReleaseOverrides == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "Release metadata storage is unavailable")
		return
	}
	if r.Method == http.MethodGet {
		limit, offset := 50, 0
		var err error
		if raw := r.URL.Query().Get("limit"); raw != "" {
			limit, err = strconv.Atoi(raw)
		}
		if err != nil {
			writeReleaseOverrideError(w, catalog.ErrInvalidReleaseOverride)
			return
		}
		if raw := r.URL.Query().Get("offset"); raw != "" {
			offset, err = strconv.Atoi(raw)
		}
		if err != nil {
			writeReleaseOverrideError(w, catalog.ErrInvalidReleaseOverride)
			return
		}
		entries, err := h.ReleaseOverrides.ListNeedsMetadata(r.Context(), viewer.UserID, limit, offset)
		if err != nil {
			writeReleaseOverrideError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Unsupported method")
		return
	}
	var id catalog.ReleaseIdentity
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&id); err != nil {
		writeReleaseOverrideError(w, catalog.ErrInvalidReleaseOverride)
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeReleaseOverrideError(w, catalog.ErrInvalidReleaseOverride)
		return
	}
	count, err := h.ReleaseOverrides.RetryNeedsMetadata(r.Context(), viewer.UserID, id)
	if err != nil {
		writeReleaseOverrideError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"catalog_items_queued": count})
}

func writeReleaseOverrideError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, catalog.ErrReleaseOverrideForbidden):
		writeError(w, http.StatusForbidden, "forbidden", "Server administrator required")
	case errors.Is(err, catalog.ErrReleaseOverrideConflict):
		writeError(w, http.StatusConflict, "revision_conflict", "Reload the current override revision before retrying")
	case errors.Is(err, catalog.ErrInvalidReleaseOverride):
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal_error", "Release override operation failed")
	}
}
