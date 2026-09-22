// Command residentplugin is a test fixture for the resident plugin
// supervisor: it exits with status 3 as soon as the file named by
// SILO_TEST_PLUGIN_EXIT_FILE exists, so a test can make it crash on demand.
//
// NOTE (fork, stripped for SDK): the network_access_provider.v1 stub the
// fixture used to serve needs a newer plugin SDK, so the fixture only
// exercises process lifecycle until the SDK is updated. The manifest still
// declares the capability type string so resident-type matching keeps working.
package main

import (
	_ "embed"
	"os"
	"time"

	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
)

//go:embed manifest.json
var manifestJSON []byte

// version is the manifest version the fixture reports. Tests that need a
// second release of the same plugin build it with
// -ldflags "-X main.version=0.2.0".
var version = "0.1.0"

func main() {
	if exitFile := os.Getenv("SILO_TEST_PLUGIN_EXIT_FILE"); exitFile != "" {
		go func() {
			for {
				if _, err := os.Stat(exitFile); err == nil {
					os.Exit(3)
				}
				time.Sleep(25 * time.Millisecond)
			}
		}()
	}
	sdkruntime.ServeManifest(manifestJSON, version, sdkruntime.CapabilityServers{})
}
