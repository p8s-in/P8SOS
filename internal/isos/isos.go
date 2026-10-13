// Package isos keeps the host's library of installer ISO images, which VMs can
// boot from to install any operating system by hand (through the console).
package isos

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"p8sos/internal/netx"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ISO is one image in the library.
type ISO struct {
	Name    string `json:"name"`
	Bytes   int64  `json:"bytes"`
	AddedAt string `json:"added_at"`
}

var (
	ErrNotFound = errors.New("iso not found")
	ErrExists   = errors.New("iso already exists")
	ErrInvalid  = errors.New("invalid iso")
)

var reName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,99}\.iso$`)

// ValidName reports whether name is an acceptable file name (letters, digits,
// . _ + -, ending in .iso; no paths).
func ValidName(name string) bool { return reName.MatchString(name) && !strings.Contains(name, "..") }

// MaxBytes is the largest ISO accepted (DVD images of big distros are ~10 GB).
const MaxBytes = 16 << 30

type Library struct {
	Dir    string       // e.g. /var/lib/p8s/isos
	Client *http.Client // for downloads; nil = http.DefaultClient
}

// ipv4First downloads via netx: IPv4 first, system resolver and public DNS
// when Go's own DNS lookup fails (see package netx).
var ipv4First = netx.Client(0)

// ErrNoRoute means the download site can't be reached from this network.
var ErrNoRoute = netx.ErrNoRoute

// Presets are well-known downloads with mirrors, tried in order
// ("preset:virtio-win" as the URL).
var Presets = map[string][]string{
	// 0.1.271 is the version Proxmox recommends for production; "stable" may point to a newer test build.
	"virtio-win": {
		"https://fedorapeople.org/groups/virt/virtio-win/direct-downloads/archive-virtio/virtio-win-0.1.271-1/virtio-win.iso",
		"https://fedorapeople.org/groups/virt/virtio-win/direct-downloads/stable-virtio/virtio-win.iso",
	},
}

// PresetPage is where a person can get a preset by hand (shown when the server can't).
var PresetPage = map[string]string{
	"virtio-win": "https://fedorapeople.org/groups/virt/virtio-win/direct-downloads/archive-virtio/virtio-win-0.1.271-1/virtio-win.iso",
}

// Path returns the file of an existing ISO.
func (l *Library) Path(name string) (string, error) {
	if !ValidName(name) {
		return "", fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	p := filepath.Join(l.Dir, name)
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	return p, nil
}

// List returns all ISOs, sorted by name.
func (l *Library) List() ([]ISO, error) {
	ents, err := os.ReadDir(l.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return []ISO{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []ISO{}
	for _, e := range ents {
		if e.IsDir() || !ValidName(e.Name()) {
			continue
		}
		if fi, err := e.Info(); err == nil {
			out = append(out, ISO{Name: e.Name(), Bytes: fi.Size(), AddedAt: fi.ModTime().UTC().Format(time.RFC3339)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Save stores an ISO from r. The file appears only once it is complete and
// looks like a CD/DVD image.
func (l *Library) Save(name string, r io.Reader) (ISO, error) {
	if !ValidName(name) {
		return ISO{}, fmt.Errorf("%w: name must end in .iso and use only letters, digits, . _ + -", ErrInvalid)
	}
	// QEMU (user libvirt-qemu) must be able to read the images.
	if err := os.MkdirAll(l.Dir, 0o755); err != nil {
		return ISO{}, err
	}
	final := filepath.Join(l.Dir, name)
	if _, err := os.Stat(final); err == nil {
		return ISO{}, fmt.Errorf("%w: %q", ErrExists, name)
	}
	tmp := final + ".partial"
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return ISO{}, err
	}
	fail := func(err error) (ISO, error) { f.Close(); os.Remove(tmp); return ISO{}, err }
	n, err := io.Copy(f, io.LimitReader(r, MaxBytes+1))
	if err != nil {
		return fail(err)
	}
	if n > MaxBytes {
		return fail(fmt.Errorf("%w: larger than %d GB", ErrInvalid, MaxBytes>>30))
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if !looksLikeISO(f) {
		return fail(fmt.Errorf("%w: %q is not a CD/DVD image (no ISO 9660 or UDF signature)", ErrInvalid, name))
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return ISO{}, err
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return ISO{}, err
	}
	return ISO{Name: name, Bytes: n, AddedAt: time.Now().UTC().Format(time.RFC3339)}, nil
}

// looksLikeISO checks the volume descriptor at sector 16 (offset 32769):
// "CD001" (ISO 9660) or "BEA01" (UDF, e.g. Windows images).
func looksLikeISO(f *os.File) bool {
	b := make([]byte, 5)
	if _, err := f.ReadAt(b, 32769); err != nil {
		return false
	}
	return string(b) == "CD001" || string(b) == "BEA01"
}

// Delete removes an ISO.
func (l *Library) Delete(name string) error {
	p, err := l.Path(name)
	if err != nil {
		return err
	}
	return os.Remove(p)
}

// NameFromURL suggests a library name for a download URL.
func NameFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	n := path.Base(u.Path)
	if !strings.HasSuffix(strings.ToLower(n), ".iso") {
		return ""
	}
	return n
}

// Download fetches an ISO over HTTP(S) into the library. If sha256hex is
// given, the file is checked against it before it is kept.
func (l *Library) Download(ctx context.Context, rawURL, name, sha256hex string, progress func(done, total int64)) (ISO, error) {
	if key, ok := strings.CutPrefix(rawURL, "preset:"); ok {
		urls := Presets[key]
		if len(urls) == 0 {
			return ISO{}, fmt.Errorf("%w: unknown download %q", ErrInvalid, key)
		}
		var last error
		for _, u := range urls {
			iso, err := l.Download(ctx, u, name, sha256hex, progress)
			if err == nil || errors.Is(err, ErrExists) || errors.Is(err, ErrInvalid) || ctx.Err() != nil {
				return iso, err
			}
			last = err
		}
		if errors.Is(last, ErrNoRoute) {
			return ISO{}, fmt.Errorf("%v. Download it on your computer from %s and add it with ISO images → Upload, or turn on IPv6 on your router",
				last, PresetPage[key])
		}
		return ISO{}, last
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return ISO{}, fmt.Errorf("%w: url must be http(s)://...", ErrInvalid)
	}
	if name == "" {
		name = NameFromURL(rawURL)
	}
	if !ValidName(name) {
		return ISO{}, fmt.Errorf("%w: give a name ending in .iso", ErrInvalid)
	}
	sha256hex = strings.ToLower(strings.TrimSpace(sha256hex))
	if sha256hex != "" && !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(sha256hex) {
		return ISO{}, fmt.Errorf("%w: sha256 must be 64 hex characters", ErrInvalid)
	}
	if _, err := os.Stat(filepath.Join(l.Dir, name)); err == nil {
		return ISO{}, fmt.Errorf("%w: %q", ErrExists, name)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return ISO{}, err
	}
	c := l.Client
	if c == nil {
		c = ipv4First
	}
	resp, err := c.Do(req)
	if err != nil {
		return ISO{}, fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ISO{}, fmt.Errorf("download: %s answered %s", u.Host, resp.Status)
	}
	h := sha256.New()
	pr := &progressReader{r: io.TeeReader(resp.Body, h), total: resp.ContentLength, fn: progress}
	iso, err := l.saveChecked(name, pr, func() error {
		if sha256hex != "" && hex.EncodeToString(h.Sum(nil)) != sha256hex {
			return fmt.Errorf("%w: SHA-256 does not match (download corrupted or tampered)", ErrInvalid)
		}
		return nil
	})
	return iso, err
}

// saveChecked is Save with an extra check before the file is published.
func (l *Library) saveChecked(name string, r io.Reader, check func() error) (ISO, error) {
	cr := &checkReader{r: r, check: check}
	return l.Save(name, cr)
}

// checkReader runs check when r is exhausted; an error fails the copy.
type checkReader struct {
	r     io.Reader
	check func() error
	done  bool
}

func (c *checkReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if err == io.EOF && !c.done {
		c.done = true
		if cerr := c.check(); cerr != nil {
			return n, cerr
		}
	}
	return n, err
}

type progressReader struct {
	r     io.Reader
	done  int64
	total int64
	last  time.Time
	fn    func(done, total int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.done += int64(n)
	if p.fn != nil && (time.Since(p.last) > time.Second || err == io.EOF) {
		p.last = time.Now()
		p.fn(p.done, p.total)
	}
	return n, err
}
