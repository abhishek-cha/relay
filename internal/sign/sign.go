package sign

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"relay/internal/fsutil"
	"relay/pkg/relay"
)

// Schema is the signature envelope's schema identifier, and also the first line
// of the signed message. It domain-separates a tool signature from every other
// message a Relay key might be asked to sign.
const Schema = "relay-signature/v1"

// Algorithm is the only signature algorithm this build emits or accepts.
const Algorithm = "ed25519"

// SignatureSuffix names the sidecar a tool's signature travels in. `relay sign`
// writes <tool>.sig next to the binary, and `relay install` looks for it there.
const SignatureSuffix = ".sig"

// Error codes produced by the install path (spec §48, §49). They are relay.Code
// values so they render identically to the §26 table; see the package doc for
// why they are declared here rather than in pkg/relay.
const (
	CodeSignatureInvalid relay.Code = "SIGNATURE_INVALID"
	CodeToolBlocked      relay.Code = "TOOL_BLOCKED"
)

// Signature is a publisher's signed claim about one tool build. It is
// self-contained: Verify needs no keyring, because the envelope carries the
// public key that must have made the signature.
type Signature struct {
	Schema         string    `json:"schema"`
	Algorithm      string    `json:"algorithm"`
	Publisher      string    `json:"publisher,omitempty"`
	PublicKey      string    `json:"publicKey"`
	KeyFingerprint string    `json:"keyFingerprint"`
	Signature      string    `json:"signature"`
	SignedAt       time.Time `json:"signedAt"`
}

// Message is the logical content a signature covers. Its canonical byte form is
// [CanonicalBytes].
type Message struct {
	Name             string
	Version          string
	Publisher        string
	ManifestSHA256   string
	DescriptorSHA256 string
	BinarySHA256     string
}

// CanonicalBytes renders the exact bytes that are signed. The layout is
// documented on the package, and must not change without bumping Schema: a
// signature is only meaningful against one fixed encoding.
func CanonicalBytes(msg Message) ([]byte, error) {
	for _, field := range []struct{ name, value string }{
		{"name", msg.Name},
		{"version", msg.Version},
		{"publisher", msg.Publisher},
		{"manifest-sha256", msg.ManifestSHA256},
		{"descriptor-sha256", msg.DescriptorSHA256},
		{"binary-sha256", msg.BinarySHA256},
	} {
		if strings.ContainsAny(field.value, "\r\n") {
			return nil, fmt.Errorf("signature field %s must not contain a newline", field.name)
		}
	}
	var builder strings.Builder
	builder.WriteString(Schema)
	builder.WriteByte('\n')
	fmt.Fprintf(&builder, "name: %s\n", msg.Name)
	fmt.Fprintf(&builder, "version: %s\n", msg.Version)
	fmt.Fprintf(&builder, "publisher: %s\n", msg.Publisher)
	fmt.Fprintf(&builder, "manifest-sha256: %s\n", msg.ManifestSHA256)
	fmt.Fprintf(&builder, "descriptor-sha256: %s\n", msg.DescriptorSHA256)
	fmt.Fprintf(&builder, "binary-sha256: %s\n", msg.BinarySHA256)
	return []byte(builder.String()), nil
}

// Sign signs a tool's identity with an ed25519 private key. manifest and
// descriptor are the raw bytes the tool prints for `--manifest` and
// `--describe`; binaryDigest is the SHA-256 of the tool binary (see
// [DigestFile]).
func Sign(private ed25519.PrivateKey, publisher, name, version string, manifest, descriptor []byte, binaryDigest string, now time.Time) (Signature, error) {
	if len(private) != ed25519.PrivateKeySize {
		return Signature{}, fmt.Errorf("private key is %d bytes, want %d", len(private), ed25519.PrivateKeySize)
	}
	public, ok := private.Public().(ed25519.PublicKey)
	if !ok {
		return Signature{}, errors.New("private key has no ed25519 public half")
	}
	payload, err := CanonicalBytes(Message{
		Name:             name,
		Version:          version,
		Publisher:        publisher,
		ManifestSHA256:   Digest(manifest),
		DescriptorSHA256: Digest(descriptor),
		BinarySHA256:     binaryDigest,
	})
	if err != nil {
		return Signature{}, err
	}
	return Signature{
		Schema:         Schema,
		Algorithm:      Algorithm,
		Publisher:      publisher,
		PublicKey:      EncodePublicKey(public),
		KeyFingerprint: KeyFingerprint(public),
		Signature:      base64.StdEncoding.EncodeToString(ed25519.Sign(private, payload)),
		SignedAt:       now.UTC(),
	}, nil
}

