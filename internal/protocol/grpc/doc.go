// Package grpc implements the gRPC executor behind the same protocol.Executor
// seam as REST and GraphQL, keeping the capability abstraction identical
// (spec §19, §45).
//
// The executor speaks the standard gRPC wire protocol with the proto codec and
// resolves message descriptors through server reflection at call time, so a
// tool needs no generated Go stubs. Requests and responses cross the boundary
// as protojson, giving the daemon, CLI, and MCP surfaces the same JSON shape
// REST and GraphQL already return.
//
// The server address comes from the protocol block's baseUrl or endpoint, and
// the gRPC method is named in the operation's request block either literally as
// request.package / request.service / request.method, which the executor
// assembles into /package.Service/Method, or as that canonical path in
// request.path, which it parses as before. The capability model is untouched:
// this package only adds an Executor (spec §44, §45).
//
// See TASKS.md milestone M10.
package grpc
