package report

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type ArchiveEntry struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// ArchiveRun writes a portable, content-addressed tar.gz for one isolated run.
// Duplicate input content appears only once under blobs/<sha256>; manifest.json
// maps each logical name back to its blob. Only explicitly supplied files enter
// the archive, so it cannot accidentally include credentials from state_dir.
func ArchiveRun(outputPath string, files map[string]string) error {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(outputPath), ".archive-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	zipped := gzip.NewWriter(tmp)
	tw := tar.NewWriter(zipped)
	var entries []ArchiveEntry
	seen := map[string]bool{}
	for _, name := range names {
		if name == "" || filepath.Base(name) != name {
			err = fmt.Errorf("invalid archive entry %q", name)
			break
		}
		path := files[name]
		file, openErr := os.Open(path)
		if os.IsNotExist(openErr) {
			continue
		}
		if openErr != nil {
			err = openErr
			break
		}
		info, statErr := file.Stat()
		if statErr != nil {
			_ = file.Close()
			err = statErr
			break
		}
		if !info.Mode().IsRegular() {
			_ = file.Close()
			err = fmt.Errorf("archive input %q is not a regular file", path)
			break
		}
		hash := sha256.New()
		if _, copyErr := io.Copy(hash, file); copyErr != nil {
			_ = file.Close()
			err = copyErr
			break
		}
		checksum := hex.EncodeToString(hash.Sum(nil))
		entries = append(entries, ArchiveEntry{Name: name, SHA256: checksum, Size: info.Size()})
		if !seen[checksum] {
			if _, seekErr := file.Seek(0, io.SeekStart); seekErr != nil {
				_ = file.Close()
				err = seekErr
				break
			}
			if writeErr := tw.WriteHeader(&tar.Header{Name: "blobs/" + checksum, Mode: 0o600,
				Size: info.Size(), ModTime: time.Unix(0, 0)}); writeErr != nil {
				_ = file.Close()
				err = writeErr
				break
			}
			if _, copyErr := io.Copy(tw, file); copyErr != nil {
				_ = file.Close()
				err = copyErr
				break
			}
			seen[checksum] = true
		}
		_ = file.Close()
	}
	if err == nil && len(entries) == 0 {
		err = fmt.Errorf("archive has no readable inputs")
	}
	if err == nil {
		manifest, marshalErr := json.MarshalIndent(map[string]any{"version": 1, "entries": entries}, "", "  ")
		if marshalErr != nil {
			err = marshalErr
		} else {
			if writeErr := tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o600,
				Size: int64(len(manifest)), ModTime: time.Unix(0, 0)}); writeErr != nil {
				err = writeErr
			} else {
				_, err = tw.Write(manifest)
			}
		}
	}
	if closeErr := tw.Close(); err == nil {
		err = closeErr
	}
	if closeErr := zipped.Close(); err == nil {
		err = closeErr
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), outputPath)
}
