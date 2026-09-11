package daemon

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"relay/internal/paths"
	"relay/internal/protocol"
	"relay/internal/registry"
	"relay/internal/sign"
	"relay/pkg/relay"
)

// This file is the M11 install half of the signed-tools work (spec §48, §49).
// It proves the daemon enforces a signature and the local trust policy before a
// tool is copied anywhere, and that an unsigned tool still installs as before.
//
// Every test builds a throwaway RELAY_HOME and signs with an in-test ed25519
// key, so nothing here touches a real Keychain, the user's ~/.relay, or the
// network.

const signInstallManifest = `apiVersion: relay/v1
kind: Tool
metadata:
  name: demo
  version: 1.0.0
runtime:
  name: relay
  apiVersion: v1
protocol:
  type: rest
  baseUrl: https://example.test
tools:
  - name: ping
    description: ping the example service
    input:
      type: object
    request:
      method: GET
      path: /ping
`

const signInstallDescriptor = `{"apiVersion":"relay/v1","kind":"Tool","name":"demo","version":"1.0.0","protocol":"rest","runtime":{"name":"relay","apiVersion":"v1"},"tools":[{"name":"ping","description":"ping the example service"}]}`

// signInstallTool is a fake tool binary plus the two files it serves, so a test
// can tamper with one artifact at a time.
type signInstallTool struct {
	Binary     string
	Manifest   string
	Descriptor string
}

// writeSignInstallTool writes an executable that serves --manifest and
// --describe by catting the files, so the bytes the daemon verifies are exactly
// the bytes the test controls.
func writeSignInstallTool(t *testing.T, manifest, descriptor string) signInstallTool {
	t.Helper()
	dir := t.TempDir()
	manifestPath := filepath.Join(dir, "manifest.yaml")
	descriptorPath := filepath.Join(dir, "descriptor.json")
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(descriptorPath, []byte(descriptor), 0o644); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "demo")
	body := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  --manifest) cat " + manifestPath + " ;;\n" +
		"  --describe) cat " + descriptorPath + " ;;\n" +
		"esac\n"
	if err := os.WriteFile(binary, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return signInstallTool{Binary: binary, Manifest: manifestPath, Descriptor: descriptorPath}
}

// signInstall signs a fake tool exactly as `relay sign` would: it reads the
// --manifest and --describe bytes from the binary itself and writes the sidecar.
func signInstall(t *testing.T, tool signInstallTool, publisher string) {
	t.Helper()
	_, private, err := sign.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	manifest := execToolOutput(t, tool.Binary, "--manifest")
	descriptor := execToolOutput(t, tool.Binary, "--describe")
	var parsed relay.Descriptor
	if err := json.Unmarshal(descriptor, &parsed); err != nil {
		t.Fatalf("parse descriptor: %v", err)
	}
	digest, err := sign.DigestFile(tool.Binary)
	if err != nil {
		t.Fatalf("DigestFile: %v", err)
	}
	signature, err := sign.Sign(private, publisher, parsed.Name, parsed.Version, manifest, descriptor, digest, time.Unix(0, 0))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := sign.Save(sign.SignaturePath(tool.Binary), signature); err != nil {
		t.Fatalf("Save signature: %v", err)
	}
}

func execToolOutput(t *testing.T, binary, arg string) []byte {
	t.Helper()
	output, err := exec.Command(binary, arg).Output()
	if err != nil {
		t.Fatalf("%s %s: %v", binary, arg, err)
	}
	return output
}

// newSignInstallDaemon builds a daemon over a throwaway home with an injected
// Keychain and executor.
func newSignInstallDaemon(t *testing.T) *Daemon {
	t.Helper()
	t.Setenv(paths.EnvHome, t.TempDir())
	layout := paths.Default()
	if err := layout.Ensure(); err != nil {
		t.Fatalf("ensure layout: %v", err)
	}
	return New(Config{
		Layout:    layout,
		Version:   "test",
		Executors: map[string]protocol.Executor{"rest": &fakeExecutor{}},
		Keychain:  newMemKeychain(),
		Log:       io.Discard,
	})
}

