// Package offsite copies backups off the server: to a folder (USB disk, NAS
// mount), to S3-compatible storage (AWS S3, Backblaze B2, Cloudflare R2,
// Wasabi, MinIO) or to another server over SSH. Every finished backup is
// copied to each enabled target; each target keeps the newest N per VM or
// container; a copy can be fetched back and restored like a local backup.
//
// Layout at a target: [prefix/]vm|ct/<guest>/<guest>-<id>.zfs.zst[.p8senc]
// plus <guest>-<id>.json (written last: a backup counts once its .json is there).
package offsite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"p8sos/internal/backup"
	"p8sos/internal/jobs"
)

// Target is one place backups are copied to.
type Target struct {
	Name    string `json:"name"`
	Type    string `json:"type"` // dir, s3, ssh
	Enabled bool   `json:"enabled"`
	Keep    int    `json:"keep"` // newest backups kept per VM/container at the target

	Path string `json:"path,omitempty"` // dir: e.g. /mnt/usb-backup

	Endpoint    string `json:"endpoint,omitempty"` // s3
	Region      string `json:"region,omitempty"`
	Bucket      string `json:"bucket,omitempty"`
	Prefix      string `json:"prefix,omitempty"` // folder inside the bucket / below the path
	AccessKey   string `json:"access_key,omitempty"`
	SecretKey   string `json:"secret_key,omitempty"`
	VirtualHost bool   `json:"virtual_host,omitempty"` // bucket.endpoint instead of endpoint/bucket

	Host      string `json:"host,omitempty"` // ssh
	User      string `json:"user,omitempty"`
	Port      int    `json:"port,omitempty"`
	RemoteDir string `json:"remote_dir,omitempty"`

	// Passphrase encrypts what is stored (AES-256-GCM). Without it nobody can
	// restore these copies — not even P8S support.
	Passphrase string `json:"passphrase,omitempty"`
}

// Status is how a target is doing.
type Status struct {
	LastOK    string `json:"last_ok,omitempty"`    // last successful copy (RFC 3339)
	LastError string `json:"last_error,omitempty"` // empty when the last attempt worked
	ErrorAt   string `json:"error_at,omitempty"`
	Copies    int    `json:"copies"` // backups copied since p8sd started
	Bytes     int64  `json:"bytes"`
	Running   string `json:"running,omitempty"` // what it is copying now
}

// View is a target as the dashboard sees it (secrets hidden).
type View struct {
	Target
	HasSecret     bool   `json:"has_secret"`
	HasPassphrase bool   `json:"has_passphrase"`
	Status        Status `json:"status"`
}

// Remote is a backup stored at a target.
type Remote struct {
	Kind      string `json:"kind"` // vm or ct
	Guest     string `json:"guest"`
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
	Owner     string `json:"owner,omitempty"`
	Bytes     int64  `json:"bytes"`
	Encrypted bool   `json:"encrypted"`
	Local     bool   `json:"local"` // also on this server
	data      string
	meta      string
}

var (
	ErrInvalid = errors.New("invalid")
	ErrNoSuch  = errors.New("no such target")
	reName     = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	reGuest    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,47}[a-z0-9]$`)
	reID       = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}$`)
	rePrefix   = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)
)

const encExt = ".p8senc"

// Manager keeps the targets and copies backups.
type Manager struct {
	File      string // data_dir/offsite.json (targets and secrets, mode 0600)
	BackupDir string // local backups (VMs at <dir>/<vm>, containers at <dir>/_ct/<name>)
	KeyFile   string // SSH key for ssh targets (made on first use)
	Log       *slog.Logger
	Jobs      *jobs.Manager
	HTTP      *http.Client
	Cmd       func(ctx context.Context, name string, args ...string) *exec.Cmd
	Allowed   func() bool // subscription allows off-site copies; nil = yes
	Now       func() time.Time

	mu      sync.Mutex
	targets []Target
	loaded  bool
	status  map[string]*Status
	tmu     sync.Map // target name -> *sync.Mutex (one copy at a time per target)
	queue   chan item
	qonce   sync.Once
}

type item struct{ kind, guest, id string }

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) cmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	if m.Cmd != nil {
		return m.Cmd(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...)
}

