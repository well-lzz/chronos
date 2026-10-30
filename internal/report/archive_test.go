package report

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArchiveRunDeduplicatesBlobsAndKeepsManifest(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "one.txt")
	second := filepath.Join(dir, "two.txt")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, []byte("same evidence"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	archivePath := filepath.Join(dir, "archive.tar.gz")
	if err := ArchiveRun(archivePath, map[string]string{"report.md": first, "evidence.txt": second}); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	zipped, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer zipped.Close()
	reader := tar.NewReader(zipped)
	blobs := 0
	var entries []ArchiveEntry
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(header.Name, "blobs/") {
			blobs++
		}
		if header.Name == "manifest.json" {
			var manifest struct {
				Entries []ArchiveEntry `json:"entries"`
			}
			if err := json.NewDecoder(reader).Decode(&manifest); err != nil {
				t.Fatal(err)
			}
			entries = manifest.Entries
		}
	}
	if blobs != 1 || len(entries) != 2 || entries[0].SHA256 != entries[1].SHA256 {
		t.Fatalf("content dedup failed: blobs=%d entries=%+v", blobs, entries)
	}
}
