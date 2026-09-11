package grpc

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"strings"
	"time"

	"relay/internal/protocol"
	"relay/pkg/relay"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

// defaultTimeout bounds a single gRPC call when the caller's context carries no
// earlier deadline. The daemon supplies its own deadline; this is a backstop,
// matching the GraphQL executor (spec §26).
const defaultTimeout = 30 * time.Second

// Executor is a gRPC protocol executor that satisfies protocol.Executor.
//
// It holds no per-request state, so one Executor safely serves concurrent
// invocations.
type Executor struct {
	// DialOptions are the gRPC dial options used when connecting to a server.
	// When empty, the executor chooses transport credentials from the address
	// scheme: https/grpcs use TLS, everything else is plaintext. When set, the
	// caller owns every option, including credentials, which lets tests inject
	// a custom dialer.
	DialOptions []grpc.DialOption
}

// New returns an Executor with sensible defaults.
func New() *Executor {
	return &Executor{}
}

// Execute performs one unary gRPC call and returns its response as JSON.
func (e *Executor) Execute(ctx context.Context, req protocol.Request) (protocol.Response, error) {
	spec := req.Spec

	address := spec.BaseURL
	if address == "" {
		address = spec.Endpoint
	}
	if address == "" {
		return protocol.Response{}, relay.NewError(
			relay.CodeInvalidInput,
			"BaseURL or Endpoint must be set for a grpc operation",
		)
	}

	fullMethod, service, method, err := resolveMethod(spec)
	if err != nil {
		return protocol.Response{}, err
	}

	// A gRPC call takes its deadline from the caller's context; add a backstop
	// only when the caller supplied none (spec §26).
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultTimeout)
		defer cancel()
	}

	conn, err := e.dial(address)
	if err != nil {
		return protocol.Response{}, relay.NewError(
			relay.CodeNetworkError,
			"failed to dial grpc server: "+err.Error(),
		).WithDetails(map[string]any{"endpoint": address, "method": fullMethod})
	}
	defer conn.Close()

	request, response, err := resolveMessages(ctx, conn, service, method)
	if err != nil {
		return protocol.Response{}, mapReflectionError(ctx, err, address, fullMethod)
	}

	payload := spec.Body
	if payload == nil {
		payload = req.Input
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return protocol.Response{}, relay.NewError(
			relay.CodeInvalidInput,
			"failed to encode grpc request message: "+err.Error(),
		).WithDetails(map[string]any{"method": fullMethod})
	}
	if err := protojson.Unmarshal(payloadJSON, request); err != nil {
		return protocol.Response{}, relay.NewError(
			relay.CodeInvalidInput,
			"grpc request message does not match the server's descriptor: "+err.Error(),
		).WithDetails(map[string]any{"method": fullMethod})
	}

	ctx = withOutgoingMetadata(ctx, spec.Headers, req.Credential)

	var headerMD, trailerMD metadata.MD
	if err := conn.Invoke(ctx, fullMethod, request, response,
		grpc.Header(&headerMD), grpc.Trailer(&trailerMD)); err != nil {
		return protocol.Response{}, mapStatusError(ctx, err, address, fullMethod)
	}

	responseJSON, err := protojson.Marshal(response)
	if err != nil {
		return protocol.Response{}, relay.NewError(
			relay.CodeProtocolError,
			"failed to encode grpc response message: "+err.Error(),
		).WithDetails(map[string]any{"method": fullMethod})
	}
	var body any
	if err := json.Unmarshal(responseJSON, &body); err != nil {
		return protocol.Response{}, relay.NewError(
			relay.CodeProtocolError,
			"grpc response message is not valid JSON: "+err.Error(),
		).WithDetails(map[string]any{"method": fullMethod})
	}

	return protocol.Response{
		Status:  int(codes.OK),
		Headers: mergeMetadata(headerMD, trailerMD),
		Body:    body,
	}, nil
}

// dial opens a client connection to the server. The connection is lazy: a
// transport failure surfaces on the first RPC as codes.Unavailable, which
// Execute maps to NETWORK_ERROR.
func (e *Executor) dial(address string) (*grpc.ClientConn, error) {
	options := e.DialOptions
	target := address
	if len(options) == 0 {
		secure := false
		switch {
		case strings.HasPrefix(address, "https://"), strings.HasPrefix(address, "grpcs://"):
			secure = true
			target = address[strings.Index(address, "://")+3:]
		case strings.HasPrefix(address, "http://"), strings.HasPrefix(address, "grpc://"):
			target = address[strings.Index(address, "://")+3:]
		}
		if secure {
			options = []grpc.DialOption{
				grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})),
			}
		} else {
			options = []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
		}
	}
	return grpc.NewClient(target, options...)
}

// resolveMethod names the gRPC method to call. An operation that named its
// method literally (spec §45) carries Package, Service, and the RPC method name
// in Method, so the executor assembles /package.Service/Method from them. An
// operation addressed by path carries the canonical path in Path and is parsed
// exactly as before, so path-addressed manifests keep working unchanged.
func resolveMethod(spec protocol.Spec) (full, service, method string, err error) {
	if spec.Package != "" || spec.Service != "" {
		if spec.Package == "" || spec.Service == "" || spec.Method == "" {
			return "", "", "", relay.NewError(
				relay.CodeProtocolError,
				"grpc operation must declare request.package, request.service, and request.method together",
			).WithDetails(map[string]any{
				"package": spec.Package,
				"service": spec.Service,
				"method":  spec.Method,
			})
		}
		service = spec.Package + "." + spec.Service
		method = spec.Method
		return "/" + service + "/" + method, service, method, nil
	}
	return parseMethodPath(spec.Path)
}