// writeTrustPolicy drops a policy file under the daemon's Relay home.
func writeTrustPolicy(t *testing.T, d *Daemon, body string) {
	t.Helper()
	if err := os.MkdirAll(d.layout.Config, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.layout.Config, sign.PolicyFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func installedTrust(t *testing.T, d *Daemon, name string) sign.TrustLevel {
	t.Helper()
	level, err := registry.New(d.layout.Registry).Trust(name)
	if err != nil {
		t.Fatalf("read trust for %s: %v", name, err)
	}
	return level
}

func TestInstallSignedToolIsVerified(t *testing.T) {
	d := newSignInstallDaemon(t)
	tool := writeSignInstallTool(t, signInstallManifest, signInstallDescriptor)
	signInstall(t, tool, "GitHub")

	response := d.register(context.Background(), relay.RegisterRequest{Type: relay.FrameRegister, Path: tool.Binary})
	if !response.Success {
		t.Fatalf("register refused a validly signed tool: %+v", response.Error)
	}
	if got := installedTrust(t, d, "demo"); got != sign.TrustVerified {
		t.Fatalf("trust = %q, want %q", got, sign.TrustVerified)
	}
}

func TestInstallRefusesTamperedSignedTool(t *testing.T) {
	cases := []struct {
		name   string
		tamper func(t *testing.T, tool signInstallTool)
	}{
		{"manifest", func(t *testing.T, tool signInstallTool) {
			appendFile(t, tool.Manifest, "# injected after signing\n")
		}},
		{"descriptor", func(t *testing.T, tool signInstallTool) {
			// Still a valid, runtime-compatible descriptor, but not the bytes that
			// were signed.
			const swapped = `{"apiVersion":"relay/v1","kind":"Tool","name":"demo","version":"9.9.9","protocol":"rest","runtime":{"name":"relay","apiVersion":"v1"},"tools":[{"name":"ping","description":"ping the example service"}]}`
			if err := os.WriteFile(tool.Descriptor, []byte(swapped), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"binary", func(t *testing.T, tool signInstallTool) {
			appendFile(t, tool.Binary, "# tampered\n")
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newSignInstallDaemon(t)
			tool := writeSignInstallTool(t, signInstallManifest, signInstallDescriptor)
			signInstall(t, tool, "GitHub")
			tc.tamper(t, tool)

			response := d.register(context.Background(), relay.RegisterRequest{Type: relay.FrameRegister, Path: tool.Binary})
			if response.Success || response.Error == nil {
				t.Fatalf("a tampered %s was installed: %+v", tc.name, response)
			}
			if response.Error.Code != sign.CodeSignatureInvalid {
				t.Fatalf("code = %s, want %s", response.Error.Code, sign.CodeSignatureInvalid)
			}
			if _, err := registry.New(d.layout.Registry).Get("demo"); err == nil {
				t.Fatal("a refused signature left a registry record behind")
			}
			if _, err := os.Stat(d.layout.Binary("demo")); !os.IsNotExist(err) {
				t.Fatal("a refused signature left the binary behind")
			}
		})
	}
}

func TestInstallRefusesBlockedTool(t *testing.T) {
	d := newSignInstallDaemon(t)
	writeTrustPolicy(t, d, "blocked:\n  - name: demo\n")
	tool := writeSignInstallTool(t, signInstallManifest, signInstallDescriptor)

	response := d.register(context.Background(), relay.RegisterRequest{Type: relay.FrameRegister, Path: tool.Binary})
	if response.Success || response.Error == nil {
		t.Fatalf("a blocked tool was installed: %+v", response)
	}
	if response.Error.Code != sign.CodeToolBlocked {
		t.Fatalf("code = %s, want %s", response.Error.Code, sign.CodeToolBlocked)
	}
	if _, err := registry.New(d.layout.Registry).Get("demo"); err == nil {
		t.Fatal("a blocked tool left a registry record behind")
	}
}

// Local policy outranks a valid signature: a blocked tool is refused even when
// its signature verifies.
func TestInstallRefusesBlockedToolDespiteValidSignature(t *testing.T) {
	d := newSignInstallDaemon(t)
	writeTrustPolicy(t, d, "blocked:\n  - name: demo\n")
	tool := writeSignInstallTool(t, signInstallManifest, signInstallDescriptor)
	signInstall(t, tool, "GitHub")

	response := d.register(context.Background(), relay.RegisterRequest{Type: relay.FrameRegister, Path: tool.Binary})
	if response.Success || response.Error == nil {
		t.Fatalf("a blocked signed tool was installed: %+v", response)
	}
	if response.Error.Code != sign.CodeToolBlocked {
		t.Fatalf("code = %s, want %s", response.Error.Code, sign.CodeToolBlocked)
	}
}

func TestInstallUnsignedToolStaysUnknown(t *testing.T) {
	d := newSignInstallDaemon(t)
	tool := writeSignInstallTool(t, signInstallManifest, signInstallDescriptor)

	response := d.register(context.Background(), relay.RegisterRequest{Type: relay.FrameRegister, Path: tool.Binary})
	if !response.Success {
		t.Fatalf("an unsigned tool was refused: %+v", response.Error)
	}
	if got := installedTrust(t, d, "demo"); got != sign.TrustUnknown {
		t.Fatalf("trust = %q, want %q", got, sign.TrustUnknown)
	}
}

func TestInstallAllowlistedToolIsTrusted(t *testing.T) {
	d := newSignInstallDaemon(t)
	writeTrustPolicy(t, d, "trusted:\n  - name: demo\n")
	tool := writeSignInstallTool(t, signInstallManifest, signInstallDescriptor)

	response := d.register(context.Background(), relay.RegisterRequest{Type: relay.FrameRegister, Path: tool.Binary})
	if !response.Success {
		t.Fatalf("an allowlisted unsigned tool was refused: %+v", response.Error)
	}
	if got := installedTrust(t, d, "demo"); got != sign.TrustTrusted {
		t.Fatalf("trust = %q, want %q", got, sign.TrustTrusted)
	}
}

// A policy the operator wrote but that cannot be parsed must stop the install,
// not be ignored: ignoring it could install something the operator tried to
// keep out.
func TestInstallRefusesMalformedTrustPolicy(t *testing.T) {
	d := newSignInstallDaemon(t)
	writeTrustPolicy(t, d, "trusted: {not a list}\n")
	tool := writeSignInstallTool(t, signInstallManifest, signInstallDescriptor)

	response := d.register(context.Background(), relay.RegisterRequest{Type: relay.FrameRegister, Path: tool.Binary})
	if response.Success || response.Error == nil {
		t.Fatalf("install proceeded under a malformed policy: %+v", response)
	}
}

func appendFile(t *testing.T, path, extra string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(extra); err != nil {
		t.Fatal(err)
	}
}
