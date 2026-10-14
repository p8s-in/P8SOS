// Command p8s-witness is the tie-breaker for a P8S OS cluster of two servers
// (like Proxmox's QDevice). Run it on any always-on Linux box that is not one
// of the two servers — a Raspberry Pi, a NAS, a small VPS:
//
//	sudo ./p8s-witness install   copy itself to /usr/local/bin, start at boot, print the setup line
//	p8s-witness init             make the key and certificate; print the setup line
//	p8s-witness serve            run it (the service does this)
//
// Every command takes -dir (default /var/lib/p8s-witness) and serve/install
// take -addr (default :8007). On the cluster master, paste the printed line:
//
//	p8 cluster witness https://ADDRESS:8007 TOKEN FINGERPRINT
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"p8sos/internal/tlsutil"
	"p8sos/internal/witness"
)

const unit = `[Unit]
Description=P8S witness (tie-breaker for a two-server P8S OS cluster)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/p8s-witness serve -dir %s -addr %s
Restart=always
RestartSec=3
NoNewPrivileges=yes
PrivateTmp=yes
ProtectHome=yes
ProtectSystem=strict
ReadWritePaths=%s

[Install]
WantedBy=multi-user.target
`

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	dir := fs.String("dir", "/var/lib/p8s-witness", "where it keeps its key, certificate and votes")
	addr := fs.String("addr", ":8007", "address to listen on")
	_ = fs.Parse(os.Args[2:])
	var err error
	switch cmd {
	case "init":
		err = initDir(*dir, *addr)
	case "serve":
		err = serve(*dir, *addr)
	case "install":
		err = install(*dir, *addr)
	case "version", "-v", "--version":
		fmt.Println("p8s-witness 1")
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "p8s-witness:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: p8s-witness install | init | serve  [-dir /var/lib/p8s-witness] [-addr :8007]")
	os.Exit(2)
}

// prepare makes (or reads) the key and certificate.
func prepare(dir string) (token, cert, key, pin string, err error) {
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	tf := filepath.Join(dir, "token")
	if b, e := os.ReadFile(tf); e == nil && len(strings.TrimSpace(string(b))) >= 16 {
		token = strings.TrimSpace(string(b))
	} else {
		r := make([]byte, 24)
		_, _ = rand.Read(r)
		token = hex.EncodeToString(r)
		if err = os.WriteFile(tf, []byte(token+"\n"), 0o600); err != nil {
			return
		}
	}
	if cert, key, err = tlsutil.Ensure(dir); err != nil {
		return
	}
	b, err := os.ReadFile(cert)
	if err != nil {
		return
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		err = errors.New(cert + " isn't a certificate")
		return
	}
	pin = witness.Fingerprint(blk.Bytes)
	return
}

func initDir(dir, addr string) error {
	token, _, _, pin, err := prepare(dir)
	if err != nil {
		return err
	}
	_, port, _ := net.SplitHostPort(addr)
	if port == "" {
		port = "8007"
	}
	ips := addresses()
	if len(ips) == 0 {
		ips = []string{"THIS-BOX-ADDRESS"}
	}
	fmt.Println("Witness ready. On the cluster master run:")
	fmt.Println()
	fmt.Printf("  p8 cluster witness https://%s:%s %s %s\n", ips[0], port, token, pin)
	if len(ips) > 1 {
		fmt.Printf("\n(use whichever address both servers reach: %s)\n", strings.Join(ips, ", "))
	}
	fmt.Println("\nOr in the dashboard: Cluster → Witness, with these three values.")
	return nil
}

func addresses() []string {
	var out []string
	l, _ := net.InterfaceAddrs()
	for _, a := range l {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && n.IP.To4() != nil {
			out = append(out, n.IP.String())
		}
	}
	return out
}

func serve(dir, addr string) error {
	token, cert, key, pin, err := prepare(dir)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ws := &witness.Server{File: filepath.Join(dir, "clusters.json"), Token: token}
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: addr, Handler: ws.Handler(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Info("p8s witness listening", "addr", addr, "fingerprint", pin)
	if err := srv.ListenAndServeTLS("", ""); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func install(dir, addr string) error {
	if os.Geteuid() != 0 {
		return errors.New("run it with sudo")
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	b, err := os.ReadFile(self)
	if err != nil {
		return err
	}
	if self != "/usr/local/bin/p8s-witness" {
		tmp := "/usr/local/bin/p8s-witness.new"
		if err := os.WriteFile(tmp, b, 0o755); err != nil {
			return err
		}
		if err := os.Rename(tmp, "/usr/local/bin/p8s-witness"); err != nil {
			return err
		}
	}
	if _, _, _, _, err := prepare(dir); err != nil {
		return err
	}
	if err := os.WriteFile("/etc/systemd/system/p8s-witness.service", []byte(fmt.Sprintf(unit, dir, addr, dir)), 0o644); err != nil {
		return err
	}
	for _, c := range [][]string{{"daemon-reload"}, {"enable", "--now", "p8s-witness"}, {"restart", "p8s-witness"}} {
		if out, err := exec.Command("systemctl", c...).CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl %s: %v: %s", strings.Join(c, " "), err, out)
		}
	}
	fmt.Println("✔ p8s-witness runs now and at every boot (systemctl status p8s-witness).")
	fmt.Println("  Open TCP port", strings.TrimPrefix(addr, ":"), "to the two servers if this box has a firewall.")
	fmt.Println()
	return initDir(dir, addr)
}