func (m *Manager) load() error {
	if m.loaded {
		return nil
	}
	m.status = map[string]*Status{}
	b, err := os.ReadFile(m.File)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		var st struct {
			Targets []Target          `json:"targets"`
			Status  map[string]Status `json:"status"`
		}
		if err := json.Unmarshal(b, &st); err != nil {
			return err
		}
		m.targets = st.Targets
		for k, v := range st.Status {
			v := v
			v.Running = ""
			m.status[k] = &v
		}
	}
	m.loaded = true
	return nil
}

func (m *Manager) save() error {
	st := map[string]Status{}
	for k, v := range m.status {
		st[k] = *v
	}
	b, _ := json.MarshalIndent(map[string]any{"targets": m.targets, "status": st}, "", "  ")
	if m.File == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(m.File), 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(m.File+".tmp", b, 0o600); err != nil {
		return err
	}
	return os.Rename(m.File+".tmp", m.File)
}

func (m *Manager) stat(name string) *Status {
	s := m.status[name]
	if s == nil {
		s = &Status{}
		m.status[name] = s
	}
	return s
}

// Targets lists the targets with secrets hidden.
func (m *Manager) Targets() ([]View, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.load(); err != nil {
		return nil, err
	}
	out := []View{}
	for _, t := range m.targets {
		v := View{Target: t, HasSecret: t.SecretKey != "", HasPassphrase: t.Passphrase != "", Status: *m.stat(t.Name)}
		v.SecretKey, v.Passphrase = "", ""
		out = append(out, v)
	}
	return out, nil
}

func (m *Manager) target(name string) (Target, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.load(); err != nil {
		return Target{}, err
	}
	for _, t := range m.targets {
		if t.Name == name {
			return t, nil
		}
	}
	return Target{}, fmt.Errorf("%w %q", ErrNoSuch, name)
}

func validate(t *Target) error {
	bad := func(f string, a ...any) error { return fmt.Errorf("%w: "+f, append([]any{ErrInvalid}, a...)...) }
	if !reName.MatchString(t.Name) {
		return bad("name: lowercase letters, digits and dashes, starting with a letter")
	}
	if t.Keep == 0 {
		t.Keep = 7
	}
	if t.Keep < 1 || t.Keep > 365 {
		return bad("keep must be 1-365")
	}
	t.Prefix = strings.Trim(t.Prefix, "/")
	if t.Prefix != "" && !rePrefix.MatchString(t.Prefix) {
		return bad("folder: letters, digits, . _ - and /")
	}
	if t.Passphrase != "" && len(t.Passphrase) < 12 {
		return bad("passphrase: at least 12 characters")
	}
	switch t.Type {
	case "dir":
		if !filepath.IsAbs(t.Path) || filepath.Clean(t.Path) == "/" {
			return bad("path must be a folder like /mnt/usb-backup")
		}
		t.Path = filepath.Clean(t.Path)
		for _, no := range []string{"/home", "/root", "/etc", "/usr", "/boot", "/proc", "/sys", "/dev"} {
			if t.Path == no || strings.HasPrefix(t.Path, no+"/") {
				return bad("p8sd can't write below %s; mount the disk under /mnt or /media", no)
			}
		}
	case "s3":
		if _, err := newS3(*t, nil); err != nil {
			return bad("%v", err)
		}
		if t.Bucket == "" || strings.ContainsAny(t.Bucket, "/ ") {
			return bad("bucket name needed")
		}
		if t.AccessKey == "" || t.SecretKey == "" {
			return bad("access key and secret key needed")
		}
	case "ssh":
		if !reHost.MatchString(t.Host) {
			return bad("host: a name or IP address")
		}
		if t.User == "" {
			t.User = "root"
		}
		if !reUser.MatchString(t.User) {
			return bad("user name")
		}
		if t.Port == 0 {
			t.Port = 22
		}
		if t.Port < 1 || t.Port > 65535 {
			return bad("port 1-65535")
		}
		if t.RemoteDir == "" {
			t.RemoteDir = "/var/lib/p8s-offsite"
		}
		if !reRemote.MatchString(t.RemoteDir) || strings.Contains(t.RemoteDir, "..") {
			return bad("remote folder: an absolute path like /backup/p8s")
		}
	default:
		return bad("type must be dir, s3 or ssh")
	}
	return nil
}

