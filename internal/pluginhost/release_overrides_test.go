package pluginhost

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/structpb"
)

type overrideReaderStub struct{}

func (overrideReaderStub) LookupReleaseOverrides(_ context.Context, ids []catalog.ReleaseIdentity) ([]catalog.ReleaseOverride, error) {
	date := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	out := make([]catalog.ReleaseOverride, len(ids))
	for i := range out {
		out[i] = catalog.ReleaseOverride{Revision: 9007199254740993, ReleaseAt: &date, EvidenceNote: "private evidence", ActorAccountID: 42}
	}
	return out, nil
}

func TestReleaseOverrideRPCContract(t *testing.T) {
	server := grpc.NewServer()
	registerReleaseOverrideRPC(server, overrideReaderStub{}, 27)
	listener := bufconn.Listen(1 << 20)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///release-overrides", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	input, _ := structpb.NewStruct(map[string]any{"identities": []any{map[string]any{"media_type": "movie", "provider": "tmdb", "provider_id": "42"}}})
	output := new(structpb.Struct)
	if err := conn.Invoke(context.Background(), "/silo.plugin.v1.ReleaseOverrides/Lookup", input, output); err != nil {
		t.Fatal(err)
	}
	data, err := output.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"9007199254740993"`) || strings.Contains(string(data), "evidence") || strings.Contains(string(data), "actor") {
		t.Fatalf("unexpected response %s", data)
	}
	if err := conn.Invoke(context.Background(), "/silo.plugin.v1.ReleaseOverrides/Set", input, output); status.Code(err) != codes.Unimplemented {
		t.Fatalf("write method exposed: %v", err)
	}
	for _, tc := range []struct {
		server releaseOverrideServer
		input  *structpb.Struct
		code   codes.Code
	}{
		{releaseOverrideServer{reader: overrideReaderStub{}}, input, codes.PermissionDenied},
		{releaseOverrideServer{installationID: 1}, input, codes.Unavailable},
		{releaseOverrideServer{installationID: 1, reader: overrideReaderStub{}}, nil, codes.InvalidArgument},
		{releaseOverrideServer{installationID: 1, reader: overrideReaderStub{}}, new(structpb.Struct), codes.InvalidArgument},
	} {
		if _, err := tc.server.Lookup(context.Background(), tc.input); status.Code(err) != tc.code {
			t.Fatalf("got %v want %v", err, tc.code)
		}
	}
}
