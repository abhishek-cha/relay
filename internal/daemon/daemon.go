package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"relay/internal/auth"
	"relay/internal/browser"
	"relay/internal/ipc"
	"relay/internal/keychain"
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
	// Keychain resolves tool credentials (spec §21, §22). It defaults to the
	// macOS Keychain; tests inject a fake Store so the daemon is constructible
	// without touching the real Keychain.
	Keychain keychain.Store
	// Telemetry records one local usage event per invocation (spec §31). A nil
	// recorder disables telemetry, which is what a directly constructed daemon
	// and the unit tests want; relayd injects the real one. Recording is
	// best-effort and can never fail an invocation.
	Telemetry UsageRecorder
	// ToolTimeout bounds a tool's discovery reply. Defaults to 10s.
	ToolTimeout time.Duration
	// DestructiveOps names the operations that require explicit confirmation
	// before they run (spec §25). It is daemon configuration because the manifest
	// schema has no field for destructiveness, so a tool cannot widen its own
	// confirmation surface by editing its manifest. A nil slice means "read
	// permissions.yaml" (falling back to DefaultDestructiveOps when the file is
	// absent), while a non-nil slice overrides the file outright — including an
	// explicitly empty slice, which gates nothing. See resolveDestructiveOps.
	DestructiveOps []string
	// Now is the clock, injectable so uptime and install timestamps are testable.
	Now func() time.Time
	// Socket overrides the socket path. Empty means the layout's default, which
	// is what every caller except a test or an explicit --socket wants.
	Socket string
	// Log receives daemon diagnostics. Defaults to stderr.
	Log io.Writer
	// Sessions owns the browser tools' login flows and cookie jars (spec §23).
	// The daemon must be handed the same instance its browser executor uses, so a
	// `session_login` frame and a later browser operation share one store; a nil
	// value leaves browser sessions unconfigured rather than silently separate.
	Sessions *browser.Sessions
}

