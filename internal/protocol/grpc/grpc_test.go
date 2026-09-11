package grpc

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"relay/internal/protocol"
	"relay/pkg/relay"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// The test service is described by a hand-built FileDescriptorProto rather than
// generated stubs, which is the whole point of the executor: a real reflection
// server serves this descriptor over the wire, exactly as it would for a tool
// with no generated Go code (spec §45).
const (
	testProtoFile = "relay/test/v1/echo.proto"
	testPackage   = "relay.test.v1"
	testService   = "relay.test.v1.EchoService"

	echoMethodPath = "/" + testService + "/Echo"
	failMethodPath = "/" + testService + "/Fail"
	slowMethodPath = "/" + testService + "/Slow"
)

// testFile is built once and registered in the process-wide descriptor registry
// so that reflection.Register can resolve it.
var testFile = buildTestFileDescriptor()

func buildTestFileDescriptor() protoreflect.FileDescriptor {
	field := func(name string, number int32) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{
			Name:   proto.String(name),
			Number: proto.Int32(number),
			Label:  descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:   descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
		}
	}
	method := func(name string) *descriptorpb.MethodDescriptorProto {
		return &descriptorpb.MethodDescriptorProto{
			Name:       proto.String(name),
			InputType:  proto.String("." + testPackage + ".EchoRequest"),
			OutputType: proto.String("." + testPackage + ".EchoReply"),
		}
	}
	descriptor := &descriptorpb.FileDescriptorProto{
		Name:    proto.String(testProtoFile),
		Package: proto.String(testPackage),
		Syntax:  proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("EchoRequest"), Field: []*descriptorpb.FieldDescriptorProto{field("message", 1)}},
			{Name: proto.String("EchoReply"), Field: []*descriptorpb.FieldDescriptorProto{field("message", 1)}},
		},
		Service: []*descriptorpb.ServiceDescriptorProto{
			{
				Name:   proto.String("EchoService"),
				Method: []*descriptorpb.MethodDescriptorProto{method("Echo"), method("Fail"), method("Slow")},
			},
		},
	}
	file, err := protodesc.NewFile(descriptor, nil)
	if err != nil {
		panic("build test descriptor: " + err.Error())
	}
	if err := protoregistry.GlobalFiles.RegisterFile(file); err != nil {
		panic("register test descriptor: " + err.Error())
	}
	return file
}

// testServiceInterface is the empty handler interface the manually registered
// ServiceDesc advertises.
type testServiceInterface interface{}

type testServiceImpl struct{}

// metaCapture records the metadata the server last received.
type metaCapture struct {
	mu sync.Mutex
	md metadata.MD
}

func (c *metaCapture) record(ctx context.Context) {
	md, _ := metadata.FromIncomingContext(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.md = md
}

func (c *metaCapture) get(key string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.md.Get(key)
}

// startTestServer stands up a real gRPC server with the test service and
// reflection enabled, returning its address.
func startTestServer(t *testing.T) (string, *metaCapture) {
	t.Helper()

	requestDescriptor := testFile.Messages().ByName("EchoRequest")
	replyDescriptor := testFile.Messages().ByName("EchoReply")
	capture := &metaCapture{}

	decode := func(ctx context.Context, dec func(any) error) (*dynamicpb.Message, error) {
		capture.record(ctx)
		in := dynamicpb.NewMessage(requestDescriptor)
		if err := dec(in); err != nil {
			return nil, err
		}
		return in, nil
	}
	reply := func(in *dynamicpb.Message) *dynamicpb.Message {
		out := dynamicpb.NewMessage(replyDescriptor)
		message := in.Get(requestDescriptor.Fields().ByName("message")).String()
		out.Set(replyDescriptor.Fields().ByName("message"), protoreflect.ValueOfString("echo:"+message))
		return out
	}

	serviceDescriptor := &grpc.ServiceDesc{
		ServiceName: testService,
		HandlerType: (*testServiceInterface)(nil),
		Methods: []grpc.MethodDesc{
			{MethodName: "Echo", Handler: func(_ any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
				in, err := decode(ctx, dec)
				if err != nil {
					return nil, err
				}
				return reply(in), nil
			}},
			{MethodName: "Fail", Handler: func(_ any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
				if _, err := decode(ctx, dec); err != nil {
					return nil, err
				}
				return nil, status.Error(codes.InvalidArgument, "missing required field")
			}},
			{MethodName: "Slow", Handler: func(_ any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
				in, err := decode(ctx, dec)
				if err != nil {
					return nil, err
				}
				select {
				case <-ctx.Done():
					return nil, status.FromContextError(ctx.Err()).Err()
				case <-time.After(2 * time.Second):
				}
				return reply(in), nil
			}},
		},
	}

	server := grpc.NewServer()
	server.RegisterService(serviceDescriptor, &testServiceImpl{})
	reflection.Register(server)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	return listener.Addr().String(), capture
}

func requireCode(t *testing.T, err error, want relay.Code) *relay.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with code %q, got nil", want)
	}
	var structured *relay.Error
	if !errors.As(err, &structured) {
		t.Fatalf("error is not *relay.Error: %T (%v)", err, err)
	}
	if structured.Code != want {
		t.Fatalf("code = %q, want %q (message: %s)", structured.Code, want, structured.Message)
	}
	return structured
}

