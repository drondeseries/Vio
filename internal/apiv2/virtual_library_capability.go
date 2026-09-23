package apiv2

import (
	"context"
	"net/http"
)

// VirtualLibraryStatusService reports whether the virtual-library indexer paths
// are wired. Both reads are configuration probes, not network calls.
type VirtualLibraryStatusService interface {
	IndexerCapabilities() (indexerSearch bool, indexerRequest bool)
}

// VirtualLibraryCapabilities is the virtual-library feature-detection
// document. indexer_search gates the "what's on the indexers" list;
// indexer_request gates the per-release request action.
type VirtualLibraryCapabilities struct {
	Capability
	IndexerSearch  bool `json:"indexer_search"`
	IndexerRequest bool `json:"indexer_request"`
}

type VirtualLibraryCapabilitiesOutput struct {
	Status       int
	ETag         string `header:"ETag"`
	CacheControl string `header:"Cache-Control"`
	Body         VirtualLibraryCapabilities
}

func registerVirtualLibraryCapabilities(reg *Registry) {
	Register(reg, Operation{Operation: humaOp(http.MethodGet, Prefix+"/capabilities/virtual-library", "getVirtualLibraryCapabilities", "watch",
		"Discover whether indexer search and provider release requests are available."), Class: ClassProfileScoped, ProfileOptional: true, ServiceBacked: true},
		func(_ context.Context, _ *CapabilityInput) (*VirtualLibraryCapabilitiesOutput, error) {
			indexerSearch, indexerRequest := false, false
			if reg.deps.VirtualLibraryStatus != nil {
				indexerSearch, indexerRequest = reg.deps.VirtualLibraryStatus.IndexerCapabilities()
			}
			state := StateNotConfigured
			if indexerSearch || indexerRequest {
				state = StateAvailable
			}
			return &VirtualLibraryCapabilitiesOutput{Body: VirtualLibraryCapabilities{
				Capability:     Capability{State: state},
				IndexerSearch:  indexerSearch,
				IndexerRequest: indexerRequest,
			}}, nil
		})
}
