package starling_test

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	starling "github.com/jerkeyray/starling"
)

func TestMigrateCommand_FreshIsNoop(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "log.db")

	var buf bytes.Buffer
	c := starling.MigrateCommand()
	c.Output = &buf
	if err := c.Run([]string{dbPath}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(buf.String(), "already at v") {
		t.Fatalf("output = %q, want \"already at v\"", buf.String())
	}
}

func TestSchemaVersionCommand(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "log.db")

	// Initialize via MigrateCommand so the meta table exists.
	if err := starling.MigrateCommand().Run([]string{dbPath}); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	var buf bytes.Buffer
	c := starling.SchemaVersionCommand()
	c.Output = &buf
	if err := c.Run([]string{dbPath}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.TrimSpace(buf.String()); got != "2" {
		t.Fatalf("output = %q, want \"2\"", got)
	}
}
