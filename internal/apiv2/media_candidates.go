package apiv2

import (
	"context"
	"net/http"

	"github.com/Silo-Server/silo-server/internal/api/handlers"
	catalogpkg "github.com/Silo-Server/silo-server/internal/catalog"
)

const opRefreshVirtualCandidates = "refreshVirtualCandidates"

// VirtualCandidatesRefreshService force-re-lists and persists one virtual
// item's provider candidates, then answers the retained list. A missing
// implementation is a wiring gap, not a server fault.
type VirtualCandidatesRefreshService interface {
	RefreshVirtualCandidates(ctx context.Context, userID int, profileID, contentID string, filter catalogpkg.AccessFilter) ([]catalogpkg.FileVersion, error)
}

// VirtualCandidatesRefreshInput names the item whose version candidates to
// re-list.
type VirtualCandidatesRefreshInput struct {
	MediaID ID `path:"media_id" doc:"A movie or episode whose virtual candidates to re-list" example:"movie:heat-1995"`
}

// VirtualCandidatesRefresh is the refreshed version list in the watch-detail
// shape, so a client can replace its version menu wholesale.
type VirtualCandidatesRefresh struct {
	Versions []WatchFileVersion `json:"versions" doc:"Every retained virtual candidate of the item; empty, never null"`
}

// VirtualCandidatesRefreshOutput is the refresh response.
type VirtualCandidatesRefreshOutput struct {
	Body VirtualCandidatesRefresh
}

func registerMediaCandidates(reg *Registry) {
	op := humaOp(http.MethodPost, Prefix+"/media/{media_id}/virtual-candidates:refresh", opRefreshVirtualCandidates, "watch",
		"Force a fresh provider re-list for a virtual item's version candidates and return the retained list.")
	op.DefaultStatus = http.StatusOK
	Register(reg, Operation{
		Operation:       op,
		Class:           ClassProfileScoped,
		ProfileOptional: true,
		ServiceBacked:   true,
		// Concurrent refreshes of one title coalesce on the same provider
		// re-list; the command returns the already-refreshed list rather than
		// starting a second one.
		RetrySafety: RetrySafetyCoalescing,
	}, func(ctx context.Context, in *VirtualCandidatesRefreshInput) (*VirtualCandidatesRefreshOutput, error) {
		if reg.deps.VirtualCandidatesRefresh == nil || reg.deps.Watch == nil {
			return nil, unavailable("virtual candidates refresh")
		}
		claims := claimsFrom(ctx)
		if claims == nil {
			return nil, NewProblem(TypeAuthenticationRequired, "Authentication is required.")
		}
		filter, err := reg.deps.Watch.ContextAccessFilter(ctx, handlers.AccessFilterOptions{})
		if err != nil {
			return nil, NewProblem(TypeInternalError, "An unexpected error occurred.")
		}
		versions, err := reg.deps.VirtualCandidatesRefresh.RefreshVirtualCandidates(ctx, claims.UserID, profileFrom(ctx), string(in.MediaID), filter)
		if err != nil {
			return nil, serviceProblem(err)
		}
		return &VirtualCandidatesRefreshOutput{Body: VirtualCandidatesRefresh{Versions: watchVersionsOf(versions)}}, nil
	})
}
