package builtin

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jerkeyray/starling/tool"
)

// readFileMaxBytes caps a single ReadFile call at 1 MiB.
const readFileMaxBytes = 1 << 20

// ReadFileInput is the JSON-schema-describing input type for ReadFile.
type ReadFileInput struct {
	Path string `json:"path" jsonschema:"description=Path relative to the tool's base directory."`
}

// ReadFileOutput is the JSON-schema-describing output type for ReadFile.
type ReadFileOutput struct {
	Contents string `json:"contents"`
}

// ReadFile returns a tool that reads a file under baseDir (and only under
// baseDir — attempts to escape via "..", absolute paths, or symlinks are
// rejected). Contents are capped at 1 MiB.
//
// Returns an error if baseDir cannot be resolved to an absolute path;
// an invalid baseDir is a caller (user) bug, not a programmer bug, so
// the error is surfaced rather than panicked.
//
// baseDir is required: defaulting to "." would silently expose the
// process's cwd.
func ReadFile(baseDir string) (tool.Tool, error) {
	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, fmt.Errorf("builtin.ReadFile: resolve base dir %q: %w", baseDir, err)
	}
	// Also resolve symlinks on the base so that containment checks on a
	// resolved target compare like-to-like (macOS /tmp -> /private/tmp is
	// the classic gotcha).
	if resolved, err := filepath.EvalSymlinks(absBase); err == nil {
		absBase = resolved
	}

	return tool.Typed[ReadFileInput, ReadFileOutput](
		"read_file",
		"Read a file under the tool's base directory. Returns up to 1 MiB of contents.",
		func(ctx context.Context, in ReadFileInput) (ReadFileOutput, error) {
			if in.Path == "" {
				return ReadFileOutput{}, fmt.Errorf("read_file: path is required")
			}
			if filepath.IsAbs(in.Path) {
				return ReadFileOutput{}, fmt.Errorf("read_file: absolute paths are not allowed")
			}

			joined := filepath.Join(absBase, filepath.Clean(in.Path))
			if !withinBase(absBase, joined) {
				return ReadFileOutput{}, fmt.Errorf("read_file: path escapes base directory")
			}

			// After resolving symlinks, re-verify containment.
			resolved, err := filepath.EvalSymlinks(joined)
			if err != nil {
				return ReadFileOutput{}, fmt.Errorf("read_file: resolve path: %w", err)
			}
			if !withinBase(absBase, resolved) {
				return ReadFileOutput{}, fmt.Errorf("read_file: symlink escapes base directory")
			}

			f, err := os.Open(resolved)
			if err != nil {
				return ReadFileOutput{}, fmt.Errorf("read_file: open: %w", err)
			}
			defer f.Close()

			// Read one extra byte so we can distinguish "exactly at cap" from
			// "truncated". Silent truncation would feed the model a clipped
			// file with no signal.
			b, err := io.ReadAll(io.LimitReader(f, readFileMaxBytes+1))
			if err != nil {
				return ReadFileOutput{}, fmt.Errorf("read_file: read: %w", err)
			}
			if len(b) > readFileMaxBytes {
				return ReadFileOutput{}, fmt.Errorf("read_file: file exceeds %d-byte limit", readFileMaxBytes)
			}
			return ReadFileOutput{Contents: string(b)}, nil
		},
	), nil
}

// withinBase reports whether target is base itself or lives inside it.
// Both inputs should already be absolute and clean.
func withinBase(base, target string) bool {
	if target == base {
		return true
	}
	sep := string(filepath.Separator)
	baseWithSep := base
	if !strings.HasSuffix(baseWithSep, sep) {
		baseWithSep += sep
	}
	return strings.HasPrefix(target, baseWithSep)
}