// Save adds or changes a target. Empty secret key / passphrase keep the old ones.
func (m *Manager) Save(t Target) (View, error) {
	m.mu.Lock()
	var old *Target
	if err := m.load(); err != nil {
		m.mu.Unlock()
		return View{}, err
	}
	for i := range m.targets {
		if m.targets[i].Name == t.Name {
			old = &m.targets[i]
		}
	}
	if old != nil {
		if t.SecretKey == "" {
			t.SecretKey = old.SecretKey
		}
		if t.Passphrase == "" {
			t.Passphrase = old.Passphrase
		}
	}
	if err := validate(&t); err != nil {
		m.mu.Unlock()
		return View{}, err
	}
	if old != nil {
		*old = t
	} else {
		m.targets = append(m.targets, t)
	}
	sort.Slice(m.targets, func(i, j int) bool { return m.targets[i].Name < m.targets[j].Name })
	err := m.save()
	m.mu.Unlock()
	if err != nil {
		return View{}, err
	}
	for _, v := range must(m.Targets()) {
		if v.Name == t.Name {
			return v, nil
		}
	}
	return View{}, nil
}

func must(v []View, _ error) []View { return v }

// Delete forgets a target (what is stored there stays).
func (m *Manager) Delete(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.load(); err != nil {
		return err
	}
	for i, t := range m.targets {
		if t.Name == name {
			m.targets = append(m.targets[:i], m.targets[i+1:]...)
			delete(m.status, name)
			return m.save()
		}
	}
	return fmt.Errorf("%w %q", ErrNoSuch, name)
}

// PublicKey returns this server's SSH key for ssh targets (made on first use).
func (m *Manager) PublicKey(ctx context.Context) (string, error) {
	if m.KeyFile == "" {
		return "", errors.New("no key file configured")
	}
	if _, err := os.Stat(m.KeyFile); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(m.KeyFile), 0o700); err != nil {
			return "", err
		}
		host, _ := os.Hostname()
		if out, err := m.cmd(ctx, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "p8s-offsite@"+host, "-f", m.KeyFile).CombinedOutput(); err != nil {
			return "", fmt.Errorf("ssh-keygen: %v %s", err, out)
		}
	}
	b, err := os.ReadFile(m.KeyFile + ".pub")
	return strings.TrimSpace(string(b)), err
}

func (m *Manager) backend(ctx context.Context, t Target) (Backend, string, error) {
	switch t.Type {
	case "dir":
		return dirBackend{root: t.Path}, t.Prefix, nil
	case "s3":
		b, err := newS3(t, m.HTTP)
		return b, t.Prefix, err
	case "ssh":
		if _, err := m.PublicKey(ctx); err != nil {
			return nil, "", err
		}
		return &sshBackend{host: t.Host, user: t.User, port: t.Port, dir: t.RemoteDir, keyFile: m.KeyFile,
			knownHosts: m.KeyFile + ".known_hosts", cmd: m.cmd}, t.Prefix, nil
	}
	return nil, "", fmt.Errorf("%w: type %q", ErrInvalid, t.Type)
}

func join(prefix string, parts ...string) string {
	if prefix != "" {
		parts = append([]string{prefix}, parts...)
	}
	return path.Join(parts...)
}

// Test checks a target by writing, reading and removing a small file.
func (m *Manager) Test(ctx context.Context, name string) error {
	t, err := m.target(name)
	if err != nil {
		return err
	}
	b, prefix, err := m.backend(ctx, t)
	if err != nil {
		return err
	}
	key := join(prefix, ".p8s-test")
	msg := "P8S OS off-site test " + m.now().UTC().Format(time.RFC3339)
	if err := b.Put(ctx, key, strings.NewReader(msg)); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	var got bytes.Buffer
	if err := b.Get(ctx, key, &got); err != nil {
		return fmt.Errorf("read back: %w", err)
	}
	if got.String() != msg {
		return errors.New("read back something else than was written")
	}
	if err := b.Delete(ctx, key); err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	if _, err := b.List(ctx, join(prefix, "vm")); err != nil {
		return fmt.Errorf("list: %w", err)
	}
	return nil
}

// --- local backups ---

