// Package build implements the manifest-to-binary pipeline (spec §12):
// parse, validate the manifest, validate the skill, package both alongside the
// generic runtime, and produce one self-contained executable.
//
// The generic runtime interprets the manifest and delegates execution to the
// daemon, so the generated binary contains no endpoint-specific code (spec §13).
package build

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"relay/internal/manifest"
	"relay/internal/paths"
	"relay/internal/skill"
)

//go:embed toolmain.go.tmpl
var toolMainTemplate string

// Options configures one build.
type Options struct {
	// ManifestPath is the tool's YAML manifest. Required.
	ManifestPath string
	// SkillPath is the tool's SKILL.md. Optional, but a tool without guidance
	// is only half a Relay tool (spec §8).
	SkillPath string
	// OutPath is the output binary. Defaults to dist/<tool>.
	OutPath string
	// SourceRoot is the Relay source tree that provides the runtime. When empty
	// it is discovered from RELAY_SOURCE or the current directory.
	SourceRoot string
	// KeepWork leaves the generated build directory in place for inspection.
	KeepWork bool
	// Verbose reports progress on stderr.
	Verbose bool
}

// Result reports what a build produced.
type Result struct {
	Name    string
	Version string
	OutPath string
	WorkDir string
}

// Build runs the full pipeline and returns the produced binary.
func Build(ctx context.Context, opts Options) (*Result, error) {
	doc, err := manifest.Load(opts.ManifestPath)
	if err != nil {
		return nil, err
	}
	if err := doc.Validate(); err != nil {
		return nil, err
	}

	skillBytes, err := loadSkill(opts.SkillPath, opts.Verbose)
	if err != nil {
		return nil, err
	}
	// The skill is guidance, never a second schema, so a skill that restates the
	// manifest's inputs is a build error rather than a warning: shipping it would
	// give an agent two places to read a schema from and let them drift (§7, §30).
	if err := lintSkill(skillBytes, doc, opts.Verbose); err != nil {
		return nil, err
	}
	// The skill and the manifest are versioned together (spec §36): a skill that
	// declares a version must agree with the manifest it ships with, or guidance
	// written for one revision would silently ride along with another.
	if err := checkSkillVersion(opts.SkillPath, skillBytes, doc); err != nil {
		return nil, err
	}

	sourceRoot, err := findSourceRoot(opts.SourceRoot)
	if err != nil {
		return nil, err
	}

	workDir := filepath.Join(sourceRoot, "build", doc.Metadata.Name)
	if err := os.RemoveAll(workDir); err != nil {
		return nil, fmt.Errorf("clear build directory: %w", err)
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, fmt.Errorf("create build directory: %w", err)
	}

	manifestBytes, err := os.ReadFile(opts.ManifestPath)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	if err := writeIfChanged(filepath.Join(workDir, "manifest.yaml"), manifestBytes); err != nil {
		return nil, err
	}
	if err := writeIfChanged(filepath.Join(workDir, "SKILL.md"), skillBytes); err != nil {
		return nil, err
	}
	if err := writeIfChanged(filepath.Join(workDir, "go.mod"), []byte(goMod(doc.Metadata.Name, sourceRoot))); err != nil {
		return nil, err
	}

	mainSource, err := renderToolMain()
	if err != nil {
		return nil, err
	}
	if err := writeIfChanged(filepath.Join(workDir, "main.go"), mainSource); err != nil {
		return nil, err
	}

	if err := goModTidy(ctx, workDir); err != nil {
		return nil, err
	}

	outPath := opts.OutPath
	if outPath == "" {
		outPath = filepath.Join("dist", doc.Metadata.Name)
	}
	outPath, err = filepath.Abs(outPath)
	if err != nil {
		return nil, fmt.Errorf("resolve output path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return nil, fmt.Errorf("create output directory: %w", err)
	}

	if err := goBuild(ctx, workDir, outPath); err != nil {
		return nil, err
	}

	// When the caller did not specify an explicit output path, also create a
	// PATH shim in layout.Bin so the tool is reachable via the user's PATH
	// (spec §38).  The shim is best-effort: a failure is reported on stderr
	// when Verbose but must never fail the build.
	if opts.OutPath == "" {
		layout := paths.Default()
		shim := layout.Shims(doc.Metadata.Name)
		if err := os.MkdirAll(layout.Bin, 0o755); err != nil {
			if opts.Verbose {
				fmt.Fprintf(os.Stderr, "relay: warning: create bin directory: %v\n", err)
			}
		} else {
			if err := os.Symlink(outPath, shim); err != nil {
				if opts.Verbose {
					fmt.Fprintf(os.Stderr, "relay: warning: create shim %s: %v\n", shim, err)
				}
			}
		}
	}

	result := &Result{
		Name:    doc.Metadata.Name,
		Version: doc.Metadata.Version,
		OutPath: outPath,
		WorkDir: workDir,
	}
	if !opts.KeepWork {
		// The built binary is self-contained, so the work directory is
		// disposable once the compile succeeds.
		_ = os.RemoveAll(filepath.Dir(workDir))
	}
	return result, nil
}

// lintSkill enforces the manifest/skill split (spec §7, §30).
//
// An Error-severity issue fails the build; a Warning is printed but tolerated,
// because a weak skill is a quality problem while a schema-restating one is a
// correctness problem: two sources of truth that will drift apart.
func lintSkill(skillBytes []byte, doc *manifest.Document, verbose bool) error {
	// No skill was supplied: loadSkill already warned, and there is nothing to
	// lint. An explicitly empty file never reaches here, because skill.Validate
	// rejects it first.
	if len(skillBytes) == 0 {
		return nil
	}
	issues := skill.Lint(string(skillBytes), doc)
	var failures []string
	for _, issue := range issues {
		if issue.Severity == skill.Error {
			failures = append(failures, fmt.Sprintf("%d: %s", issue.Line, issue.Message))
			continue
		}
		if verbose {
			fmt.Fprintf(os.Stderr, "relay: skill warning (line %d): %s\n", issue.Line, issue.Message)
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("skill does not meet §30: %s", strings.Join(failures, "; "))
	}
	return nil
}

// checkSkillVersion enforces spec §36: when a skill declares a version it must
// equal the manifest's metadata.version, and a mismatch is a build error, not a
// warning. A skill without a version marker (the empty case, or the common
// backward-compatible case) is accepted unchanged, so existing skills keep
// building.
func checkSkillVersion(skillPath string, skillBytes []byte, doc *manifest.Document) error {
	if len(skillBytes) == 0 {
		return nil
	}
	declared, ok, err := skill.Version(skillBytes)
	if err != nil {
		return fmt.Errorf("skill %s: %w", skillName(skillPath), err)
	}
	if !ok {
		return nil
	}
	if declared != doc.Metadata.Version {
		return fmt.Errorf("skill %s declares version %q but the manifest metadata.version is %q; version the skill and manifest together (spec §36)", skillName(skillPath), declared, doc.Metadata.Version)
	}
	return nil
}

// skillName is the label used in errors when a skill path is unavailable.
func skillName(path string) string {
	if path == "" {
		return "SKILL.md"
	}
	return filepath.Base(path)
}

func loadSkill(path string, verbose bool) ([]byte, error) {
	if path == "" {
		if verbose {
			fmt.Fprintln(os.Stderr, "relay: warning: no skill given; the tool will carry no AI guidance")
		}
		// The file must exist for go:embed, so embed an empty placeholder.
		return []byte{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read skill %s: %w", path, err)
	}
	if err := skill.Validate(data); err != nil {
		return nil, fmt.Errorf("skill %s: %w", path, err)
	}
	return data, nil
}

func renderToolMain() ([]byte, error) {
	tmpl, err := template.New("toolmain").Parse(toolMainTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse tool main template: %w", err)
	}
	var out strings.Builder
	if err := tmpl.Execute(&out, nil); err != nil {
		return nil, fmt.Errorf("render tool main: %w", err)
	}
	return []byte(out.String()), nil
}

// goMod generates the build module. It is a nested module, so the Relay source
// tree's own build command never tries to compile generated tools.
func goMod(name, sourceRoot string) string {
	return fmt.Sprintf(`module relay-tool/%s

go 1.22

require relay v0.0.0

replace relay => %s
`, name, sourceRoot)
}

func goModTidy(ctx context.Context, dir string) error {
	cmd := exec.CommandContext(ctx, "go", "mod", "tidy")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go mod tidy: %w\n%s", err, out)
	}
	return nil
}

func goBuild(ctx context.Context, dir, outPath string) error {
	args := []string{"build", "-trimpath", "-ldflags", "-s -w", "-o", outPath, "."}
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build: %w\n%s", err, out)
	}
	return nil
}

// findSourceRoot locates the Relay source tree that provides the embedded
// runtime. Building requires the source; the output stays portable (spec §34).
func findSourceRoot(explicit string) (string, error) {
	if explicit != "" {
		return verifySourceRoot(explicit)
	}
	if fromEnv := os.Getenv("RELAY_SOURCE"); fromEnv != "" {
		return verifySourceRoot(fromEnv)
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("determine working directory: %w", err)
	}
	for {
		candidate := filepath.Join(dir, "go.mod")
		if data, err := os.ReadFile(candidate); err == nil && isRelayModule(data) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("cannot find the Relay source tree; run from the Relay repository or set RELAY_SOURCE")
}

func verifySourceRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve source root: %w", err)
	}
	data, err := os.ReadFile(filepath.Join(abs, "go.mod"))
	if err != nil {
		return "", fmt.Errorf("source root %s: %w", abs, err)
	}
	if !isRelayModule(data) {
		return "", fmt.Errorf("source root %s is not the Relay module", abs)
	}
	return abs, nil
}

func isRelayModule(gomod []byte) bool {
	for _, line := range strings.Split(string(gomod), "\n") {
		if strings.TrimSpace(line) == "module relay" {
			return true
		}
	}
	return false
}

// writeIfChanged avoids needless rewrites that would bust the build cache.
func writeIfChanged(path string, data []byte) error {
	if existing, err := os.ReadFile(path); err == nil && string(existing) == string(data) {
		return nil
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	return nil
}
