package stats

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func call(ok bool, tokens int) Call {
	return Call{Time: time.Now(), Upstream: "u", OK: ok, TotalTokens: tokens, DurationMs: 10}
}

// LogBuffer used to be a config field nothing read: NewStore hardcoded 200.
func TestBufCapIsHonoured(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "s.json"), 5)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		s.RecordCall(call(true, 1))
	}
	if got := len(s.Recent(100)); got != 5 {
		t.Fatalf("buffer should cap at 5, got %d entries", got)
	}
}

func TestBufCapFallsBackWhenUnset(t *testing.T) {
	s, err := NewStore(filepath.Join(t.TempDir(), "s.json"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.bufCap != 200 {
		t.Fatalf("0 should fall back to the 200 default, got %d", s.bufCap)
	}
}

// Aggregates must survive a restart even though the detail buffer is bounded.
func TestPersistAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	s, _ := NewStore(path, 10)
	s.RecordCall(call(true, 100))
	s.RecordCall(call(false, 0))
	s.save()

	again, err := NewStore(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Now().Format("2006-01-02")
	d := again.Snapshot()[day]
	if d == nil {
		t.Fatal("no aggregate reloaded")
	}
	if d.Total != 2 || d.OK != 1 || d.Failed != 1 || d.TotalTokens != 100 {
		t.Fatalf("aggregates lost on reload: %+v", d)
	}
	if got := len(again.Recent(10)); got != 2 {
		t.Fatalf("call details lost on reload: %d", got)
	}
}

func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.json")
	s, _ := NewStore(path, 10)
	for i := 0; i < 3; i++ {
		s.RecordCall(call(true, 1))
		s.save()
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A temp file left behind would mean the rename path is broken.
	for _, e := range ents {
		if e.Name() != "s.json" {
			t.Fatalf("unexpected leftover file: %s", e.Name())
		}
	}
}

func TestCorruptFileDoesNotPanic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	if err := os.WriteFile(path, []byte(`{"calls":[{"time":`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(path, 10)
	if err != nil {
		t.Fatalf("a truncated file should not fail startup: %v", err)
	}
	// Starting empty is the accepted outcome; crashing is not.
	if got := len(s.Recent(10)); got != 0 {
		t.Fatalf("expected empty store, got %d", got)
	}
}
