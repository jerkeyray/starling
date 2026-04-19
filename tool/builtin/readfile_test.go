package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestReadFile_Success(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(path, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}

	tl, _ := ReadFile(dir)
	in, _ := json.Marshal(ReadFileInput{Path: "hello.txt"})
	out, err := tl.Execute(context.Background(), in)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got ReadFileOutput
	_ = json.Unmarshal(out, &got)
	if got.Contents != "hello world" {
		t.Fatalf("got = %q", got.Contents)
	}
}

func TestReadFile_EscapesBaseDir(t *testing.T) {
	dir := t.TempDir()
	tl, _ := ReadFile(dir)
	in, _ := json.Marshal(ReadFileInput{Path: "../../../etc/passwd"})
	_, err := tl.Execute(context.Background(), in)
	if err == nil {
		t.Fatalf("expected error for ..-escape")
	}
}

func TestReadFile_AbsolutePathRejected(t *testing.T) {
	dir := t.TempDir()
	tl, _ := ReadFile(dir)
	in, _ := json.Marshal(ReadFileInput{Path: "/etc/passwd"})
	_, err := tl.Execute(context.Background(), in)
	if err == nil {
		t.Fatalf("expected error for absolute path")
	}
}

func TestReadFile_SymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink test skipped on windows")
	}
	base := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}

	tl, _ := ReadFile(base)
	in, _ := json.Marshal(ReadFileInput{Path: "link"})
	_, err := tl.Execute(context.Background(), in)
	if err == nil {
		t.Fatalf("expected error for symlink escape")
	}
}

func TestReadFile_Nonexistent(t *testing.T) {
	dir := t.TempDir()
	tl, _ := ReadFile(dir)
	in, _ := json.Marshal(ReadFileInput{Path: "nope.txt"})
	_, err := tl.Execute(context.Background(), in)
	if err == nil {
		t.Fatalf("expected error")
	}
}

func TestReadFile_MissingPath(t *testing.T) {
	dir := t.TempDir()
	tl, _ := ReadFile(dir)
	_, err := tl.Execute(context.Background(), json.RawMessage(`{}`))
	if err == nil {
		t.Fatalf("expected error for missing path")
	}
}
