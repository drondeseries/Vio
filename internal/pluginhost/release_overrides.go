package pluginhost

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

type releaseOverrideReader interface {
	LookupReleaseOverrides(context.Context, []catalog.ReleaseIdentity) ([]catalog.ReleaseOverride, error)
}

type releaseOverrideRPC interface {
	Lookup(context.Context, *structpb.Struct) (*structpb.Struct, error)
}

type releaseOverrideServer struct {
	reader         releaseOverrideReader
	installationID int
}

func (s *releaseOverrideServer) Lookup(ctx context.Context, request *structpb.Struct) (*structpb.Struct, error) {
	if s.installationID <= 0 {
		return nil, status.Error(codes.PermissionDenied, "plugin installation is not bound")
	}
	if request == nil {
		return nil, status.Error(codes.InvalidArgument, "lookup is required")
	}
	if s.reader == nil {
		return nil, status.Error(codes.Unavailable, "release overrides unavailable")
	}
	data, err := request.MarshalJSON()
	if err != nil || len(data) > 64<<10 {
		return nil, status.Error(codes.InvalidArgument, "invalid lookup")
	}
	var input struct {
		Identities []catalog.ReleaseIdentity `json:"identities"`
	}
	if err := json.Unmarshal(data, &input); err != nil || len(input.Identities) < 1 || len(input.Identities) > 100 {
		return nil, status.Error(codes.InvalidArgument, "identities must contain 1 to 100 entries")
	}
	for _, id := range input.Identities {
		if id.Validate() != nil {
			return nil, status.Error(codes.InvalidArgument, "invalid release identity")
		}
	}
	entries, err := s.reader.LookupReleaseOverrides(ctx, input.Identities)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "release override lookup failed")
	}
	out := make([]any, 0, len(entries))
	for _, entry := range entries {
		var releaseAt any
		if entry.ReleaseAt != nil {
			releaseAt = entry.ReleaseAt.UTC().Format("2006-01-02T15:04:05.999999Z07:00")
		}
		out = append(out, map[string]any{"release_at": releaseAt, "revision": strconv.FormatInt(entry.Revision, 10)})
	}
	return structpb.NewStruct(map[string]any{"entries": out})
}

func registerReleaseOverrideRPC(server *grpc.Server, reader releaseOverrideReader, installationID int) {
	server.RegisterService(&grpc.ServiceDesc{
		ServiceName: "silo.plugin.v1.ReleaseOverrides",
		HandlerType: (*releaseOverrideRPC)(nil),
		Methods: []grpc.MethodDesc{{MethodName: "Lookup", Handler: func(srv any, ctx context.Context, decode func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
			request := new(structpb.Struct)
			if err := decode(request); err != nil {
				return nil, err
			}
			handler := func(ctx context.Context, req any) (any, error) {
				service, ok := srv.(releaseOverrideRPC)
				if !ok {
					return nil, status.Error(codes.Internal, "invalid release service")
				}
				input, ok := req.(*structpb.Struct)
				if !ok {
					return nil, status.Error(codes.InvalidArgument, "invalid lookup")
				}
				return service.Lookup(ctx, input)
			}
			if interceptor == nil {
				return handler(ctx, request)
			}
			return interceptor(ctx, request, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/silo.plugin.v1.ReleaseOverrides/Lookup"}, handler)
		}}},
	}, &releaseOverrideServer{reader: reader, installationID: installationID})
}
