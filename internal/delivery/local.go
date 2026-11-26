package delivery

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/andreimerlescu/lemmings/v2/internal/report"
)

var safeNameRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// LocalTarget writes rendered reports and standalone screenshot artifacts into
// <basePath>/lemmings/<domain>/ using os.Root confinement (SEC-06).
type LocalTarget struct {
	basePath    string
	onDelivered DeliveredFunc
}

// NewLocalTarget constructs a LocalTarget rooted at basePath.
func NewLocalTarget(basePath string, onDelivered DeliveredFunc) *LocalTarget {
	return &LocalTarget{basePath: basePath, onDelivered: onDelivered}
}

// Name returns a human-readable identifier for this target.
func (t *LocalTarget) Name() string {
	return fmt.Sprintf("local(%s)", t.basePath)
}

// Deliver writes .md, .html, .json, and any standalone .artifacts/ screenshots
// inside a directory confined by os.OpenRoot so traversal/symlink escapes cannot occur.
func (t *LocalTarget) Deliver(_ context.Context, r report.Rendered) error {
	if err := validateSafeComponent(r.Filename); err != nil {
		return fmt.Errorf("local target invalid filename %q: %w", r.Filename, err)
	}
	domain := extractDomain(r.Domain, r.Filename)
	if err := validateSafeComponent(domain); err != nil {
		return fmt.Errorf("local target invalid domain %q: %w", domain, err)
	}

	dir := filepath.Join(t.basePath, "lemmings", domain)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("local target mkdir %s: %w", dir, err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("local target open root %s: %w", dir, err)
	}
	defer root.Close() // read/root handle

	for _, f := range []struct {
		ext, label string
		body       []byte
	}{
		{".md", "md", []byte(r.Markdown)},
		{".html", "html", []byte(r.HTML)},
		{".json", "json", r.JSON},
	} {
		if f.body == nil {
			continue
		}
		name := r.Filename + f.ext
		if err := writeInRoot(root, name, f.body, 0o640); err != nil {
			return fmt.Errorf("local target write %s: %w", f.label, err)
		}
		if t.onDelivered != nil {
			t.onDelivered(f.label, filepath.Join(dir, name))
		}
	}

	if len(r.Artifacts) > 0 {
		artDir := r.Filename + ".artifacts"
		if err := root.Mkdir(artDir, 0o750); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("local target mkdir %s: %w", artDir, err)
		}
		for _, a := range r.Artifacts {
			if err := validateSafeComponent(a.Filename); err != nil {
				return fmt.Errorf("local target invalid artifact name %q: %w", a.Filename, err)
			}
			rel := filepath.Join(artDir, a.Filename)
			if err := writeInRoot(root, rel, a.Data, 0o640); err != nil {
				return fmt.Errorf("local target write artifact %s: %w", a.Filename, err)
			}
		}
		if t.onDelivered != nil {
			t.onDelivered("assets", filepath.Join(dir, artDir))
		}
	}
	return nil
}

func writeInRoot(root *os.Root, rel string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(rel)
	var (
		f      *os.File
		tmpRel string
	)
	for range 8 {
		var rnd [8]byte
		if _, rerr := rand.Read(rnd[:]); rerr != nil {
			return fmt.Errorf("generate temp filename: %w", rerr)
		}
		tmpName := ".tmp-" + hex.EncodeToString(rnd[:])
		if dir != "." && dir != "" {
			tmpRel = filepath.Join(dir, tmpName)
		} else {
			tmpRel = tmpName
		}
		f, err = root.OpenFile(tmpRel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	if f == nil {
		return fmt.Errorf("create unique temp file for %s: %w", rel, err)
	}

	closed := false
	renamed := false
	defer func() {
		if !closed {
			if cerr := f.Close(); cerr != nil {
				err = errors.Join(err, cerr)
			}
		}
		if !renamed {
			_ = root.Remove(tmpRel)
		}
	}()

	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		closed = true
		return err
	}
	closed = true

	if err = root.Rename(tmpRel, rel); err != nil {
		return err
	}
	renamed = true
	return nil
}

func extractDomain(explicit, filename string) string {
	if explicit != "" {
		return explicit
	}
	parts := strings.SplitN(filename, ".", 5)
	if len(parts) == 5 && parts[4] != "" {
		return parts[4]
	}
	return "unknown"
}

func validateSafeComponent(name string) error {
	if name == "" || name == "." || name == ".." || strings.Contains(name, "..") || strings.ContainsAny(name, `/\`) {
		return errors.New("unsafe path component")
	}
	for i := 0; i < len(name); i++ {
		if name[i] < 0x20 || name[i] == 0x7f {
			return errors.New("unsafe path component")
		}
	}
	if !safeNameRe.MatchString(name) {
		return errors.New("unsafe path component")
	}
	return nil
}
