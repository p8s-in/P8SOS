package offsite

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// sshBackend keeps backups in a folder on another Linux server (another P8S
// server, a NAS, a cheap VPS), reached with this server's own SSH key.
type sshBackend struct {
	host, user, dir string
	port            int
	keyFile         string // private key (its .pub goes into the remote authorized_keys)
	knownHosts      string // remembered host keys (first connect is trusted)
	cmd             func(ctx context.Context, name string, args ...string) *exec.Cmd
}

var (
	reHost   = regexp.MustCompile(`^[A-Za-z0-9.\-:\[\]]{1,253}$`)
	reUser   = regexp.MustCompile(`^[a-z_][a-z0-9_.-]{0,31}$`)
	reRemote = regexp.MustCompile(`^/[A-Za-z0-9._/-]{1,200}$`)
)

func (s *sshBackend) run(ctx context.Context, stdin io.Reader, stdout io.Writer, script string, args ...string) error {
	// arguments are passed as quoted words, never pasted into the script
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	full := "set -e; set -- " + strings.Join(q, " ") + "; " + script
	argv := []string{"-F", "/dev/null", "-i", s.keyFile, "-o", "IdentitiesOnly=yes", "-p", strconv.Itoa(s.port),
		"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new", "-o", "UserKnownHostsFile=" + s.knownHosts,
		"-o", "ConnectTimeout=15", "-o", "ServerAliveInterval=30",
		s.user + "@" + s.host, "sh -c " + "'" + strings.ReplaceAll(full, "'", `'\''`) + "'"}
	c := s.cmd(ctx, "ssh", argv...)
	var errb bytes.Buffer
	c.Stdin, c.Stdout, c.Stderr = stdin, stdout, &errb
	if err := c.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if strings.Contains(msg, "No such file") {
			return fmt.Errorf("%w: %s", ErrNotFound, msg)
		}
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("ssh %s@%s: %s", s.user, s.host, msg)
	}
	return nil
}

func (s *sshBackend) Put(ctx context.Context, key string, r io.Reader) error {
	if err := safeKey(key); err != nil {
		return err
	}
	p := s.dir + "/" + key
	return s.run(ctx, r, io.Discard, `mkdir -p "$(dirname "$1")" && cat > "$1.partial" && mv "$1.partial" "$1"`, p)
}

func (s *sshBackend) Get(ctx context.Context, key string, w io.Writer) error {
	if err := safeKey(key); err != nil {
		return err
	}
	return s.run(ctx, nil, w, `cat "$1"`, s.dir+"/"+key)
}

func (s *sshBackend) List(ctx context.Context, prefix string) ([]Object, error) {
	var out bytes.Buffer
	if err := s.run(ctx, nil, &out, `mkdir -p "$1" && cd "$1" && find . -type f ! -name '*.partial' -exec stat -c '%s %n' {} +`, s.dir); err != nil {
		return nil, err
	}
	l := []Object{}
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		size, name, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			continue
		}
		key := strings.TrimPrefix(name, "./")
		n, err := strconv.ParseInt(size, 10, 64)
		if err != nil || !strings.HasPrefix(key, prefix) {
			continue
		}
		l = append(l, Object{Key: key, Size: n})
	}
	return l, nil
}

func (s *sshBackend) Delete(ctx context.Context, key string) error {
	if err := safeKey(key); err != nil {
		return err
	}
	return s.run(ctx, nil, io.Discard, `rm -f "$1"; [ "$(dirname "$1")" = "$2" ] || rmdir "$(dirname "$1")" 2>/dev/null || true`, s.dir+"/"+key, s.dir)
}
