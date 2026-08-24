package manifest

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func LoadDirectory(directory, gatewayVersion string) ([]Validated, error) {
	if !filepath.IsAbs(directory) || strings.TrimSpace(directory) != directory {
		return nil, ErrInvalid
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
		return nil, ErrInvalid
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, ErrInvalid
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	results := make([]Validated, 0, len(entries))
	ids := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" || entry.Name() != filepath.Base(entry.Name()) {
			return nil, ErrInvalid
		}
		path := filepath.Join(directory, entry.Name())
		fileInfo, statErr := os.Lstat(path)
		if statErr != nil || !fileInfo.Mode().IsRegular() || fileInfo.Mode()&os.ModeSymlink != 0 || fileInfo.Mode().Perm()&0o022 != 0 || fileInfo.Size() < 1 || fileInfo.Size() > MaximumBytes {
			return nil, ErrInvalid
		}
		file, openErr := os.Open(path)
		if openErr != nil {
			return nil, ErrInvalid
		}
		body, readErr := io.ReadAll(io.LimitReader(file, MaximumBytes+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil {
			return nil, ErrInvalid
		}
		validated, parseErr := Parse(body, gatewayVersion)
		if parseErr != nil {
			return nil, fmt.Errorf("%w: %s", ErrInvalid, entry.Name())
		}
		if ids[validated.Manifest.ID] {
			return nil, ErrInvalid
		}
		ids[validated.Manifest.ID] = true
		results = append(results, validated)
	}
	return results, nil
}

func IsInvalid(err error) bool { return errors.Is(err, ErrInvalid) }
