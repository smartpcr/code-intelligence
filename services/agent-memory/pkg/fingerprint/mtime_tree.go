package fingerprint

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
)

// MTimeTreeSHA computes a deterministic 32-character lowercase
// hex digest of the mtime/size tree rooted at rootDir.
//
// It walks rootDir directly via os.ReadDir / os.Stat (NOT via
// repoindexer.Workspace.Walk, because the synthesised SHA for
// non-git local scans must be available BEFORE the materializer
// constructs a Workspace -- see architecture.md S4.3 / S9.1).
// Any directory whose base name appears in excludes is pruned
// from the walk (matching gitWorkspace's exclude semantics: base
// name match, not full path). The root directory itself is never
// excluded by name.
//
// For every surviving regular file the helper accumulates the
// pre-image:
//
//	relPath || 0x00 || mtime-unix-seconds || 0x00 || size || 0x00
//
// where relPath is the slash-separated path relative to rootDir,
// mtime is stat.ModTime().UTC().Unix() rendered as a base-10
// signed integer, and size is stat.Size() rendered as a base-10
// signed integer. Files are emitted in stable lexicographic order
// of relPath. The returned string is the lowercase hex encoding
// of the first 16 bytes of sha256(pre-image), giving the 32-char
// signature documented in architecture.md S4.3.
//
// A non-nil error is returned (with an empty hash string) when
// rootDir does not exist, is not a directory, or any os.ReadDir /
// os.Stat call fails. Symlinks, sockets, devices, and named
// pipes are skipped silently -- only regular files contribute to
// the digest.
func MTimeTreeSHA(rootDir string, excludes []string) (string, error) {
	info, err := os.Stat(rootDir)
	if err != nil {
		return "", fmt.Errorf("fingerprint: MTimeTreeSHA: stat %s: %w", rootDir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("fingerprint: MTimeTreeSHA: %s is not a directory", rootDir)
	}

	excludeSet := make(map[string]struct{}, len(excludes))
	for _, name := range excludes {
		if name == "" {
			continue
		}
		excludeSet[name] = struct{}{}
	}

	type entry struct {
		relPath string
		mtime   int64
		size    int64
	}
	var entries []entry

	var walk func(absDir, relDir string) error
	walk = func(absDir, relDir string) error {
		dirEntries, err := os.ReadDir(absDir)
		if err != nil {
			return fmt.Errorf("fingerprint: MTimeTreeSHA: readdir %s: %w", absDir, err)
		}
		for _, de := range dirEntries {
			name := de.Name()
			childAbs := filepath.Join(absDir, name)
			childRel := name
			if relDir != "" {
				childRel = path.Join(relDir, name)
			}
			if de.IsDir() {
				if _, skip := excludeSet[name]; skip {
					continue
				}
				if err := walk(childAbs, childRel); err != nil {
					return err
				}
				continue
			}
			// Resolve via os.Stat so symlinks pointing at a
			// regular file are followed -- but the stat must
			// still be a regular file to contribute. This
			// keeps parity with the architecture S4.3 wording
			// ("via os.ReadDir / os.Stat").
			fi, err := os.Stat(childAbs)
			if err != nil {
				// Skip transient races (file removed between
				// ReadDir and Stat); other errors surface.
				if os.IsNotExist(err) {
					continue
				}
				return fmt.Errorf("fingerprint: MTimeTreeSHA: stat %s: %w", childAbs, err)
			}
			if !fi.Mode().IsRegular() {
				continue
			}
			entries = append(entries, entry{
				relPath: childRel,
				mtime:   fi.ModTime().UTC().Unix(),
				size:    fi.Size(),
			})
		}
		return nil
	}

	if err := walk(rootDir, ""); err != nil {
		return "", err
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].relPath < entries[j].relPath
	})

	h := sha256.New()
	var sep = []byte{0x00}
	for _, e := range entries {
		h.Write([]byte(e.relPath))
		h.Write(sep)
		h.Write([]byte(strconv.FormatInt(e.mtime, 10)))
		h.Write(sep)
		h.Write([]byte(strconv.FormatInt(e.size, 10)))
		h.Write(sep)
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:16]), nil
}
