// Package auth resolves credentials on behalf of tools. Generated binaries
// never manage secrets directly; the daemon looks up the credential and injects
// it at execution time (spec §21, §22).
//
// See TASKS.md milestone M4.
package auth
