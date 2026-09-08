package stats

import (
	"os"
	"testing"
	"time"
)

// Regression: RecordCall must not panic when DayAgg.ByModel is nil, and must
// populate it. A missing nil-map guard previously caused a panic on the first
// call carrying a RealModel, leaving the per-model ranking permanently empty.
func TestRecordCallByModelNoPanic(t *testing.T) {
	s, err := NewStore(t.TempDir()+"/stats.json", 10)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	base := time.Now()
	models := []string{"a/model-x", "a/model-x", "b/model-y"}
	for i, m := range models {
		s.RecordCall(Call{
			Time:      base.Add(time.Duration(i) * time.Minute),
			RealModel: m,
			OK:        true,
			TotalTokens: 10,
		})
	}

	day := base.Format("2006-01-02")
	d := s.Snapshot()[day]
	if d == nil {
		t.Fatal("expected day aggregate")
	}
	if len(d.ByModel) != 2 {
		t.Fatalf("expected 2 distinct models, got %d: %v", len(d.ByModel), d.ByModel)
	}
	if d.ByModel["a/model-x"].Total != 2 {
		t.Fatalf("expected a/model-x total=2, got %d", d.ByModel["a/model-x"].Total)
	}
	if d.ByModel["b/model-y"].Total != 1 {
		t.Fatalf("expected b/model-y total=1, got %d", d.ByModel["b/model-y"].Total)
	}
}

// Regression: load() must rebuild ByModel from the recent-call buffer when the
// persisted snapshot left it empty (the state produced by the nil-map bug).
func TestLoadRebuildsByModel(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/stats.json"
	day := "2026-09-08"
	// Craft a buggy persisted file: calls carry real_model, but by_day has no
	// by_model map at all.
	raw := `{
  "calls": [
    {"time":"2026-09-08T10:00:00Z","real_model":"m1","ok":true,"total_tokens":10},
    {"time":"2026-09-08T10:01:00Z","real_model":"m1","ok":true,"total_tokens":20},
    {"time":"2026-09-08T10:02:00Z","real_model":"m2","ok":false,"total_tokens":0}
  ],
  "by_day": {
    "2026-09-08": {
      "total": 3, "ok": 2, "failed": 1
    }
  },
  "saved_at":"2026-09-08T10:05:00Z"
}`
	if err := writeFileForTest(path, []byte(raw)); err != nil {
		t.Fatalf("write: %v", err)
	}

	s, err := NewStore(path, 50)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	d := s.Snapshot()[day]
	if d == nil || d.ByModel == nil {
		t.Fatalf("expected ByModel rebuilt from buffer, got day=%v", d)
	}
	if d.ByModel["m1"].Total != 2 || d.ByModel["m1"].TotalTokens != 30 {
		t.Fatalf("expected m1 total=2 tokens=30 after rebuild, got %+v", d.ByModel["m1"])
	}
	if d.ByModel["m2"].Total != 1 || d.ByModel["m2"].Failed != 1 {
		t.Fatalf("expected m2 total=1 failed=1 after rebuild, got %+v", d.ByModel["m2"])
	}
}

func writeFileForTest(path string, data []byte) error {
	return os.WriteFile(path, data, 0o600)
}
