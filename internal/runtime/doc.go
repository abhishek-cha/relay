// Package runtime holds the generic tool runtime: it loads the embedded
// manifest and skill, parses the CLI invocation, validates input, and forwards
// the operation to the daemon over IPC.
//
// It deliberately contains no endpoint-specific code. One consistent runtime
// for every tool is the whole product (spec §13).
//
// See TASKS.md milestones M1 and M2.
package runtime
