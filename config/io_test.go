package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func sample() *Config {
	return &Config{
		JWTSecret: "s3cret",
		Upstreams: []Upstream{{Name: "a", Protocol: ProtocolOpenAI, BaseURL: "http://x", APIKey: "k1", Enabled: true}},
	}
}

func TestWriteFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeFile(path, sample()); err != nil {
		t.Fatal(err)
	}
	var got Config
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("written config is not valid JSON: %v", err)
	}
	if got.JWTSecret != "s3cret" || len(got.Upstreams) != 1 || got.Upstreams[0].APIKey != "k1" {
		t.Fatalf("round trip lost data: %+v", got)
	}
}

func TestWriteFileKeepsBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	first := sample()
	if err := writeFile(path, first); err != nil {
		t.Fatal(err)
	}
	second := sample()
	second.JWTSecret = "rotated"
	if err := writeFile(path, second); err != nil {
		t.Fatal(err)
	}

	var bak Config
	b, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("no backup written: %v", err)
	}
	if err := json.Unmarshal(b, &bak); err != nil {
		t.Fatal(err)
	}
	if bak.JWTSecret != "s3cret" {
		t.Fatalf("backup should hold the previous contents, got %q", bak.JWTSecret)
	}
}

func TestWriteFileFirstRunNeedsNoBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := writeFile(path, sample()); err != nil {
		t.Fatalf("first write must succeed with no existing file: %v", err)
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Fatal("first run should not create a backup")
	}
}

func TestWriteFileLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	for i := 0; i < 3; i++ {
		if err := writeFile(path, sample()); err != nil {
			t.Fatal(err)
		}
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if name := e.Name(); name != "config.json" && name != "config.json.bak" {
			t.Fatalf("leftover file after write: %s", name)
		}
	}
}
