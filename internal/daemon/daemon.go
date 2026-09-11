package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"relay/internal/manifest"
	"relay/internal/paths"
	"relay/internal/protocol"
	"relay/internal/registry"
	"relay/pkg/relay"
)

// defaultToolTimeout bounds how long the daemon waits for an installed tool to
// answer a discovery request.
const defaultToolTimeout = 10 * time.Second

// Config configures a daemon instance.
type Config struct {
	// Layout is the Relay home this daemon owns.
	Layout paths.Layout
	// Version is the daemon's own version string.
	Version string
	// Executors maps a manifest protocol.type to its implementation. A protocol
	// with no entry is rejected with PROTOCOL_ERROR rather than guessed at
	// (spec §19).
	Executors map[string]protocol.Executor
	// ToolTimeout bounds a tool's discovery reply. Defaults to 10s.
	ToolTimeout time.Duration
	// Now is the clock, injectable so uptime and install timestamps are testable.
	Now func() time.Time
	// Socket overrides the socket path. Empty means the layout's default, which
	// is what every caller except a test or an explicit --socket wants.
	Socket string
	// Log receives daemon diagnostics. Defaults to stderr.
	Log io.Writer
}

// Daemon serves Relay operations over IPC.
type Daemon struct {
	layout      paths.Layout
	version     string
	store       *registry.Store
	executors   map[string]protocol.Executor
	toolTimeout time.Duration
	now         func() time.Time
	started     time.Time
	socket      string
	logw        io.Writer
}

// New builds a daemon. The daemon owns its Relay home: it is the only writer of
// the registry, so a second daemon on one home would be a correctness bug rather
// than a performance choice (spec §3.3).
func New(cfg Config) *Daemon {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	timeout := cfg.ToolTimeout
	if timeout <= 0 {
		timeout = defaultToolTimeout
	}
	executors := cfg.Executors
	if executors == nil {
		executors = map[string]protocol.Executor{}
	}
	logw := cfg.Log
	if logw == nil {
		logw = os.Stderr
	}
	socket := cfg.Socket
	if socket == "" {
		socket = cfg.Layout.Socket()
	}
	return &Daemon{
		layout:      cfg.Layout,
		version:     cfg.Version,
		store:       registry.New(cfg.Layout.Registry),
		executors:   executors,
		toolTimeout: timeout,
		now:         now,
		started:     now(),
		socket:      socket,
		logw:        logw,
	}
}

// Ping reports whether a daemon answers on the configured socket.
func (d *Daemon) Ping() bool {
	connection, err := dial(d.socket)
	if err != nil {
		return false
	}
	connection.Close()
	return true
}

// Handle implements the IPC handler contract: it decodes a frame's kind and
// dispatches it.
//
// Application failures are returned as structured values inside a normal reply,
// never as errors, so a bad request cannot break the connection or the daemon.
func (d *Daemon) Handle(ctx context.Context, kind string, frame []byte) (any, error) {
	switch kind {
	case relay.FrameHello:
		var request relay.HelloRequest
		if failure := decode(frame, &request); failure != nil {
			return failedHello(failure), nil
		}
		return d.hello(request), nil

	case relay.FrameInvoke:
		var request relay.InvokeRequest
		if failure := decode(frame, &request); failure != nil {
			return relay.InvokeResponse{Success: false, Error: failure}, nil
		}
		return d.invoke(ctx, request), nil

	case relay.FrameList:
		return d.list(), nil

	case relay.FrameInspect:
		var request relay.InspectRequest
		if failure := decode(frame, &request); failure != nil {
			return failedInspect(failure), nil
		}
		return d.inspect(ctx, request.Tool), nil

	case relay.FrameStatus:
		return d.status(), nil

	case relay.FrameRegister:
		var request relay.RegisterRequest
		if failure := decode(frame, &request); failure != nil {
			return failedMutation(failure), nil
		}
		return d.register(ctx, request), nil

	case relay.FrameRemove:
		var request relay.RemoveRequest
		if failure := decode(frame, &request); failure != nil {
			return failedMutation(failure), nil
		}
		return d.remove(request.Tool), nil

	default:
		return map[string]any{
			"success": false,
			"error": relay.NewError(relay.CodeInvalidInput,
				fmt.Sprintf("unknown frame type %q", kind)),
		}, nil
	}
}