func (m *Manager) store(kind string) *backup.Store {
	dir := m.BackupDir
	if kind == "ct" {
		dir = filepath.Join(dir, "_ct")
	}
	return &backup.Store{Dir: dir}
}

func (m *Manager) local() (map[string]backup.Meta, error) {
	out := map[string]backup.Meta{}
	for _, kind := range []string{"vm", "ct"} {
		l, err := m.store(kind).List("")
		if err != nil {
			return nil, err
		}
		for _, b := range l {
			out[kind+"/"+b.VM+"/"+b.ID] = b
		}
	}
	return out, nil
}

// --- copying ---

func (m *Manager) lockTarget(name string) func() {
	v, _ := m.tmu.LoadOrStore(name, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (m *Manager) note(name string, f func(s *Status)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.load() != nil {
		return
	}
	f(m.stat(name))
	_ = m.save()
}

// upload copies one local backup to t (skipped when already there).
func (m *Manager) upload(ctx context.Context, t Target, b Backend, prefix, kind string, bm backup.Meta, have map[string]bool) (int64, error) {
	data, meta := m.store(kind).Files(bm)
	base := bm.VM + "-" + bm.ID
	metaKey := join(prefix, kind, bm.VM, base+".json")
	if have[metaKey] {
		return 0, nil
	}
	dataKey := join(prefix, kind, bm.VM, filepath.Base(data))
	f, err := os.Open(data)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	cr := &countReader{r: f}
	var r io.Reader = cr
	if t.Passphrase != "" {
		dataKey += encExt
		if r, err = Encrypt(cr, t.Passphrase); err != nil {
			return 0, err
		}
	}
	m.note(t.Name, func(s *Status) { s.Running = kind + " " + bm.VM + " " + bm.ID })
	defer m.note(t.Name, func(s *Status) { s.Running = "" })
	if err := b.Put(ctx, dataKey, r); err != nil {
		return 0, err
	}
	mb, err := os.ReadFile(meta)
	if err != nil {
		return 0, err
	}
	if err := b.Put(ctx, metaKey, bytes.NewReader(mb)); err != nil {
		return 0, err
	}
	return cr.n, nil
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// remote lists the complete backups at a target.
func (m *Manager) remote(ctx context.Context, b Backend, prefix string) ([]Remote, error) {
	objs, err := b.List(ctx, join(prefix, ""))
	if err != nil {
		return nil, err
	}
	files := map[string]Object{}
	for _, o := range objs {
		files[o.Key] = o
	}
	var out []Remote
	for _, o := range objs {
		rel := strings.TrimPrefix(strings.TrimPrefix(o.Key, prefix), "/")
		parts := strings.Split(rel, "/")
		if len(parts) != 3 || (parts[0] != "vm" && parts[0] != "ct") || !strings.HasSuffix(parts[2], ".json") {
			continue
		}
		guest := parts[1]
		id := strings.TrimSuffix(strings.TrimPrefix(parts[2], guest+"-"), ".json")
		if !reGuest.MatchString(guest) || !reID.MatchString(id) {
			continue
		}
		r := Remote{Kind: parts[0], Guest: guest, ID: id, meta: o.Key}
		for _, ext := range []string{".zfs.zst", ".zfs.gz"} {
			for _, enc := range []string{"", encExt} {
				k := join(prefix, parts[0], guest, guest+"-"+id+ext+enc)
				if d, ok := files[k]; ok {
					r.data, r.Bytes, r.Encrypted = k, d.Size, enc != ""
				}
			}
		}
		if r.data == "" {
			continue // metadata without data: an interrupted copy
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID != out[j].ID {
			return out[i].ID > out[j].ID
		}
		return out[i].Kind+out[i].Guest < out[j].Kind+out[j].Guest
	})
	return out, nil
}

// prune keeps the newest t.Keep backups per guest at the target.
func (m *Manager) prune(ctx context.Context, t Target, b Backend, prefix string) (int, error) {
	l, err := m.remote(ctx, b, prefix)
	if err != nil {
		return 0, err
	}
	seen := map[string]int{}
	n := 0
	for _, r := range l { // newest first
		k := r.Kind + "/" + r.Guest
		seen[k]++
		if seen[k] <= t.Keep {
			continue
		}
		if err := b.Delete(ctx, r.meta); err != nil {
			return n, err
		}
		if err := b.Delete(ctx, r.data); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// syncTarget copies every local backup missing at t, then prunes.
func (m *Manager) syncTarget(ctx context.Context, t Target, only *item, p jobs.Progress) (string, error) {
	defer m.lockTarget(t.Name)()
	fail := func(err error) (string, error) {
		m.note(t.Name, func(s *Status) {
			s.LastError, s.ErrorAt = err.Error(), m.now().UTC().Format(time.RFC3339)
		})
		return "", err
	}
	b, prefix, err := m.backend(ctx, t)
	if err != nil {
		return fail(err)
	}
	objs, err := b.List(ctx, join(prefix, ""))
	if err != nil {
		return fail(fmt.Errorf("list: %w", err))
	}
	have := map[string]bool{}
	for _, o := range objs {
		have[o.Key] = true
	}
	local, err := m.local()
	if err != nil {
		return fail(err)
	}
	keys := make([]string, 0, len(local))
	for k := range local {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	copied, bytes := 0, int64(0)
	for _, k := range keys {
		kind := strings.SplitN(k, "/", 2)[0]
		if only != nil && k != only.kind+"/"+only.guest+"/"+only.id {
			continue
		}
		if !m.newestKept(local, kind, local[k], t.Keep) {
			continue // would be pruned right away
		}
		if p != nil {
			p("copying " + kind + " " + local[k].VM + " " + local[k].ID)
		}
		n, err := m.upload(ctx, t, b, prefix, kind, local[k], have)
		if err != nil {
			return fail(fmt.Errorf("copy %s %s: %w", local[k].VM, local[k].ID, err))
		}
		if n > 0 {
			copied++
			bytes += n
		}
	}
	removed, err := m.prune(ctx, t, b, prefix)
	if err != nil {
		return fail(fmt.Errorf("remove old copies: %w", err))
	}
	m.note(t.Name, func(s *Status) {
		s.LastOK, s.LastError, s.ErrorAt = m.now().UTC().Format(time.RFC3339), "", ""
		s.Copies += copied
		s.Bytes += bytes
	})
	msg := fmt.Sprintf("%s: %d backup(s) copied", t.Name, copied)
	if removed > 0 {
		msg += fmt.Sprintf(", %d old one(s) removed there", removed)
	}
	return msg, nil
}

// newestKept: is bm among the newest keep local backups of its guest?
func (m *Manager) newestKept(local map[string]backup.Meta, kind string, bm backup.Meta, keep int) bool {
	newer := 0
	for k, o := range local {
		if strings.HasPrefix(k, kind+"/") && o.VM == bm.VM && o.ID > bm.ID {
			newer++
		}
	}
	return newer < keep
}

func (m *Manager) allowed() bool { return m.Allowed == nil || m.Allowed() }

func (m *Manager) enabled() []Target {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.load() != nil {
		return nil
	}
	var out []Target
	for _, t := range m.targets {
		if t.Enabled {
			out = append(out, t)
		}
	}
	return out
}

// Enqueue asks for one new local backup to be copied to every enabled target.
func (m *Manager) Enqueue(kind, guest, id string) {
	m.qonce.Do(func() { m.queue = make(chan item, 256) })
	select {
	case m.queue <- item{kind, guest, id}:
	default: // full: the periodic sync picks it up
	}
}

// Run copies queued backups and, every few hours, anything missing (a target
// that was offline catches up).
func (m *Manager) Run(ctx context.Context, every time.Duration) {
	m.qonce.Do(func() { m.queue = make(chan item, 256) })
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case it := <-m.queue:
			if !m.allowed() {
				continue
			}
			for _, tg := range m.enabled() {
				if _, err := m.syncTarget(ctx, tg, &it, nil); err != nil && m.Log != nil {
					m.Log.Warn("off-site copy failed", "target", tg.Name, "backup", it.guest+" "+it.id, "err", err)
				}
			}
		case <-t.C:
			if !m.allowed() {
				continue
			}
			for _, tg := range m.enabled() {
				if _, err := m.syncTarget(ctx, tg, nil, nil); err != nil && m.Log != nil {
					m.Log.Warn("off-site sync failed", "target", tg.Name, "err", err)
				}
			}
		}
	}
}

// StartSync copies everything missing to one target now (a job).
func (m *Manager) StartSync(name string) (jobs.Job, error) {
	t, err := m.target(name)
	if err != nil {
		return jobs.Job{}, err
	}
	return m.Jobs.Start("offsite-sync", "offsite:"+name, func(ctx context.Context, p jobs.Progress) (string, error) {
		return m.syncTarget(ctx, t, nil, p)
	})
}

// RemoteBackups lists what is stored at a target.
func (m *Manager) RemoteBackups(ctx context.Context, name string) ([]Remote, error) {
	t, err := m.target(name)
	if err != nil {
		return nil, err
	}
	b, prefix, err := m.backend(ctx, t)
	if err != nil {
		return nil, err
	}
	l, err := m.remote(ctx, b, prefix)
	if err != nil {
		return nil, err
	}
	local, _ := m.local()
	for i := range l {
		_, l[i].Local = local[l[i].Kind+"/"+l[i].Guest+"/"+l[i].ID]
		var meta bytes.Buffer
		if err := b.Get(ctx, l[i].meta, &meta); err == nil {
			var bm backup.Meta
			if json.Unmarshal(meta.Bytes(), &bm) == nil {
				l[i].CreatedAt, l[i].Owner = bm.CreatedAt, bm.Owner
			}
		}
	}
	if l == nil {
		l = []Remote{}
	}
	return l, nil
}

// StartFetch copies a backup from a target back to this server (a job); it
// then shows with the local backups and is restored from there.
func (m *Manager) StartFetch(name, kind, guest, id string) (jobs.Job, error) {
	t, err := m.target(name)
	if err != nil {
		return jobs.Job{}, err
	}
	if (kind != "vm" && kind != "ct") || !reGuest.MatchString(guest) || !reID.MatchString(id) {
		return jobs.Job{}, fmt.Errorf("%w: backup", ErrInvalid)
	}
	return m.Jobs.Start("offsite-fetch", "offsite:"+name, func(ctx context.Context, p jobs.Progress) (string, error) {
		defer m.lockTarget(t.Name)()
		b, prefix, err := m.backend(ctx, t)
		if err != nil {
			return "", err
		}
		l, err := m.remote(ctx, b, prefix)
		if err != nil {
			return "", err
		}
		var r *Remote
		for i := range l {
			if l[i].Kind == kind && l[i].Guest == guest && l[i].ID == id {
				r = &l[i]
			}
		}
		if r == nil {
			return "", fmt.Errorf("%w: %s %s %s", ErrNotFound, kind, guest, id)
		}
		if r.Encrypted && t.Passphrase == "" {
			return "", errors.New("this copy is encrypted: set the target's passphrase first")
		}
		var meta bytes.Buffer
		if err := b.Get(ctx, r.meta, &meta); err != nil {
			return "", err
		}
		var bm backup.Meta
		if err := json.Unmarshal(meta.Bytes(), &bm); err != nil || bm.VM != guest || bm.ID != id {
			return "", errors.New("the copy's description doesn't match")
		}
		data, metaPath := m.store(kind).Files(bm)
		if _, err := os.Stat(metaPath); err == nil {
			return "", fmt.Errorf("backup %s of %s is already on this server", id, guest)
		}
		if err := os.MkdirAll(filepath.Dir(data), 0o700); err != nil {
			return "", err
		}
		tmp := data + ".partial"
		f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return "", err
		}
		p("downloading")
		if r.Encrypted {
			pr, pw := io.Pipe()
			go func() { pw.CloseWithError(b.Get(ctx, r.data, pw)) }()
			err = Decrypt(f, pr, t.Passphrase)
			pr.CloseWithError(err)
		} else {
			err = b.Get(ctx, r.data, f)
		}
		if err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(tmp)
			return "", err
		}
		if err := os.WriteFile(metaPath, meta.Bytes(), 0o600); err != nil {
			os.Remove(tmp)
			return "", err
		}
		if err := os.Rename(tmp, data); err != nil {
			os.Remove(tmp)
			os.Remove(metaPath)
			return "", err
		}
		what := "VM"
		if kind == "ct" {
			what = "container"
		}
		return fmt.Sprintf("backup %s of %s %s is back on this server — restore it from Backups", id, what, guest), nil
	})
}
