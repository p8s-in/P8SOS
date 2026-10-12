// Package release makes, signs, checks and unpacks P8S OS update bundles.
//
// A bundle is a .tar.gz of the source tree (what update.sh installs) with a
// VERSION file. P8S signs "version + sha256 + size" with the same Ed25519 key
// as licences, so a server only installs updates P8S made: p8sd checks the
// checksum and the signature before it unpacks anything.
package release

import (
	"archive/tar"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Manifest describes one release.
type Manifest struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
	Notes   string `json:"notes,omitempty"`
	Date    string `json:"date"`
}

var reVersion = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._+-]{0,39}$`)

// ValidVersion: 1-40 of letters, digits, . _ + -
func ValidVersion(v string) bool { return reVersion.MatchString(v) }

func (m Manifest) message() []byte {
	return []byte(fmt.Sprintf("p8s-release:v1\n%s\n%s\n%d\n", m.Version, m.SHA256, m.Size))
}

// Sign returns the signature (base64) of a manifest.
func Sign(priv ed25519.PrivateKey, m Manifest) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, m.message()))
}

// Verify checks a manifest's signature against trusted keys.
func Verify(keys []ed25519.PublicKey, m Manifest, sig string) bool {
	s, err := base64.StdEncoding.DecodeString(sig)
	if err != nil || len(s) != ed25519.SignatureSize {
		return false
	}
	for _, k := range keys {
		if len(k) == ed25519.PublicKeySize && ed25519.Verify(k, m.message(), s) {
			return true
		}
	}
	return false
}

// skip leaves build output, VCS data, disk images and secrets out of bundles.
func skip(rel string, d fs.DirEntry) bool {
	base := d.Name()
	if d.IsDir() {
		switch base {
		case ".git", "bin", "dist", ".secrets", "node_modules":
			return true
		}
		return false
	}
	for _, ext := range []string{".iso", ".img", ".qcow2", ".key", ".tar.gz", ".tmp"} {
		if strings.HasSuffix(base, ext) {
			return true
		}
	}
	return rel == "VERSION"
}

// Bundle writes dir as a release bundle to out and returns its manifest.
func Bundle(dir, version, notes string, out string) (Manifest, error) {
	if !ValidVersion(version) {
		return Manifest{}, fmt.Errorf("version %q: 1-40 letters, digits, . _ + -", version)
	}
	if _, err := os.Stat(filepath.Join(dir, "update.sh")); err != nil {
		return Manifest{}, fmt.Errorf("%s is not a P8S OS source folder (no update.sh)", dir)
	}
	f, err := os.OpenFile(out+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return Manifest{}, err
	}
	defer os.Remove(out + ".tmp")
	h := sha256.New()
	cw := &countWriter{w: io.MultiWriter(f, h)}
	gz := gzip.NewWriter(cw)
	tw := tar.NewWriter(gz)
	mod := time.Now().UTC()
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if skip(rel, d) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return tw.WriteHeader(&tar.Header{Name: rel + "/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: mod})
		case info.Mode().IsRegular():
			mode := int64(0o644)
			if info.Mode()&0o111 != 0 || strings.HasSuffix(rel, ".sh") || rel == "scripts/p8" {
				mode = 0o755
			}
			if err := tw.WriteHeader(&tar.Header{Name: rel, Typeflag: tar.TypeReg, Mode: mode, Size: info.Size(), ModTime: mod}); err != nil {
				return err
			}
			src, err := os.Open(p)
			if err != nil {
				return err
			}
			defer src.Close()
			_, err = io.Copy(tw, src)
			return err
		}
		return nil // symlinks and devices are left out
	})
	if err == nil {
		v := []byte(version + "\n")
		if err = tw.WriteHeader(&tar.Header{Name: "VERSION", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(v)), ModTime: mod}); err == nil {
			_, err = tw.Write(v)
		}
	}
	if err == nil {
		err = tw.Close()
	}
	if err == nil {
		err = gz.Close()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return Manifest{}, err
	}
	if err := os.Rename(out+".tmp", out); err != nil {
		return Manifest{}, err
	}
	return Manifest{Version: version, SHA256: hex.EncodeToString(h.Sum(nil)), Size: cw.n, Notes: notes, Date: mod.Format("2006-01-02")}, nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// FileSHA256 returns a file's checksum and size.
func FileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// MaxSize is the largest bundle p8sd accepts.
const MaxSize = 200 << 20

// Extract unpacks a bundle into dest (which must not exist), refusing paths
// outside dest, links and anything that isn't a plain file or folder.
func Extract(bundle, dest string) error {
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("%s exists", dest)
	}
	f, err := os.Open(bundle)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(filepath.FromSlash(h.Name))
		if name == "." || filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("bad path in bundle: %q", h.Name)
		}
		p := filepath.Join(dest, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			total += h.Size
			if total > 4*MaxSize {
				return errors.New("bundle unpacks too large")
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			out, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, io.LimitReader(tr, h.Size))
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported entry in bundle: %q", h.Name)
		}
	}
	if _, err := os.Stat(filepath.Join(dest, "update.sh")); err != nil {
		return errors.New("bundle has no update.sh")
	}
	return nil
}