func TestEchoUnary(t *testing.T) {
	address, _ := startTestServer(t)
	executor := New()

	response, err := executor.Execute(context.Background(), protocol.Request{
		Spec: protocol.Spec{
			Type:    "grpc",
			BaseURL: address,
			Path:    echoMethodPath,
			Body:    map[string]any{"message": "hello"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if response.Status != int(codes.OK) {
		t.Errorf("status = %d, want %d", response.Status, int(codes.OK))
	}
	body, ok := response.Body.(map[string]any)
	if !ok {
		t.Fatalf("body is %T, want map[string]any", response.Body)
	}
	if body["message"] != "echo:hello" {
		t.Errorf("message = %v, want echo:hello", body["message"])
	}
}

// A manifest may name the method with or without a leading slash.
func TestEchoUnaryWithoutLeadingSlash(t *testing.T) {
	address, _ := startTestServer(t)
	executor := New()

	response, err := executor.Execute(context.Background(), protocol.Request{
		Spec: protocol.Spec{
			Type:     "grpc",
			Endpoint: address,
			Path:     testService + "/Echo",
			Body:     map[string]any{"message": "no-slash"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	body := response.Body.(map[string]any)
	if body["message"] != "echo:no-slash" {
		t.Errorf("message = %v, want echo:no-slash", body["message"])
	}
}

func TestMetadataAndCredential(t *testing.T) {
	address, capture := startTestServer(t)
	executor := New()

	_, err := executor.Execute(context.Background(), protocol.Request{
		Spec: protocol.Spec{
			Type:    "grpc",
			BaseURL: address,
			Path:    echoMethodPath,
			Headers: map[string]string{"X-Test": "abc"},
			Body:    map[string]any{"message": "hi"},
		},
		Credential: &protocol.Credential{Type: "bearer", Secret: "tok", Scheme: "Bearer"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := capture.get("x-test"); len(got) != 1 || got[0] != "abc" {
		t.Errorf("x-test metadata = %v, want [abc]", got)
	}
	if got := capture.get("authorization"); len(got) != 1 || got[0] != "Bearer tok" {
		t.Errorf("authorization metadata = %v, want [Bearer tok]", got)
	}
}

func TestDeadlineFires(t *testing.T) {
	address, _ := startTestServer(t)
	executor := New()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := executor.Execute(ctx, protocol.Request{
		Spec: protocol.Spec{
			Type:    "grpc",
			BaseURL: address,
			Path:    slowMethodPath,
			Body:    map[string]any{"message": "slow"},
		},
	})
	requireCode(t, err, relay.CodeTimeout)
}

func TestNonOKStatusMapsToRemoteError(t *testing.T) {
	address, _ := startTestServer(t)
	executor := New()

	_, err := executor.Execute(context.Background(), protocol.Request{
		Spec: protocol.Spec{
			Type:    "grpc",
			BaseURL: address,
			Path:    failMethodPath,
			Body:    map[string]any{"message": "x"},
		},
	})
	structured := requireCode(t, err, relay.CodeRemoteError)
	if !strings.Contains(structured.Message, "InvalidArgument") {
		t.Errorf("message %q does not name the status code", structured.Message)
	}
}

func TestUnknownMethod(t *testing.T) {
	address, _ := startTestServer(t)
	executor := New()

	_, err := executor.Execute(context.Background(), protocol.Request{
		Spec: protocol.Spec{
			Type:    "grpc",
			BaseURL: address,
			Path:    "/" + testService + "/NoSuchMethod",
			Body:    map[string]any{"message": "x"},
		},
	})
	requireCode(t, err, relay.CodeProtocolError)
}

func TestMalformedManifestBlocks(t *testing.T) {
	address, _ := startTestServer(t)
	executor := New()

	tests := []struct {
		name string
		spec protocol.Spec
		code relay.Code
	}{
		{
			name: "missing address",
			spec: protocol.Spec{Type: "grpc", Path: echoMethodPath},
			code: relay.CodeInvalidInput,
		},
		{
			name: "missing method path",
			spec: protocol.Spec{Type: "grpc", BaseURL: address},
			code: relay.CodeProtocolError,
		},
		{
			name: "path without method",
			spec: protocol.Spec{Type: "grpc", BaseURL: address, Path: "/" + testService},
			code: relay.CodeProtocolError,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := executor.Execute(context.Background(), protocol.Request{Spec: test.spec})
			requireCode(t, err, test.code)
		})
	}
}

// A dead server is a transport failure, not a manifest problem, even though it
// fails during descriptor reflection (spec §26).
func TestTransportFailure(t *testing.T) {
	executor := New()

	_, err := executor.Execute(context.Background(), protocol.Request{
		Spec: protocol.Spec{
			Type:    "grpc",
			BaseURL: "127.0.0.1:1",
			Path:    echoMethodPath,
			Body:    map[string]any{"message": "x"},
		},
	})
	requireCode(t, err, relay.CodeNetworkError)
}

func TestParseMethodPath(t *testing.T) {
	tests := []struct {
		raw     string
		full    string
		service string
		method  string
		wantErr bool
	}{
		{raw: "/relay.test.v1.EchoService/Echo", full: "/relay.test.v1.EchoService/Echo", service: "relay.test.v1.EchoService", method: "Echo"},
		{raw: "relay.test.v1.EchoService/Echo", full: "/relay.test.v1.EchoService/Echo", service: "relay.test.v1.EchoService", method: "Echo"},
		{raw: "", wantErr: true},
		{raw: "/onlyservice", wantErr: true},
		{raw: "/svc/", wantErr: true},
	}
	for _, test := range tests {
		full, service, method, err := parseMethodPath(test.raw)
		if test.wantErr {
			if err == nil {
				t.Errorf("parseMethodPath(%q) = no error, want error", test.raw)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseMethodPath(%q) error: %v", test.raw, err)
			continue
		}
		if full != test.full || service != test.service || method != test.method {
			t.Errorf("parseMethodPath(%q) = (%q, %q, %q), want (%q, %q, %q)",
				test.raw, full, service, method, test.full, test.service, test.method)
		}
	}
}

// TestLiteralMethodMatchesPath proves an operation that names its gRPC method
// with the literal package/service/method triple resolves and calls the same
// method as one that smuggles the canonical path into request.path (spec §45).
func TestLiteralMethodMatchesPath(t *testing.T) {
	address, _ := startTestServer(t)
	executor := New()

	pathResponse, err := executor.Execute(context.Background(), protocol.Request{
		Spec: protocol.Spec{
			Type:    "grpc",
			BaseURL: address,
			Path:    echoMethodPath,
			Body:    map[string]any{"message": "hello"},
		},
	})
	if err != nil {
		t.Fatalf("path-addressed call failed: %v", err)
	}

	literalResponse, err := executor.Execute(context.Background(), protocol.Request{
		Spec: protocol.Spec{
			Type:    "grpc",
			BaseURL: address,
			Package: testPackage,
			Service: "EchoService",
			Method:  "Echo",
			Body:    map[string]any{"message": "hello"},
		},
	})
	if err != nil {
		t.Fatalf("literal-addressed call failed: %v", err)
	}

	pathBody := pathResponse.Body.(map[string]any)
	literalBody := literalResponse.Body.(map[string]any)
	if literalBody["message"] != pathBody["message"] {
		t.Errorf("literal body %v differs from path body %v", literalBody["message"], pathBody["message"])
	}
	if literalBody["message"] != "echo:hello" {
		t.Errorf("literal message = %v, want echo:hello", literalBody["message"])
	}
}

// TestResolveMethod covers the two addressing forms and the malformed literal
// block they share (spec §45).
func TestResolveMethod(t *testing.T) {
	tests := []struct {
		name    string
		spec    protocol.Spec
		full    string
		service string
		method  string
		wantErr bool
	}{
		{
			name:    "path fallback",
			spec:    protocol.Spec{Path: echoMethodPath},
			full:    echoMethodPath,
			service: testService,
			method:  "Echo",
		},
		{
			name:    "path fallback ignores legacy http verb",
			spec:    protocol.Spec{Method: "POST", Path: echoMethodPath},
			full:    echoMethodPath,
			service: testService,
			method:  "Echo",
		},
		{
			name:    "literals assemble the canonical path",
			spec:    protocol.Spec{Package: testPackage, Service: "EchoService", Method: "Echo"},
			full:    echoMethodPath,
			service: testService,
			method:  "Echo",
		},
		{
			name:    "literals without a service",
			spec:    protocol.Spec{Package: testPackage, Method: "Echo"},
			wantErr: true,
		},
		{
			name:    "literals without a method",
			spec:    protocol.Spec{Package: testPackage, Service: "EchoService"},
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			full, service, method, err := resolveMethod(test.spec)
			if test.wantErr {
				if err == nil {
					t.Fatalf("resolveMethod(%+v) = no error, want error", test.spec)
				}
				requireCode(t, err, relay.CodeProtocolError)
				return
			}
			if err != nil {
				t.Fatalf("resolveMethod(%+v) error: %v", test.spec, err)
			}
			if full != test.full || service != test.service || method != test.method {
				t.Errorf("resolveMethod(%+v) = (%q, %q, %q), want (%q, %q, %q)",
					test.spec, full, service, method, test.full, test.service, test.method)
			}
		})
	}
}
