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
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"p8sos/internal/api"
	"p8sos/internal/backup"
	"p8sos/internal/cloudinit"
	"p8sos/internal/config"
	"p8sos/internal/execx"
	"p8sos/internal/hostinfo"
	"p8sos/internal/isos"
	"p8sos/internal/jobs"
	"p8sos/internal/metrics"
	"p8sos/internal/storage"
	"p8sos/internal/tlsutil"
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
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go svc.RunSchedule(ctx, cfg.BackupTime, nil)
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
			UI: web.Handler(),
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