// parseMethodPath splits a canonical gRPC method path, /package.Service/Method,
// into its full path, fully-qualified service name, and method name. A leading
// slash is optional.
func parseMethodPath(raw string) (full, service, method string, err error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", "", "", relay.NewError(
			relay.CodeProtocolError,
			"grpc operation must declare request.path as /package.Service/Method",
		)
	}
	if !strings.HasPrefix(trimmed, "/") {
		trimmed = "/" + trimmed
	}
	separator := strings.LastIndex(trimmed, "/")
	if separator <= 0 {
		return "", "", "", relay.NewError(
			relay.CodeProtocolError,
			"grpc request.path must be /package.Service/Method, got "+raw,
		).WithDetails(map[string]any{"path": raw})
	}
	service = trimmed[1:separator]
	method = trimmed[separator+1:]
	if service == "" || method == "" {
		return "", "", "", relay.NewError(
			relay.CodeProtocolError,
			"grpc request.path must be /package.Service/Method, got "+raw,
		).WithDetails(map[string]any{"path": raw})
	}
	return trimmed, service, method, nil
}

// withOutgoingMetadata attaches the manifest's headers and the daemon-injected
// credential to the outgoing context. gRPC metadata keys are lowercase, so the
// HTTP header names the other executors use are normalized here (spec §22).
func withOutgoingMetadata(ctx context.Context, headers map[string]string, credential *protocol.Credential) context.Context {
	pairs := make([]string, 0, len(headers)*2+2)
	for name, value := range headers {
		if name == "" {
			continue
		}
		pairs = append(pairs, strings.ToLower(name), value)
	}
	if credential != nil {
		name := credential.Header
		if name == "" {
			name = "Authorization"
		}
		value := credential.Secret
		if credential.Scheme != "" {
			value = credential.Scheme + " " + credential.Secret
		}
		pairs = append(pairs, strings.ToLower(name), value)
	}
	if len(pairs) == 0 {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, pairs...)
}

// mergeMetadata folds response headers and trailers into one map, matching the
// protocol.Response shape the REST executor returns.
func mergeMetadata(header, trailer metadata.MD) map[string][]string {
	merged := make(map[string][]string, len(header)+len(trailer))
	for name, values := range header {
		merged[name] = append([]string(nil), values...)
	}
	for name, values := range trailer {
		merged[name] = append(merged[name], values...)
	}
	return merged
}

// mapStatusError maps a failed RPC onto the shared error taxonomy (spec §26).
// A canceled or expired context is TIMEOUT; an unreachable server is
// NETWORK_ERROR; every other non-OK status is REMOTE_ERROR carrying the gRPC
// status code in its message.
func mapStatusError(ctx context.Context, err error, address, fullMethod string) error {
	details := map[string]any{"endpoint": address, "method": fullMethod}
	if ctx.Err() != nil {
		return relay.NewError(
			relay.CodeTimeout,
			"context deadline exceeded or cancelled",
		).WithDetails(details)
	}
	grpcStatus, ok := status.FromError(err)
	if !ok {
		return relay.NewError(
			relay.CodeNetworkError,
			"transport error: "+err.Error(),
		).WithDetails(details)
	}
	switch grpcStatus.Code() {
	case codes.DeadlineExceeded:
		return relay.NewError(relay.CodeTimeout, "grpc deadline exceeded").WithDetails(details)
	case codes.Unavailable, codes.Canceled:
		return relay.NewError(
			relay.CodeNetworkError,
			"grpc transport error: "+grpcStatus.Message(),
		).WithDetails(details)
	default:
		return relay.NewError(
			relay.CodeRemoteError,
			"grpc status "+grpcStatus.Code().String()+": "+grpcStatus.Message(),
		).WithDetails(details)
	}
}

// mapReflectionError maps a descriptor-resolution failure onto the taxonomy.
// The transport codes win first: an unreachable server fails during reflection
// too, and that is still NETWORK_ERROR, not a manifest problem. A name the
// server cannot resolve, or a server with no reflection service, is a
// malformed or unsupported manifest block, so it is PROTOCOL_ERROR.
func mapReflectionError(ctx context.Context, err error, address, fullMethod string) error {
	details := map[string]any{"endpoint": address, "method": fullMethod}
	if ctx.Err() != nil {
		return relay.NewError(
			relay.CodeTimeout,
			"context deadline exceeded or cancelled",
		).WithDetails(details)
	}
	if grpcStatus, ok := status.FromError(err); ok {
		switch grpcStatus.Code() {
		case codes.DeadlineExceeded:
			return relay.NewError(relay.CodeTimeout, "grpc deadline exceeded").WithDetails(details)
		case codes.Unavailable, codes.Canceled:
			return relay.NewError(
				relay.CodeNetworkError,
				"grpc transport error: "+grpcStatus.Message(),
			).WithDetails(details)
		case codes.Unimplemented:
			return relay.NewError(
				relay.CodeProtocolError,
				"grpc server does not support reflection",
			).WithDetails(details)
		}
	}
	return relay.NewError(
		relay.CodeProtocolError,
		"failed to resolve grpc method: "+err.Error(),
	).WithDetails(details)
}

// Compile-time assertion that the executor satisfies the seam.
var _ protocol.Executor = (*Executor)(nil)