// Daemon serves Relay operations over IPC.
type Daemon struct {
	layout         paths.Layout
	version        string
	store          *registry.Store
	resolver       *auth.Resolver
	usage          UsageRecorder
	executors      map[string]protocol.Executor
	toolTimeout    time.Duration
	destructiveOps []string
	configErr      error
	now            func() time.Time
	started        time.Time
	socket         string
	logw           io.Writer
	sessions       *browser.Sessions
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
	keychainStore := cfg.Keychain
	if keychainStore == nil {
		keychainStore = &keychain.SecurityStore{}
	}
	// The destructive list comes from daemon configuration, not the manifest, so
	// a tool cannot widen its own confirmation surface (spec §25). A malformed
	// policy file is a hard error: the daemon refuses to start rather than run
	// with a list the operator did not intend. The fallback here is the default
	// list, so even the error path is fail-closed for a caller that invokes the
	// daemon directly without Run.
	destructiveOps, configErr := resolveDestructiveOps(cfg.Layout.Config, cfg.DestructiveOps)
	if configErr != nil {
		destructiveOps = append([]string(nil), DefaultDestructiveOps...)
	}
	return &Daemon{
		layout:         cfg.Layout,
		version:        cfg.Version,
		store:          registry.New(cfg.Layout.Registry),
		resolver:       auth.New(keychainStore),
		usage:          cfg.Telemetry,
		executors:      executors,
		toolTimeout:    timeout,
		destructiveOps: destructiveOps,
		configErr:      configErr,
		now:            now,
		started:        now(),
		socket:         socket,
		logw:           logw,
		sessions:       cfg.Sessions,
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
		// Every invoke is decoded through ipc.InvokeFrame so the optional
		// confirmation acknowledgment is read from the same frame. A plain
		// relay.InvokeRequest (what a tool binary and the MCP adapter send)
		// decodes to an empty Confirmation and is never treated as confirmed
		// (spec §41).
		var request ipc.InvokeFrame
		if failure := decode(frame, &request); failure != nil {
			return relay.InvokeResponse{Success: false, Error: failure}, nil
		}
		return d.invokeWith(ctx, request.InvokeRequest, request.Confirmation), nil

	case relay.FrameList:
		return d.list(), nil

	case relay.FrameInspect:
		var request relay.InspectRequest
		if failure := decode(frame, &request); failure != nil {
			return failedInspect(failure), nil
		}
		return d.inspect(ctx, request.Tool), nil

	case ipc.FrameSkill:
		var request ipc.SkillRequest
		if failure := decode(frame, &request); failure != nil {
			return failedSkill(failure), nil
		}
		return d.skill(ctx, request.Tool), nil

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

	case relay.FrameAuthSet:
		var request relay.AuthSetRequest
		if failure := decode(frame, &request); failure != nil {
			return failedMutation(failure), nil
		}
		return d.authSet(ctx, request), nil

	case relay.FrameAuthClear:
		var request relay.AuthClearRequest
		if failure := decode(frame, &request); failure != nil {
			return failedMutation(failure), nil
		}
		return d.authClear(request), nil

	case relay.FrameAuthStatus:
		var request relay.AuthStatusRequest
		if failure := decode(frame, &request); failure != nil {
			return failedAuthStatus(failure), nil
		}
		return d.authStatus(ctx, request), nil

	case ipc.FrameAuthDeviceStart:
		var request ipc.AuthDeviceStartRequest
		if failure := decode(frame, &request); failure != nil {
			return failedDeviceStart(failure), nil
		}
		return d.authDeviceStart(ctx, request), nil

	case ipc.FrameAuthDeviceWait:
		var request ipc.AuthDeviceWaitRequest
		if failure := decode(frame, &request); failure != nil {
			return failedDeviceWait(failure), nil
		}
		return d.authDeviceWait(ctx, request), nil

	case ipc.FrameAuthBrowserStart:
		var request ipc.AuthBrowserStartRequest
		if failure := decode(frame, &request); failure != nil {
			return failedBrowserStart(failure), nil
		}
		return d.authBrowserStart(ctx, request), nil

	case ipc.FrameAuthBrowserWait:
		var request ipc.AuthBrowserWaitRequest
		if failure := decode(frame, &request); failure != nil {
			return failedBrowserWait(failure), nil
		}
		return d.authBrowserWait(ctx, request), nil

	case ipc.FrameAuthRefresh:
		var request ipc.AuthRefreshRequest
		if failure := decode(frame, &request); failure != nil {
			return failedAuthRefresh(failure), nil
		}
		return d.authRefresh(ctx, request), nil

	case ipc.FrameSessionLogin:
		var request ipc.SessionLoginRequest
		if failure := decode(frame, &request); failure != nil {
			return failedSessionLogin(failure), nil
		}
		return d.sessionLogin(ctx, request), nil

	case ipc.FrameSessionClear:
		var request ipc.SessionClearRequest
		if failure := decode(frame, &request); failure != nil {
			return failedSessionClear(failure), nil
		}
		return d.sessionClear(request), nil

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

// invoke executes one declared operation with no confirmation. It is the
// unconfirmed seam the unit tests drive directly; the IPC surface goes through
// invokeWith so a frame's acknowledgement is honored (spec §25).
func (d *Daemon) invoke(ctx context.Context, request relay.InvokeRequest) relay.InvokeResponse {
	return d.invokeWith(ctx, request, "")
}

// invokeWith executes one declared operation, resolving the destructive gate
// with confirmation when the caller supplied it (spec §25).
//
// The order matters: the tool must be registered, its manifest must be readable
// and compatible, the operation must exist, and the input must validate before
// any network traffic happens. The permission boundary then runs before the
// credential is read and before protocol dispatch (spec §18). That way a caller
// gets a precise error instead of a confusing remote failure (spec §18, §26).
func (d *Daemon) invokeWith(ctx context.Context, request relay.InvokeRequest, confirmation string) relay.InvokeResponse {
	started := d.now()
	response := d.invokeOperation(ctx, request, confirmation)
	d.recordUsage(request, response, d.now().Sub(started))
	return response
}

// invokeOperation runs the ordered pre-flight checks and then dispatches to the
// protocol executor. confirmation is the caller's explicit acknowledgment of a
// destructive operation; it resolves only the destructive gate and never an
// entitlement denial (spec §25, §40).
func (d *Daemon) invokeOperation(ctx context.Context, request relay.InvokeRequest, confirmation string) relay.InvokeResponse {
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

	// The permission boundary runs before any credential is read and before a
	// protocol executor is handed the call (spec §18, §40): an operation the
	// manifest is not entitled to run is refused here, so no secret leaves the
	// Keychain and no request leaves the daemon on its behalf. This sits inside
	// the timed invoke, so a denied call is still recorded as usage (spec §31).
	if failure := d.permissionCheck(doc, request.Tool, operation, request.Input, confirmation); failure != nil {
		return invokeFailure(failure)
	}

	executor, implemented := d.executors[doc.Protocol.Type]
	if !implemented {
		return invokeFailure(relay.NewError(relay.CodeProtocolError,
			fmt.Sprintf("this Relay build has no executor for protocol %q", doc.Protocol.Type)))
	}

	credential, failure := d.credential(doc, installation)
	if failure != nil {
		return invokeFailure(failure)
	}

	response, err := executor.Execute(ctx, protocol.Request{
		Tool:       installation.Name,
		Operation:  operation.Name,
		Input:      request.Input,
		Credential: credential,
		Paginate:   request.Paginate,
		Spec:       specFor(doc, operation),
	})
	if err != nil {
		var structured *relay.Error
		if errors.As(err, &structured) {
			// A 401 on a request that carried a daemon-injected credential means
			// the stored secret was refused, not that the user never logged in.
			// Only the daemon knows whether a credential was injected, so the
			// executor cannot make this distinction itself (spec §26).
			//
			// A browser tool is the exception: its executor never attaches the
			// credential to an operation and reports AUTH_REQUIRED only for a
			// missing session, so refining it would point the user at
			// 'relay auth login' instead of 'relay session login' (spec §23).
			if credential != nil && doc.Protocol.Type != "browser" && structured.Code == relay.CodeAuthRequired {
				structured = relay.NewError(relay.CodeAuthFailed,
					fmt.Sprintf("the service rejected the stored credential for %q; run 'relay auth login %s' to replace it",
						installation.Name, installation.Name)).
					WithDetails(structured.Details)
			}
			return invokeFailure(structured)
		}
		return invokeFailure(relay.NewError(relay.CodeProtocolError, err.Error()))
	}

	invoked := relay.InvokeResponse{Success: true, Result: response.Body}
	// Surface the walk's shape only when the caller opted into pagination, so an
	// ordinary invocation's reply stays byte-identical and a bounded walk is
	// never passed off as complete (spec §20).
	if request.Paginate {
		invoked.Pages = response.Pages
		invoked.Truncated = response.Truncated
	}
	return invoked
}

// credential resolves the secret to inject at execution time (spec §21, §22).
//
// The daemon owns credentials, so an installed tool never sees one: the
// resolver reads it from the Keychain and returns a presentation the executor
// attaches to the outbound request.
//
// This replaces the pre-M4 stub, which always returned nil. A tool that
// declares auth and has no stored credential now fails AUTH_REQUIRED before any
// request is sent, instead of silently running unauthenticated and letting the
// service answer. A tool that declares no auth is unaffected and still runs
// with no credential.
func (d *Daemon) credential(doc *manifest.Document, installation relay.Installation) (*protocol.Credential, *relay.Error) {
	return d.resolver.Resolve(doc, installation.Name)
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
//
// Endpoint is the Spec's protocol-specific destination slot. A remote protocol
// takes its endpoint from the protocol block; a local capability has no
// address, so it carries the primitive to run — the manifest's
// request.operation — in that slot (spec §46).
//
// Package and Service carry a gRPC operation's literal method addressing when
// the manifest declares it (spec §45); for a path-addressed operation they stay
// empty and the executor reads Path. Every other protocol leaves them empty.
func specFor(doc *manifest.Document, operation *manifest.Tool) protocol.Spec {
	endpoint := doc.Protocol.Endpoint
	if doc.Protocol.Type == "local" {
		endpoint = operation.Request.Operation
	}
	return protocol.Spec{
		Type:     doc.Protocol.Type,
		BaseURL:  doc.Protocol.BaseURL,
		Endpoint: endpoint,
		Method:   operation.Request.Method,
		Path:     operation.Request.Path,
		Package:  operation.Request.Package,
		Service:  operation.Request.Service,
		Query:    operation.Request.Query,
		Headers:  operation.Request.Headers,
		Body:     operation.Request.Body,

		Document:  operation.Request.Document,
		Variables: operation.Request.Variables,

		Pagination: paginationFor(operation.Request.Pagination),
	}
}

// paginationFor projects a manifest's declared pagination strategy into the
// transport-neutral shape an Executor reads (spec §19, §20). A nil manifest
// block projects to a nil spec block, so an operation that declares no strategy
// keeps executing as a single request.
func paginationFor(source *manifest.Pagination) *protocol.Pagination {
	if source == nil {
		return nil
	}
	return &protocol.Pagination{
		Style:        source.Style,
		CursorParam:  source.CursorParam,
		CursorIn:     source.CursorIn,
		CursorField:  source.CursorField,
		HasMoreField: source.HasMoreField,
		LimitParam:   source.LimitParam,
		Limit:        source.Limit,
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
