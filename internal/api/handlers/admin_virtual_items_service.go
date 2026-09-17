package handlers

import (
	"context"
	"net/http"
	"time"
)

// AdminVirtualItemView is one zero-storage virtual library item with the
// health of its virtual candidates, as an administrator sees it.
type AdminVirtualItemView struct {
	ContentID       string     `json:"content_id"`
	Title           string     `json:"title"`
	ItemType        string     `json:"type"`
	LibraryID       int        `json:"library_id"`
	LibraryName     string     `json:"library_name"`
	InstallationID  int        `json:"installation_id"`
	CandidateCount  int        `json:"candidate_count"`
	FailedCount     int        `json:"failed_count"`
	LastDeliveredAt *time.Time `json:"last_delivered_at"`
	LastSeenAt      *time.Time `json:"last_seen_at"`
	ReleaseNames    []string   `json:"release_names"`
}

// AdminVirtualItemsView is the bounded, unpaginated admin list.
type AdminVirtualItemsView struct {
	Items []AdminVirtualItemView `json:"items"`
}

// ListAdminVirtualItems lists virtual library items for the admin Release
// Desk. A nil item repository is a wiring gap, not a server fault.
func (h *LibraryCollectionHandler) ListAdminVirtualItems(ctx context.Context, limit int) (AdminVirtualItemsView, error) {
	var none AdminVirtualItemsView
	if h.itemRepo == nil {
		return none, apiError(http.StatusServiceUnavailable, "unavailable", "Virtual library listing unavailable")
	}
	summaries, err := h.itemRepo.ListVirtualItemSummaries(ctx, limit)
	if err != nil {
		return none, apiError(http.StatusInternalServerError, "internal_error", "Failed to list virtual items")
	}
	items := make([]AdminVirtualItemView, 0, len(summaries))
	for _, summary := range summaries {
		items = append(items, AdminVirtualItemView{
			ContentID:       summary.ContentID,
			Title:           summary.Title,
			ItemType:        summary.ItemType,
			LibraryID:       summary.LibraryID,
			LibraryName:     summary.LibraryName,
			InstallationID:  summary.InstallationID,
			CandidateCount:  summary.CandidateCount,
			FailedCount:     summary.FailedCount,
			LastDeliveredAt: summary.LastDeliveredAt,
			LastSeenAt:      summary.LastSeenAt,
			ReleaseNames:    summary.ReleaseNames,
		})
	}
	return AdminVirtualItemsView{Items: items}, nil
}
