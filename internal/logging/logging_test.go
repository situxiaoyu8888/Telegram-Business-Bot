package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileOutput(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	logger, file, err := New(Options{Dir: dir, MaxSizeMB: 1, MaxBackups: 1, MaxAgeDays: 1})
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("hello file")
	_ = logger.Sync()
	_ = file.Close()
	b, err := os.ReadFile(filepath.Join(dir, "bot.log"))
	if err != nil || !strings.Contains(string(b), `"msg":"hello file"`) {
		t.Fatalf("log file = %q, %v", b, err)
	}
}

func TestFileDisabled(t *testing.T) {
	_, file, err := New(Options{})
	if err != nil || file != nil {
		t.Fatal(file, err)
	}
}
