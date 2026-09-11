// Package mcp adapts the registry and the existing execution path to the Model
// Context Protocol.
//
// MCP is an adapter, not a second execution engine: discovery and invocation
// must reuse the same path as the CLI, and MCP must inherit the same auth and
// permission model with no MCP-only credential route (spec §27, §29, §41).
//
// See TASKS.md milestone M6.
package mcp