// Verify checks a signature against a tool's current bytes. It returns nil when
// the signature is well-formed and matches the manifest, descriptor, and binary
// digest, and an error describing the first mismatch otherwise.
func (s Signature) Verify(name, version string, manifest, descriptor []byte, binaryDigest string) error {
	if s.Schema != Schema {
		return fmt.Errorf("unsupported signature schema %q", s.Schema)
	}
	if s.Algorithm != Algorithm {
		return fmt.Errorf("unsupported signature algorithm %q", s.Algorithm)
	}
	public, err := ParsePublicKey(s.PublicKey)
	if err != nil {
		return err
	}
	// Recompute the fingerprint rather than trusting the envelope's copy: the
	// policy layer matches on the fingerprint, so it must describe this key.
	if fingerprint := KeyFingerprint(public); !strings.EqualFold(fingerprint, s.KeyFingerprint) {
		return errors.New("key fingerprint does not match the embedded public key")
	}
	signature, err := base64.StdEncoding.DecodeString(s.Signature)
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}
	if len(signature) != ed25519.SignatureSize {
		return fmt.Errorf("signature is %d bytes, want %d", len(signature), ed25519.SignatureSize)
	}
	payload, err := CanonicalBytes(Message{
		Name:             name,
		Version:          version,
		Publisher:        s.Publisher,
		ManifestSHA256:   Digest(manifest),
		DescriptorSHA256: Digest(descriptor),
		BinarySHA256:     binaryDigest,
	})
	if err != nil {
		return err
	}
	if !ed25519.Verify(public, payload, signature) {
		return errors.New("signature does not verify against the tool")
	}
	return nil
}

// Digest returns the hex SHA-256 of data.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// DigestFile returns the hex SHA-256 of a file's contents.
func DigestFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// KeyFingerprint is the hex SHA-256 of a public key, and the value a trust
// policy pins to recognize a publisher's key.
func KeyFingerprint(public ed25519.PublicKey) string {
	sum := sha256.Sum256(public)
	return hex.EncodeToString(sum[:])
}

// SignaturePath is where a tool's signature sidecar lives: the binary's path
// with a .sig suffix.
func SignaturePath(binary string) string { return binary + SignatureSuffix }

// Load reads a signature envelope from disk.
func Load(path string) (Signature, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Signature{}, err
	}
	var signature Signature
	if err := json.Unmarshal(data, &signature); err != nil {
		return Signature{}, fmt.Errorf("parse signature %s: %w", path, err)
	}
	return signature, nil
}

// Save writes a signature envelope atomically.
func Save(path string, signature Signature) error {
	encoded, err := json.MarshalIndent(signature, "", "  ")
	if err != nil {
		return fmt.Errorf("encode signature: %w", err)
	}
	return fsutil.WriteFile(path, append(encoded, '\n'), 0o644)
}

// GenerateKey returns a fresh ed25519 keypair.
func GenerateKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

// EncodePrivateKey renders a private key as the base64 text `relay keygen`
// writes to disk.
func EncodePrivateKey(private ed25519.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(private)
}

// EncodePublicKey renders a public key as base64 text.
func EncodePublicKey(public ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(public)
}

// ParsePrivateKey decodes the base64 text written by [EncodePrivateKey].
func ParsePrivateKey(text string) (ed25519.PrivateKey, error) {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(text))
	if err != nil {
		return nil, fmt.Errorf("decode private key: %w", err)
	}
	if len(decoded) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("private key is %d bytes, want %d", len(decoded), ed25519.PrivateKeySize)
	}
	return ed25519.PrivateKey(decoded), nil
}

// ParsePublicKey decodes the base64 text written by [EncodePublicKey].
func ParsePublicKey(text string) (ed25519.PublicKey, error) {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(text))
	if err != nil {
		return nil, fmt.Errorf("decode public key: %w", err)
	}
	if len(decoded) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key is %d bytes, want %d", len(decoded), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(decoded), nil
}
