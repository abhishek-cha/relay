package sign

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testManifest   = "apiVersion: relay/v1\nkind: Tool\nmetadata:\n  name: demo\n  version: 1.0.0\n"
	testDescriptor = `{"apiVersion":"relay/v1","kind":"Tool","name":"demo","version":"1.0.0"}`
)

func testKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return public, private
}

func testSignature(t *testing.T, private ed25519.PrivateKey, publisher, version, manifest, descriptor, digest string) Signature {
	t.Helper()
	signature, err := Sign(private, publisher, "demo", version, []byte(manifest), []byte(descriptor), digest, time.Unix(0, 0))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return signature
}

func TestSignVerifyRoundTrip(t *testing.T) {
	_, private := testKey(t)
	digest := Digest([]byte("binary"))
	signature := testSignature(t, private, "GitHub", "1.0.0", testManifest, testDescriptor, digest)

	if err := signature.Verify("demo", "1.0.0", []byte(testManifest), []byte(testDescriptor), digest); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// A signature is only worth anything if it binds the manifest, the descriptor,
// and the binary. Each of those changing must break verification on its own.
func TestVerifyRejectsTampering(t *testing.T) {
	_, private := testKey(t)
	digest := Digest([]byte("binary"))
	signature := testSignature(t, private, "GitHub", "1.0.0", testManifest, testDescriptor, digest)

	cases := []struct {
		name       string
		version    string
		manifest   string
		descriptor string
		digest     string
	}{
		{"tampered manifest", "1.0.0", testManifest + "# injected\n", testDescriptor, digest},
		{"tampered descriptor", "1.0.0", testManifest, strings.Replace(testDescriptor, "demo", "evil", 1), digest},
		{"tampered binary", "1.0.0", testManifest, testDescriptor, Digest([]byte("binary+malware"))},
		{"tampered version", "2.0.0", testManifest, testDescriptor, digest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := signature.Verify("demo", tc.version, []byte(tc.manifest), []byte(tc.descriptor), tc.digest); err == nil {
				t.Fatalf("Verify accepted a %s", tc.name)
			}
		})
	}
}

// The policy layer matches on the fingerprint, so an envelope whose fingerprint
// does not describe its own key must be refused rather than trusted.
func TestVerifyRejectsFingerprintMismatch(t *testing.T) {
	_, private := testKey(t)
	_, other := testKey(t)
	digest := Digest([]byte("binary"))
	signature := testSignature(t, private, "GitHub", "1.0.0", testManifest, testDescriptor, digest)
	signature.PublicKey = EncodePublicKey(other.Public().(ed25519.PublicKey))

	err := signature.Verify("demo", "1.0.0", []byte(testManifest), []byte(testDescriptor), digest)
	if err == nil || !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("Verify = %v, want a fingerprint mismatch", err)
	}
}

func TestVerifyRejectsUnsupportedEnvelope(t *testing.T) {
	_, private := testKey(t)
	digest := Digest([]byte("binary"))
	signature := testSignature(t, private, "GitHub", "1.0.0", testManifest, testDescriptor, digest)

	wrongSchema := signature
	wrongSchema.Schema = "relay-signature/v2"
	if err := wrongSchema.Verify("demo", "1.0.0", []byte(testManifest), []byte(testDescriptor), digest); err == nil {
		t.Fatal("Verify accepted an unsupported schema")
	}

	wrongAlgorithm := signature
	wrongAlgorithm.Algorithm = "rsa"
	if err := wrongAlgorithm.Verify("demo", "1.0.0", []byte(testManifest), []byte(testDescriptor), digest); err == nil {
		t.Fatal("Verify accepted an unsupported algorithm")
	}
}

func TestSignatureEnvelopeRoundTripsThroughDisk(t *testing.T) {
	_, private := testKey(t)
	digest := Digest([]byte("binary"))
	signature := testSignature(t, private, "GitHub", "1.0.0", testManifest, testDescriptor, digest)

	path := filepath.Join(t.TempDir(), "demo.sig")
	if err := Save(path, signature); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := loaded.Verify("demo", "1.0.0", []byte(testManifest), []byte(testDescriptor), digest); err != nil {
		t.Fatalf("loaded signature did not verify: %v", err)
	}
}

