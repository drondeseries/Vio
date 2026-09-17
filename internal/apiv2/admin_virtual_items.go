package apiv2

import (
	"context"
	"net/http"

	"github.com/Silo-Server/silo-server/internal/api/handlers"
)

const opListAdminVirtualItems = "listAdminVirtualItems"

// AdminVirtualItemsService is the handler slice the read-only admin virtual
// items operation calls.
type AdminVirtualItemsService interface {
	ListAdminVirtualItems(context.Context, int) (handlers.AdminVirtualItemsView, error)
}

// AdminVirtualItem is one zero-storage virtual library item with the health of
// its virtual candidates.
type AdminVirtualItem struct {
	ID             ID     `json:"id"`
	Title          string `json:"title"`
	Type           string `json:"type"`
	LibraryID      ID     `json:"library_id"`
	LibraryName    string `json:"library_name"`
	InstallationID ID     `json:"installation_id"`
	CandidateCount int    `json:"candidate_count"`
	FailedCount    int    `json:"failed_count"`
	// Both are absent rather than null when never delivered or never seen:
	// the documented schema would otherwise promise a required instant the
	// server cannot always produce.
	LastDeliveredAt *Instant `json:"last_delivered_at,omitempty"`
	LastSeenAt      *Instant `json:"last_seen_at,omitempty"`
	ReleaseNames    []string `json:"release_names"`
}

// AdminVirtualItemCollection is the bounded, unpaginated response envelope.
type AdminVirtualItemCollection struct {
	Collection[AdminVirtualItem]
}

// AdminVirtualItemsOutput is the listAdminVirtualItems response.
type AdminVirtualItemsOutput struct {
	Body AdminVirtualItemCollection
}

// AdminVirtualItemsInput caps the admin list; the repository enforces the same
// bound defensively.
type AdminVirtualItemsInput struct {
	Limit int `query:"limit" minimum:"1" maximum:"500" default:"100" doc:"Maximum virtual items to return; default 100, maximum 500" example:"100"`
}

func registerAdminVirtualItems(reg *Registry) {
	op := Operation{
		Operation:     humaOp(http.MethodGet, Prefix+"/admin/virtual-items", opListAdminVirtualItems, "admin", "List zero-storage virtual library items with candidate health."),
		Class:         ClassActingAdmin,
		ServiceBacked: true,
	}
	Register(reg, op, func(ctx context.Context, in *AdminVirtualItemsInput) (*AdminVirtualItemsOutput, error) {
		if reg.deps.AdminVirtualItems == nil {
			return nil, unavailable("admin virtual items")
		}
		view, err := reg.deps.AdminVirtualItems.ListAdminVirtualItems(ctx, in.Limit)
		if err != nil {
			return nil, serviceProblem(err)
		}
		items := make([]AdminVirtualItem, 0, len(view.Items))
		for _, v := range view.Items {
			items = append(items, AdminVirtualItem{
				ID:              ID(v.ContentID),
				Title:           v.Title,
				Type:            v.ItemType,
				LibraryID:       IDFromInt(int64(v.LibraryID)),
				LibraryName:     v.LibraryName,
				InstallationID:  IDFromInt(int64(v.InstallationID)),
				CandidateCount:  v.CandidateCount,
				FailedCount:     v.FailedCount,
				LastDeliveredAt: instantPtr(v.LastDeliveredAt),
				LastSeenAt:      instantPtr(v.LastSeenAt),
				ReleaseNames:    NonNil(v.ReleaseNames),
			})
		}
		return &AdminVirtualItemsOutput{Body: AdminVirtualItemCollection{Collection: NewCollection(items)}}, nil
	})
}
