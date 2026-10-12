// Command p8sd is the P8S OS management daemon.
//
//	p8sd serve [-config /etc/p8s/p8sd.json]   run the API server + web dashboard
//	p8sd token [-name admin]                  generate an API token
//	p8sd version
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"p8sos/internal/api"
	"p8sos/internal/audit"
	"p8sos/internal/backup"
	"p8sos/internal/cloudinit"
	"p8sos/internal/config"
	"p8sos/internal/execx"
	"p8sos/internal/firewall"
	"p8sos/internal/guard"
	"p8sos/internal/hostinfo"
	"p8sos/internal/isos"
	"p8sos/internal/jobs"
	"p8sos/internal/metrics"
	"p8sos/internal/storage"
	"p8sos/internal/tlsutil"
	"p8sos/internal/tunnel"
	"p8sos/internal/users"
	"p8sos/internal/virt"
	"p8sos/internal/vm"
	"p8sos/internal/web"
)

var version = "0.1.0-dev" // overridden by -ldflags "-X main.version=..."

func main() {
	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "token":
		err = token(args)
	case "version":
		fmt.Println(version)
	default:
		err = fmt.Errorf("unknown command %q (serve, token, version)", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "p8sd:", err)
		os.Exit(1)
	}
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	path := fs.String("config", "/etc/p8s/p8sd.json", "config file")
	_ = fs.Parse(args)

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}

	run := execx.System{}
	disks := &storage.ZFS{Run: run, Dataset: cfg.VMDataset}
	svc := &vm.Service{
		HV:           &virt.Virsh{Run: run, URI: cfg.LibvirtURI},
		Disks:        disks,
		Seeds:        &cloudinit.Builder{Run: run, Dir: filepath.Join(cfg.DataDir, "seeds")},
		Templates:    cfg.Templates,
		Bridge:       cfg.Bridge,
		LANBridge:    cfg.LANBridge,
		BridgeUp:     func(n string) bool { _, err := os.Stat("/sys/class/net/" + n + "/bridge"); return err == nil },
		Limits:       vm.Limits{MaxVCPUs: cfg.MaxVCPUs, MaxMemoryMiB: cfg.MaxMemoryMiB, MaxDiskGB: cfg.MaxDiskGB},
		SSHKeys:      cfg.SSHKeys,
		Log:          log,
		Backups:      &backup.Store{Dir: cfg.BackupDir, Dataset: cfg.VMDataset, Run: run},
		Jobs:         &jobs.Manager{Log: log},
		ISOs:         &isos.Library{Dir: cfg.ISODir},
		ScheduleFile: filepath.Join(cfg.DataDir, "backup-schedule.json"),
		FirewallFile: filepath.Join(cfg.DataDir, "firewall.json"),
		Firewall: func(ctx context.Context, vms []firewall.VM) error {
			return firewall.Apply(ctx, run, vms)
		},
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if need, err := svc.UpgradeDomains(ctx); err != nil {
		log.Warn("upgrade VM definitions", "err", err)
	} else if len(need) > 0 {
		log.Info("VM definitions upgraded (VGA display); power them off and start them to apply", "vms", strings.Join(need, ","))
	}
	go svc.RunSchedule(ctx, cfg.BackupTime, nil)
	go runGuard(ctx, log, run, cfg)
	go runFirewalls(ctx, log, svc)
	virsh := &virt.Virsh{Run: run, URI: cfg.LibvirtURI}
	mc := &metrics.Collector{
		Stats: func(ctx context.Context) (map[string]metrics.Raw, error) {
			out, err := virsh.DomStats(ctx)
			if err != nil {
				return nil, err
			}
			res := map[string]metrics.Raw{}
			for dom, r := range metrics.ParseDomStats(out) {
				if name, ok := strings.CutPrefix(dom, vm.Prefix); ok {
					res[name] = r
				}
			}
			return res, nil
		},
		HostCPU: metrics.ProcStat,
		HostMem: metrics.ProcMem,
	}
	go mc.Run(ctx)
	for _, t := range cfg.Templates {
		if !disks.SnapshotExists(ctx, t.Snapshot) {
			log.Warn("template snapshot missing; creates from it will fail", "template", t.Name, "snapshot", t.Snapshot)
		}
	}

	tun := &tunnel.Manager{
		Dir:       cfg.DataDir,
		Dashboard: localURL(cfg),
		IPs: func(ctx context.Context) (map[string]string, error) {
			vms, err := svc.List(ctx)
			ips := map[string]string{}
			for _, v := range vms {
				ips[v.Name] = v.IP
			}
			return ips, err
		},
	}
	go func() { // follow VM address changes (DHCP) and re-push routes
		last := ""
		for {
			msg := ""
			if err := tun.Sync(ctx); err != nil {
				msg = err.Error()
			}
			if msg != last && msg != "" {
				log.Error("public access (Cloudflare Tunnel) routes", "err", msg)
			}
			last = msg
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Minute):
			}
		}
	}()
	sessions := &users.Sessions{Path: filepath.Join(cfg.DataDir, "sessions.json")}
	go func() { // keep last-use times on disk, so idle sign-outs survive restarts
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				sessions.Flush()
				return
			case <-t.C:
				sessions.Flush()
			}
		}
	}()

	srv := &http.Server{
		Addr: cfg.Listen,
		Handler: (&api.Server{
			VMs: svc, Templates: cfg.Templates, Tokens: cfg.Tokens, Version: version, Log: log,
			BackupKeep: cfg.BackupKeep, BackupTime: cfg.BackupTime, Metrics: mc,
			Host: func(ctx context.Context) any {
				in := hostinfo.Collect(ctx, run, cfg.VMDataset)
				in.Networks = svc.Networks()
				return in
			},
			UI:       web.Handler(),
			Users:    &users.Store{Path: filepath.Join(cfg.DataDir, "users.json")},
			Sessions: sessions,
			Tunnel:   tun,
			Audit:    &audit.Log{Path: filepath.Join(cfg.DataDir, "audit.log")},
			Issuer:   issuer(),
		}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// VM create can take a while (clone + seed + define + start).
		WriteTimeout: 5 * time.Minute,
	}
	if cfg.TLSCert == "" && cfg.TLSAuto {
		if cfg.TLSCert, cfg.TLSKey, err = tlsutil.Ensure(filepath.Join(cfg.DataDir, "tls")); err != nil {
			return fmt.Errorf("self-signed TLS: %w", err)
		}
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("p8sd listening", "addr", cfg.Listen, "version", version, "tls", cfg.TLSCert != "")
		if cfg.TLSCert != "" {
			errc <- srv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
		} else {
			errc <- srv.ListenAndServe()
		}
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// token prints a new random API token and the config entry holding its hash.
func token(args []string) error {
	fs := flag.NewFlagSet("token", flag.ExitOnError)
	name := fs.String("name", "admin", "token name")
	asJSON := fs.Bool("json", false, "print {\"name\",\"token\",\"sha256\"} as JSON (for scripts)")
	_ = fs.Parse(args)

	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	secret := "p8s_" + hex.EncodeToString(b)
	sum := sha256.Sum256([]byte(secret))
	hash := hex.EncodeToString(sum[:])
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"name": *name, "token": secret, "sha256": hash})
	}
	entry, _ := json.Marshal(config.Token{Name: *name, SHA256: hash})
	fmt.Printf("token (shown once, store it safely):\n  %s\n\nadd to \"tokens\" in p8sd.json:\n  %s\n", secret, entry)
	return nil
}