func TestSignaturePathIsASuffix(t *testing.T) {
	if got := SignaturePath("/tmp/demo"); got != "/tmp/demo.sig" {
		t.Fatalf("SignaturePath = %q", got)
	}
}

func TestCanonicalBytesBindEveryField(t *testing.T) {
	base := Message{Name: "demo", Version: "1.0.0", Publisher: "GitHub", ManifestSHA256: "m", DescriptorSHA256: "d", BinarySHA256: "b"}
	baseline, err := CanonicalBytes(base)
	if err != nil {
		t.Fatalf("CanonicalBytes: %v", err)
	}
	if !strings.HasPrefix(string(baseline), Schema+"\n") {
		t.Fatalf("canonical bytes are not domain-separated: %q", baseline)
	}

	mutations := map[string]Message{
		"name":       {Name: "evil", Version: base.Version, Publisher: base.Publisher, ManifestSHA256: "m", DescriptorSHA256: "d", BinarySHA256: "b"},
		"version":    {Name: base.Name, Version: "2.0.0", Publisher: base.Publisher, ManifestSHA256: "m", DescriptorSHA256: "d", BinarySHA256: "b"},
		"publisher":  {Name: base.Name, Version: base.Version, Publisher: "BadCorp", ManifestSHA256: "m", DescriptorSHA256: "d", BinarySHA256: "b"},
		"manifest":   {Name: base.Name, Version: base.Version, Publisher: base.Publisher, ManifestSHA256: "x", DescriptorSHA256: "d", BinarySHA256: "b"},
		"descriptor": {Name: base.Name, Version: base.Version, Publisher: base.Publisher, ManifestSHA256: "m", DescriptorSHA256: "x", BinarySHA256: "b"},
		"binary":     {Name: base.Name, Version: base.Version, Publisher: base.Publisher, ManifestSHA256: "m", DescriptorSHA256: "d", BinarySHA256: "x"},
	}
	for field, mutated := range mutations {
		encoded, err := CanonicalBytes(mutated)
		if err != nil {
			t.Fatalf("CanonicalBytes(%s): %v", field, err)
		}
		if string(encoded) == string(baseline) {
			t.Errorf("changing %s did not change the signed bytes", field)
		}
	}
}

// A newline in an identity field would let two different identities collide
// onto one signed message, so it is refused outright.
func TestCanonicalBytesRejectsNewlines(t *testing.T) {
	_, err := CanonicalBytes(Message{Name: "demo\nversion: 2.0.0", Version: "1.0.0"})
	if err == nil {
		t.Fatal("CanonicalBytes accepted a newline in a field")
	}
}

func TestDigestFileMatchesDigest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blob")
	data := []byte("relay tool binary")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	digest, err := DigestFile(path)
	if err != nil {
		t.Fatalf("DigestFile: %v", err)
	}
	if digest != Digest(data) {
		t.Fatalf("DigestFile = %s, want %s", digest, Digest(data))
	}
}

func TestKeyTextRoundTrips(t *testing.T) {
	public, private := testKey(t)
	parsedPrivate, err := ParsePrivateKey(EncodePrivateKey(private))
	if err != nil {
		t.Fatalf("ParsePrivateKey: %v", err)
	}
	if !parsedPrivate.Equal(private) {
		t.Fatal("private key did not round-trip")
	}
	parsedPublic, err := ParsePublicKey(EncodePublicKey(public))
	if err != nil {
		t.Fatalf("ParsePublicKey: %v", err)
	}
	if !parsedPublic.Equal(public) {
		t.Fatal("public key did not round-trip")
	}
	if _, err := ParsePublicKey("not base64!!"); err == nil {
		t.Fatal("ParsePublicKey accepted garbage")
	}
}
