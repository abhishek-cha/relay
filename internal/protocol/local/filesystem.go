package local

import (
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"relay/pkg/relay"
)

// entryType names a filesystem entry the way a result reports it.
func entryType(entry fs.FileInfo) string {
	switch {
	case entry.IsDir():
		return "directory"
	case entry.Mode()&fs.ModeSymlink != 0:
		return "symlink"
	case entry.Mode().IsRegular():
		return "file"
	default:
		return "other"
	}
}

// decodeContent turns an operation's `content` into bytes, honoring the
// `encoding` property. The default is text; base64 is opt-in.
func decodeContent(input map[string]any) ([]byte, *relay.Error) {
	content, ok := input["content"].(string)
	if !ok {
		return nil, relay.NewError(relay.CodeInvalidInput, "input \"content\" must be a string")
	}
	switch encoding := stringInput(input, "encoding", "utf-8"); encoding {
	case "", "utf-8":
		return []byte(content), nil
	case "base64":
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(content))
		if err != nil {
			return nil, relay.NewError(relay.CodeInvalidInput,
				"input \"content\" is not valid base64: "+err.Error())
		}
		return decoded, nil
	default:
		return nil, relay.NewError(relay.CodeInvalidInput,
			fmt.Sprintf("input \"encoding\" must be one of: utf-8, base64 (got %q)", encoding))
	}
}

// readFile returns a file's bytes as text or base64.
func readFile(path string, input map[string]any) (any, *relay.Error) {
	encoding := stringInput(input, "encoding", "utf-8")
	if encoding == "" {
		encoding = "utf-8"
	}
	if encoding != "utf-8" && encoding != "base64" {
		return nil, relay.NewError(relay.CodeInvalidInput,
			fmt.Sprintf("input \"encoding\" must be one of: utf-8, base64 (got %q)", encoding))
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, ioFailure("read file", path, err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, ioFailure("read file", path, err)
	}
	if info.IsDir() {
		return nil, relay.NewError(relay.CodeInvalidInput,
			fmt.Sprintf("read file %q: is a directory", path)).
			WithDetails(map[string]any{"path": path})
	}

	data, failure := readCapped(file, path)
	if failure != nil {
		return nil, failure
	}
	result := map[string]any{
		"path":     path,
		"encoding": encoding,
		"size":     len(data),
	}
	if encoding == "base64" {
		result["content"] = base64.StdEncoding.EncodeToString(data)
	} else {
		result["content"] = string(data)
	}
	return result, nil
}

// readCapped reads at most maxOutputBytes and refuses anything larger.
func readCapped(reader io.Reader, path string) ([]byte, *relay.Error) {
	data, err := io.ReadAll(io.LimitReader(reader, maxOutputBytes+1))
	if err != nil {
		return nil, ioFailure("read file", path, err)
	}
	if len(data) > maxOutputBytes {
		return nil, tooLarge("file", path, maxOutputBytes)
	}
	return data, nil
}

// writeFile creates or replaces a file.
func writeFile(path string, input map[string]any) (any, *relay.Error) {
	data, failure := decodeContent(input)
	if failure != nil {
		return nil, failure
	}
	if len(data) > maxOutputBytes {
		return nil, tooLarge("content", path, maxOutputBytes)
	}

	if boolInput(input, "create_dirs", false) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, ioFailure("create parent directory for", path, err)
		}
	}

	// A directory target is a request mistake, not an overwrite.
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return nil, relay.NewError(relay.CodeInvalidInput,
			fmt.Sprintf("write file %q: is a directory", path)).
			WithDetails(map[string]any{"path": path})
	}
	_, statErr := os.Stat(path)
	created := statErr != nil

	// 0600 for new files. Replacing an existing file keeps its mode, because
	// WriteFile truncates rather than recreating.
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, ioFailure("write file", path, err)
	}
	return map[string]any{
		"path":         path,
		"bytesWritten": len(data),
		"created":      created,
	}, nil
}

// listDirectory returns a directory's entries, optionally recursive and
// filtered by a glob. Symbolic links are reported but never followed, so a link
// cannot pull the listing outside the checked scope.
func listDirectory(path string, input map[string]any) (any, *relay.Error) {
	recursive := boolInput(input, "recursive", false)
	pattern := stringInput(input, "pattern", "")
	if pattern != "" {
		if _, err := filepath.Match(pattern, ""); err != nil {
			return nil, relay.NewError(relay.CodeInvalidInput,
				fmt.Sprintf("input \"pattern\" is not a valid glob: %v", err))
		}
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, ioFailure("list directory", path, err)
	}
	if !info.IsDir() {
		return nil, relay.NewError(relay.CodeInvalidInput,
			fmt.Sprintf("list directory %q: not a directory", path)).
			WithDetails(map[string]any{"path": path})
	}

	entries := make([]map[string]any, 0)
	budget := maxOutputBytes
	add := func(entryPath string, entry fs.DirEntry) *relay.Error {
		entryInfo, err := entry.Info()
		if err != nil {
			return ioFailure("list directory", entryPath, err)
		}
		if pattern != "" {
			matched, err := filepath.Match(pattern, entry.Name())
			if err != nil {
				return relay.NewError(relay.CodeInvalidInput,
					fmt.Sprintf("input \"pattern\" is not a valid glob: %v", err))
			}
			if !matched {
				return nil
			}
		}
		// Charge the entry against the 8 MiB result budget before adding it, so
		// a huge tree is refused rather than silently truncated.
		cost := len(entryPath) + len(entry.Name()) + 64
		if cost > budget {
			return tooLarge("directory listing", path, maxOutputBytes)
		}
		budget -= cost
		entries = append(entries, map[string]any{
			"name": entry.Name(),
			"path": entryPath,
			"type": entryType(entryInfo),
			"size": entryInfo.Size(),
		})
		return nil
	}

	if recursive {
		walkErr := filepath.WalkDir(path, func(entryPath string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entryPath == path {
				return nil
			}
			if failure := add(entryPath, entry); failure != nil {
				return failure
			}
			return nil
		})
		if walkErr != nil {
			if structured, ok := walkErr.(*relay.Error); ok {
				return nil, structured
			}
			return nil, ioFailure("list directory", path, walkErr)
		}
	} else {
		children, err := os.ReadDir(path)
		if err != nil {
			return nil, ioFailure("list directory", path, err)
		}
		for _, child := range children {
			if failure := add(filepath.Join(path, child.Name()), child); failure != nil {
				return nil, failure
			}
		}
	}

	return map[string]any{
		"path":      path,
		"recursive": recursive,
		"pattern":   pattern,
		"count":     len(entries),
		"entries":   entries,
	}, nil
}

// statPath reports one path's metadata.
func statPath(path string) (any, *relay.Error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, ioFailure("stat", path, err)
	}
	return map[string]any{
		"path":       path,
		"name":       info.Name(),
		"type":       entryType(info),
		"size":       info.Size(),
		"mode":       info.Mode().Perm().String(),
		"modifiedAt": info.ModTime().UTC().Format(time.RFC3339Nano),
	}, nil
}