// runGuard keeps the VM isolation firewall in place (re-applied every 5 minutes
// in case something flushed it, e.g. a libvirt or firewall restart).
func runGuard(ctx context.Context, log *slog.Logger, run execx.Runner, cfg *config.Config) {
	if !cfg.GuardOn() {
		guard.Remove(ctx, run)
		log.Warn("vm_guard is off: VMs can reach this server and the home network")
		return
	}
	gc := guard.Config{NATBridge: cfg.Bridge, LANBridge: cfg.LANBridge, Allow: cfg.VMAllow}
	lastErr := ""
	for {
		err := guard.Apply(ctx, run, gc)
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		if msg != lastErr {
			if err != nil {
				log.Error("vm guard", "err", err)
			} else {
				log.Info("vm guard active: VMs can't reach this server or the home network", "nat", gc.NATBridge, "lan", gc.LANBridge)
			}
			lastErr = msg
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Minute):
		}
	}
}

// issuer names this host in authenticator apps, e.g. "P8S OS (p8s-node)".
func issuer() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return "P8S OS (" + h + ")"
	}
	return "P8S OS"
}

// runFirewalls keeps the per-VM firewalls loaded (start-up, then every 5
// minutes, in case something flushed nftables).
func runFirewalls(ctx context.Context, log *slog.Logger, svc *vm.Service) {
	lastErr := ""
	for {
		msg := ""
		if err := svc.ApplyFirewalls(ctx); err != nil {
			msg = err.Error()
		}
		if msg != lastErr {
			if msg != "" {
				log.Error("VM firewalls could not be loaded", "err", msg)
			} else if lastErr != "" {
				log.Info("VM firewalls loaded again")
			}
			lastErr = msg
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Minute):
		}
	}
}

// localURL is where cloudflared on this host reaches the dashboard.
func localURL(cfg *config.Config) string {
	host, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		host, port = "", "8006"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	scheme := "http"
	if cfg.TLSAuto || cfg.TLSCert != "" {
		scheme = "https"
	}
	return scheme + "://" + net.JoinHostPort(host, port)
}
