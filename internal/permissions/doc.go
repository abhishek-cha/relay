// Package permissions enforces the capability model. Tools declare the
// capabilities they need, and a tool asking for a new capability is not
// silently granted it. The daemon is the security boundary (spec §24, §25, §40).
//
// See TASKS.md milestone M11.
package permissions
