// Package mcp adapts the registry and the existing execution path to the
// Model Context Protocol.
//
// MCP is an adapter, not a second execution engine: discovery and invocation
// must reuse the same path as the CLI, and MCP must inherit the same auth
// and permission model with no MCP-only credential route (spec §27, §29,
// §41).
//
// The server implements a stdio MCP transport: JSON-RPC 2.0 over
// newline-delimited stdin/stdout. stdout carries protocol frames only;
// all diagnostics go to a separate log writer (spec §10).
//
// Tool naming follows the §28 convention: "{tool}_{operation}".
//
// See TASKS.md milestone M6.
package mcp
