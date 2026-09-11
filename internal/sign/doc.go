// Package sign implements Relay's tool signing and trust model (spec §48, §49).
//
// # What a signature covers
//
// A signature binds a publisher's ed25519 key to a tool's identity. It covers
// three things, and a signature that did not cover the binary would be
// worthless, because the binary is what actually executes:
//
//   - the manifest: the exact bytes the tool prints for `--manifest`, which are
//     the bytes embedded at build time;
//   - the descriptor: the exact bytes the tool prints for `--describe`;
//   - the binary: the SHA-256 digest of the tool executable on disk.
//
// The bytes handed to ed25519 are a small, line-oriented, domain-separated
// message built by [CanonicalBytes]:
//
//	relay-signature/v1
//	name: <descriptor name>
//	version: <descriptor version>
//	publisher: <publisher label>
//	manifest-sha256: <hex SHA-256 of the --manifest bytes>
//	descriptor-sha256: <hex SHA-256 of the --describe bytes>
//	binary-sha256: <hex SHA-256 of the tool binary>
//
// The leading schema line is a domain separator, so a signature made here can
// never be replayed as a signature over some other Relay message. Each field is
// terminated by a newline; name, version, and publisher are rejected if they
// contain a newline, so two different identities can never collide onto the
// same byte string.
//
// # Self-contained envelopes, and their limit
//
// A [Signature] carries the public key that made it, so verification needs no
// external keyring: it proves the manifest, descriptor, and binary have not
// changed since the holder of that key signed them. That is tamper-evidence, and
// it is what makes a level of "verified". It does not, on its own, prove who the
// publisher is, because nothing anchors the key to a name. Elevating a tool to
// "trusted" therefore takes an explicit local decision — see the trust policy
// below.
//
// # Trust levels (spec §49)
//
// A tool's trust level is decided by [Decide], in this order:
//
//  1. blocked  — a rule in the local policy's `blocked` list matches the tool's
//     name, publisher, or signing key fingerprint. Local policy wins
//     over every other signal: a blocked tool is refused at install
//     even if its signature verifies.
//  2. trusted  — a rule in the local policy's `trusted` list matches. This is
//     the operator's explicit local allowlist, and it does not require
//     a signature.
//  3. verified — the tool carries no other local rule but its signature
//     verified. A valid signature implies verified.
//  4. unknown  — the default for an unsigned tool with no matching local rule.
//
// An unsigned tool is therefore unknown, never automatically rejected.
//
// The policy file is `trust.yaml` under the Relay home's config directory
// (`$RELAY_HOME/config/trust.yaml`, default `~/.relay/config/trust.yaml`):
//
//	trusted:
//	  - name: github
//	  - publisher: GitHub
//	  - key: <hex key fingerprint>
//	blocked:
//	  - name: malware
//
// A rule matches when every field it sets matches the tool; publisher and key
// compare case-insensitively. A missing policy file is an empty policy, not an
// error.
//
// # Error codes
//
// The install path refuses a bad signature with [CodeSignatureInvalid] and a
// blocked tool with [CodeToolBlocked]. Both are aliases of codes in the §26
// table in pkg/relay, so they travel on the wire exactly like every other
// structured error.
package sign
