// Package registry manages discovery metadata for installed tools under
// ~/.relay/registry/.
//
// The registry is discovery metadata only. The tool binary stays authoritative
// for its schema, so descriptors are re-read from the binary rather than cached
// as truth (spec §15, §16).
//
// See TASKS.md milestone M2.
package registry