// hello is the version handshake. A client built against an incompatible
// runtime contract is told immediately, rather than failing later in a way that
// looks like a tool bug (spec §35).
func (d *Daemon) hello(request relay.HelloRequest) relay.HelloResponse {
	response := relay.HelloResponse{
		Success: true,
		Server:  d.serverInfo(),
		Runtime: relay.RuntimeInfo{Name: relay.RuntimeName, APIVersion: relay.RuntimeAPIVersion},
		Tools:   []relay.Installation{},
	}
	if !relay.CompatibleRuntimeAPIVersion(request.Client.RuntimeAPIVersion, relay.RuntimeAPIVersion) {
		response.Success = false
		response.Error = relay.NewError(relay.CodeRuntimeIncompatible,
			fmt.Sprintf("this client speaks Relay runtime %s; the daemon provides %s",
				request.Client.RuntimeAPIVersion, relay.RuntimeAPIVersion))
		return response
	}
	installations, err := d.store.List()
	if err != nil {
		response.Success = false
		response.Error = relay.NewError(relay.CodeRemoteError, err.Error())
		return response
	}
	if installations != nil {
		response.Tools = installations
	}
	return response
}

// invoke executes one declared operation.
//
// The order matters: the tool must be registered, its manifest must be readable
// and compatible, the operation must exist, and the input must validate before
// any network traffic happens. That way a caller gets a precise error instead of
// a confusing remote failure (spec §18, §26).
func (d *Daemon) invoke(ctx context.Context, request relay.InvokeRequest) relay.InvokeResponse {
	installation, err := d.store.Get(request.Tool)
	if err != nil {
		return invokeFailure(unknownTool(err, request.Tool))
	}
	if request.Operation == "" {
		return invokeFailure(relay.NewError(relay.CodeInvalidInput, "no operation was given"))
	}

	doc, failure := d.readManifest(ctx, installation.Path)
	if failure != nil {
		return invokeFailure(failure)
	}

	operation := doc.Operation(request.Operation)
	if operation == nil {
		return invokeFailure(relay.NewError(relay.CodeOperationNotFound,
			fmt.Sprintf("tool %q has no operation %q", request.Tool, request.Operation)).
			WithDetails(map[string]any{"available": doc.OperationNames()}))
	}

	if failure := operation.ValidateInput(request.Input); failure != nil {
		return invokeFailure(failure)
	}

	executor, implemented := d.executors[doc.Protocol.Type]
	if !implemented {
		return invokeFailure(relay.NewError(relay.CodeProtocolError,
			fmt.Sprintf("this Relay build has no executor for protocol %q", doc.Protocol.Type)))
	}

	response, err := executor.Execute(ctx, protocol.Request{
		Tool:       installation.Name,
		Operation:  operation.Name,
		Input:      request.Input,
		Credential: d.credential(doc, installation),
		Spec:       specFor(doc, operation),
	})
	if err != nil {
		var structured *relay.Error
		if errors.As(err, &structured) {
			return invokeFailure(structured)
		}
		return invokeFailure(relay.NewError(relay.CodeProtocolError, err.Error()))
	}

	return relay.InvokeResponse{Success: true, Result: response.Body}
}

// credential resolves the secret to inject at execution time.
//
// Keychain lookup lands with TASKS.md milestone M4. Until then a tool that
// declares auth runs unauthenticated, and a service that rejects an anonymous
// request produces AUTH_REQUIRED through the normal error mapping. That keeps the
// mapping honest instead of inventing a failure the service never reported
// (spec §21, §26).
func (d *Daemon) credential(*manifest.Document, relay.Installation) *protocol.Credential {
	return nil
}

