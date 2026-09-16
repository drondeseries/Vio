package plugins

const (
	DefaultRepositoryURL  = "https://raw.githubusercontent.com/Silo-Server/silo-plugins/main/manifest.json"
	DefaultRepositoryName = "Vio maintained"

	ApprovedCommunityRepositoryURL  = "https://raw.githubusercontent.com/Silo-Community/silo-plugins/main/manifest.json"
	ApprovedCommunityRepositoryName = "Approved community"
	ForkVirtualRepositoryURL        = "https://raw.githubusercontent.com/drondeseries/silo-virtual-library/main/catalog.json"
	ForkVirtualRepositoryName       = "Vio Virtual Library"

	OfficialRepositoryManagedKey          = "official"
	ApprovedCommunityRepositoryManagedKey = "approved-community"
	ForkVirtualRepositoryManagedKey       = "fork-virtual-library"

	// The persisted source kind. It is a stored value with a database check
	// constraint (20260709191109) and an API enum, both of which name 'silo',
	// so the rebrand's product name does not apply here.
	RepositorySourceSilo              = "silo"
	RepositorySourceApprovedCommunity = "approved_community"
	RepositorySourceExternal          = "external"

	IncludeApprovedCommunityPluginsSetting = "plugins.include_approved_community_plugins"
	MigratedApprovedCommunityCountSetting  = "plugins.approved_community_migrated_plugin_count"
)

type managedRepositoryDefinition struct {
	Key         string
	URL         string
	DisplayName string
	SourceKind  string
}

var managedRepositoryDefinitions = []managedRepositoryDefinition{
	{
		Key:         OfficialRepositoryManagedKey,
		URL:         DefaultRepositoryURL,
		DisplayName: DefaultRepositoryName,
		SourceKind:  RepositorySourceSilo,
	},
	{
		Key:         ApprovedCommunityRepositoryManagedKey,
		URL:         ApprovedCommunityRepositoryURL,
		DisplayName: ApprovedCommunityRepositoryName,
		SourceKind:  RepositorySourceApprovedCommunity,
	},
	{
		Key:         ForkVirtualRepositoryManagedKey,
		URL:         ForkVirtualRepositoryURL,
		DisplayName: ForkVirtualRepositoryName,
		SourceKind:  RepositorySourceSilo,
	},
}

var approvedCommunityPluginIDs = map[string]struct{}{
	"silo.requests.arr":   {},
	"silo.requests.seerr": {},
}

func isApprovedCommunityPlugin(pluginID string) bool {
	_, ok := approvedCommunityPluginIDs[pluginID]
	return ok
}
