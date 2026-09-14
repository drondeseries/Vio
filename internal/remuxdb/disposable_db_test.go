package remuxdb

import (
	"os"
	"testing"

	"github.com/Silo-Server/silo-server/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.SetupPackage(m, "remuxdb"))
}