// list reports the registry.
func (d *Daemon) list() relay.ListResponse {
	installations, err := d.store.List()
	if err != nil {
		return failedList(relay.NewError(relay.CodeRemoteError, err.Error()))
	}
	if installations == nil {
		installations = []relay.Installation{}
	}
	return relay.ListResponse{Success: true, Tools: installations}
}

// inspect returns a tool's descriptor.
//
// The descriptor is read from the binary every time. The registry holds
// discovery metadata, not schemas, so serving the registry copy here would
// create exactly the second source of truth that spec §16 forbids.
func (d *Daemon) inspect(ctx context.Context, name string) relay.InspectResponse {
	installation, err := d.store.Get(name)
	if err != nil {
		return failedInspect(unknownTool(err, name))
	}
	descriptor, failure := d.readDescriptor(ctx, installation.Path)
	if failure != nil {
		return failedInspect(failure)
	}
	return relay.InspectResponse{
		Success:    true,
		Descriptor: &descriptor,
		Install:    &installation,
	}
}

// status reports the daemon's health (spec §3.3).
func (d *Daemon) status() relay.StatusResponse {
	response := relay.StatusResponse{
		Success: true,
		Server:  d.serverInfo(),
		Runtime: relay.RuntimeInfo{Name: relay.RuntimeName, APIVersion: relay.RuntimeAPIVersion},
	}
	if names, err := d.store.Names(); err == nil {
		response.Tools = len(names)
	}
	response.UptimeSeconds = d.now().Sub(d.started).Seconds()
	return response
}

func (d *Daemon) serverInfo() relay.ServerInfo {
	return relay.ServerInfo{
		Name:      relay.RuntimeName,
		Version:   d.version,
		Pid:       os.Getpid(),
		Socket:    d.socket,
		StartedAt: d.started,
	}
}

// specFor projects an operation's manifest request block into the
// protocol-neutral shape an Executor consumes (spec §19).
func specFor(doc *manifest.Document, operation *manifest.Tool) protocol.Spec {
	return protocol.Spec{
		Type:     doc.Protocol.Type,
		BaseURL:  doc.Protocol.BaseURL,
		Endpoint: doc.Protocol.Endpoint,
		Method:   operation.Request.Method,
		Path:     operation.Request.Path,
		Query:    operation.Request.Query,
		Headers:  operation.Request.Headers,
		Body:     operation.Request.Body,
	}
}

// decode unmarshals a frame, reporting a structured error on failure.
func decode(frame []byte, target any) *relay.Error {
	if err := json.Unmarshal(frame, target); err != nil {
		return relay.NewError(relay.CodeInvalidInput,
			fmt.Sprintf("malformed %T request: %v", target, err))
	}
	return nil
}

func invokeFailure(err *relay.Error) relay.InvokeResponse {
	return relay.InvokeResponse{Success: false, Error: err}
}

func failedHello(err *relay.Error) relay.HelloResponse {
	return relay.HelloResponse{Success: false, Error: err}
}

func failedList(err *relay.Error) relay.ListResponse {
	return relay.ListResponse{Success: false, Error: err}
}

func failedInspect(err *relay.Error) relay.InspectResponse {
	return relay.InspectResponse{Success: false, Error: err}
}

func failedMutation(err *relay.Error) relay.MutationResponse {
	return relay.MutationResponse{Success: false, Error: err}
}

// unknownTool turns a registry lookup failure into the right structured code: a
// malformed name is bad input, a missing record is a missing tool.
func unknownTool(err error, name string) *relay.Error {
	if errors.Is(err, registry.ErrNotFound) {
		return relay.NewError(relay.CodeToolNotFound,
			fmt.Sprintf("tool %q is not registered; run 'relay install <path>'", name))
	}
	return relay.NewError(relay.CodeInvalidInput, err.Error())
}
