// Command p8sd is the P8S OS management daemon.
//
//	p8sd serve [-config /etc/p8s/p8sd.json]   run the API server + web dashboard
//	p8sd token [-name admin]                  generate an API token
//	p8sd version
//	p8sd server-id                            this server's ID (for buying a subscription)
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
	"os/exec"
	"os/signal"
	"p8sos/internal/creds"
	"p8sos/internal/monitor"
	"p8sos/internal/netx"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"p8sos/internal/api"
	"p8sos/internal/audit"
	"p8sos/internal/backup"
	"p8sos/internal/billing"
	"p8sos/internal/cloud"
	"p8sos/internal/cloudinit"
	"p8sos/internal/cluster"
	"p8sos/internal/config"
	"p8sos/internal/ct"
	"p8sos/internal/execx"
	"p8sos/internal/firewall"
	"p8sos/internal/guard"
	"p8sos/internal/hostinfo"
	"p8sos/internal/ippool"
	"p8sos/internal/isos"
	"p8sos/internal/jobs"
	"p8sos/internal/ldap"
	"p8sos/internal/license"
	"p8sos/internal/mail"
	"p8sos/internal/metrics"
	"p8sos/internal/offsite"
	"p8sos/internal/portfwd"
	"p8sos/internal/secpol"
	"p8sos/internal/sso"
	"p8sos/internal/storage"
	"p8sos/internal/support"
	"p8sos/internal/tasks"
	"p8sos/internal/tlsutil"
	"p8sos/internal/tunnel"
	"p8sos/internal/updates"
	"p8sos/internal/users"
	"p8sos/internal/virt"
	"p8sos/internal/vlan"
	"p8sos/internal/vm"
	"p8sos/internal/vpn"
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
	case "server-id": // for buying a licence
		dir := "/var/lib/p8s"
		if c, err := config.Load("/etc/p8s/p8sd.json"); err == nil {
			dir = c.DataDir
		}
		fmt.Println((&license.Manager{Path: filepath.Join(dir, "license.json")}).ID())
	default:
		err = fmt.Errorf("unknown command %q (serve, token, version, server-id)", cmd)
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
	pools := &ippool.Store{Path: filepath.Join(cfg.DataDir, "ippools.json")}
	svc := &vm.Service{
		HV:           &virt.Virsh{Run: run, URI: cfg.LibvirtURI},
		Disks:        disks,
		Seeds:        &cloudinit.Builder{Run: run, Dir: filepath.Join(cfg.DataDir, "seeds")},
		Templates:    cfg.Templates,
		Bridge:       cfg.Bridge,
		LANBridge:    cfg.LANBridge,
		BridgeUp:     func(n string) bool { _, err := os.Stat("/sys/class/net/" + n + "/bridge"); return err == nil },
		TPMReady:     func() bool { _, err := exec.LookPath("swtpm"); return err == nil },
		Limits:       vm.Limits{MaxVCPUs: cfg.MaxVCPUs, MaxMemoryMiB: cfg.MaxMemoryMiB, MaxDiskGB: cfg.MaxDiskGB},
		SSHKeys:      cfg.SSHKeys,
		Log:          log,
		Backups:      &backup.Store{Dir: cfg.BackupDir, Dataset: cfg.VMDataset, Run: run},
		Jobs:         &jobs.Manager{Log: log},
		ISOs:         &isos.Library{Dir: cfg.ISODir},
		ImportDir:    filepath.Join(cfg.DataDir, "import"),   // Proxmox backups, disk images to import
		ReplicaDir:   filepath.Join(cfg.DataDir, "replicas"), // copies of other servers' VMs (high availability)
		Pools:        pools,
		ScheduleFile: filepath.Join(cfg.DataDir, "backup-schedule.json"),
		FirewallFile: filepath.Join(cfg.DataDir, "firewall.json"),
		Firewall: func(ctx context.Context, vms []firewall.VM) error {
			return firewall.Apply(ctx, run, vms)
		},
	}
	if cfg.LANBridge != "" { // tagged VLANs on the home / datacenter network
		svc.VLANs = &vlan.Manager{Run: run, LANBridge: cfg.LANBridge, ListFile: filepath.Join(cfg.DataDir, "vlans.list")}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if need, err := svc.UpgradeDomains(ctx); err != nil {
		log.Warn("upgrade VM definitions", "err", err)
	} else if len(need) > 0 {
		log.Info("VM definitions upgraded (display, faster disk and network); power them off and start them to apply", "vms", strings.Join(need, ","))
	}
	go runGuard(ctx, log, run, cfg)
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
		Connector: &tunnel.Connector{},
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
	tun.Start() // cloudflared, when public access is on
	defer tun.Connector.Stop()
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
	cloudStore := &cloud.Store{Path: filepath.Join(cfg.DataDir, "cloud.json")}
	mailer := &mail.Mailer{Dir: cfg.DataDir, Config: func() mail.Config {
		st := cloudStore.Settings()
		return mail.Config{Host: st.SMTPHost, Port: st.SMTPPort, User: st.SMTPUser, Pass: st.SMTPPass, From: st.MailFrom}
	}}
	lic := &license.Manager{Path: filepath.Join(cfg.DataDir, "license.json")}
	if cfg.LicenseServer != "" {
		lic.ShopURL = strings.TrimRight(cfg.LicenseServer, "/") + "/"
		lic.CheckURL = lic.ShopURL + "api/v1/check"
	}
	if st := lic.Status(); st.Notice {
		log.Info("P8S OS subscription", "server_id", st.ServerID, "tier", st.Tier, "state", st.State)
	}
	go lic.Run(ctx, func(err error) { log.Warn("licence check", "err", err) })
	upd := &updates.Manager{Dir: filepath.Join(cfg.DataDir, "updates"), Server: cfg.LicenseServer, Current: version, Run: run,
		Info: func() updates.Channel {
			st := lic.Status()
			c := updates.Channel{Allowed: []string{"testing"}, Server: st.ServerID}
			if st.Channel == "stable" {
				c.Allowed = []string{"stable", "testing"}
			}
			if st.License != nil {
				c.License = st.License.ID
			}
			return c
		}}
	go upd.Loop(ctx, func(err error) { log.Warn("update check", "err", err) })
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

	var cts *ct.Service
	if _, err := exec.LookPath("lxc-start"); err == nil {
		cts = &ct.Service{Run: run, Pool: strings.SplitN(cfg.VMDataset, "/", 2)[0], MountDir: filepath.Join(cfg.DataDir, "ct"),
			LXCDir: "/var/lib/lxc", MetaFile: filepath.Join(cfg.DataDir, "containers.json"), Bridge: cfg.Bridge,
			LANBridge: cfg.LANBridge, LANUp: svc.LANAvailable, SSHKeys: cfg.SSHKeys,
			Limits:  ct.Limits{MaxVCPUs: cfg.MaxVCPUs, MaxMemoryMiB: cfg.MaxMemoryMiB, MaxDiskGB: cfg.MaxDiskGB},
			Backups: svc.Backups, Jobs: svc.Jobs, Pools: pools, BridgeUp: svc.BridgeUp, Refresh: svc.ApplyFirewalls}
		go func() { // after the server boots: start containers marked autostart (not on every p8sd restart)
			if !newBoot(filepath.Join(cfg.DataDir, "ct-boot-id")) {
				return
			}
			cts.Autostart(ctx, func(name string, err error) { log.Error("container autostart", "container", name, "err", err) })
		}()
	} else {
		log.Warn("containers are off: lxc is not installed (sudo bash update.sh installs it)")
	}
	if cts != nil { // containers share the VM firewall table, keyed "ct:NAME"
		svc.OtherGuests = func(ctx context.Context) ([]firewall.VM, error) {
			list, err := cts.List(ctx)
			if err != nil {
				return nil, err
			}
			out := make([]firewall.VM, 0, len(list))
			for _, c := range list {
				e := firewall.VM{Name: "ct:" + c.Name, MAC: c.MAC}
				if c.PublicIP != "" { // pin the address to its veth (fixed name, also before it starts)
					e.LockIP, e.Ports = c.PublicIP, []string{ct.HostPort(c.Name)}
				}
				out = append(out, e)
			}
			return out, nil
		}
		svc.OtherBackup = func(ctx context.Context, key string, keep int) error {
			name, ok := strings.CutPrefix(key, "ct:")
			if !ok {
				return nil
			}
			_, err := cts.StartBackup(ctx, name, keep)
			return err
		}
	}
	go runFirewalls(ctx, log, svc)

	// off-site copies of backups (folder, S3-compatible storage, another server over SSH)
	off := &offsite.Manager{File: filepath.Join(cfg.DataDir, "offsite.json"), BackupDir: cfg.BackupDir,
		KeyFile: filepath.Join(cfg.DataDir, "offsite", "id_ed25519"), Log: log, Jobs: svc.Jobs,
		HTTP:    &http.Client{Transport: offsiteTransport()},
		Allowed: func() bool { return lic.Allows(license.FeatOffsite) }}
	svc.AfterBackup = off.Enqueue
	if cts != nil {
		cts.AfterBackup = off.Enqueue
	}
	go off.Run(ctx, 6*time.Hour)
	go svc.RunSchedule(ctx, cfg.BackupTime, nil)

	// Health & alerts: pools, disks (SMART), space, RAM, temperature, crashed
	// VMs, failed backups, power (UPS / laptop battery) with clean shutdown.
	mon := &monitor.Monitor{Dir: cfg.DataDir, Run: run, Log: log, HTTP: netx.Client(15 * time.Second),
		Mail:       func(to, subject, body string) { mailer.Queue(to, subject, body, "alert") },
		Paths:      watchPaths("/", cfg.DataDir, cfg.BackupDir),
		MemUsedPct: func() int { m := hostinfo.ReadMem(); return m.UsedMiB * 100 / max(m.TotalMiB, 1) },
		Guests: func(ctx context.Context) []monitor.Guest {
			l, err := svc.List(ctx)
			if err != nil {
				return nil
			}
			var g []monitor.Guest
			for _, v := range l {
				g = append(g, monitor.Guest{Name: v.Name, State: string(v.State)})
			}
			return g
		},
		FailedJobs: func(since time.Time) []string {
			var out []string
			for _, j := range svc.ListJobs() {
				if t, err := time.Parse(time.RFC3339, j.Finished); err == nil && t.After(since) && j.State == "failed" {
					out = append(out, j.Kind+" of "+j.Target+": "+j.Error)
				}
			}
			return out
		},
		StopGuests: func(ctx context.Context) error { return stopAllGuests(ctx, svc, cts) },
		Poweroff: func() error {
			_, err := run.Run(context.Background(), "systemctl", "poweroff")
			return err
		},
	}
	go mon.Loop(ctx)

	apiSrv := &api.Server{
		VMs: svc, Templates: cfg.Templates, Tokens: cfg.Tokens, Version: version, Log: log,
		BackupKeep: cfg.BackupKeep, BackupTime: cfg.BackupTime, Metrics: mc,
		Host: func(ctx context.Context) any {
			in := hostinfo.Collect(ctx, run, cfg.VMDataset)
			in.Networks = svc.Networks()
			in.VLANs = svc.VLANs != nil && svc.LANAvailable()
			return in
		},
		UI:       web.Handler(),
		Users:    &users.Store{Path: filepath.Join(cfg.DataDir, "users.json")},
		Sessions: sessions,
		Tunnel:   tun,
		Pools:    pools,
		Cloud:    cloudStore,
		Billing:  &billing.Store{Dir: filepath.Join(cfg.DataDir, "billing")},
		Mail:     mailer,
		Support:  &support.Store{Path: filepath.Join(cfg.DataDir, "tickets.json")},
		Audit:    &audit.Log{Path: filepath.Join(cfg.DataDir, "audit.log"), RetainDays: cfg.AuditRetainDays},
		Issuer:   issuer(),
		License:  lic,
		Updates:  upd,
		CTs:      cts,
		Offsite:  off,
		Monitor:  mon,
		// root passwords made for new VMs/containers, shown to their owners
		Creds:         &creds.Store{File: filepath.Join(cfg.DataDir, "credentials.json")},
		AutoPasswords: cfg.AutoRootPasswordsOn(),
		Imports:       svc,
		ImportFolder:  filepath.Join(cfg.DataDir, "import"),
		Node: &tasks.Node{
			Tasks: &tasks.Manager{Dir: filepath.Join(cfg.DataDir, "tasks"), Scripts: scriptsDir(), Run: run},
			Run:   run, Pool: strings.SplitN(cfg.VMDataset, "/", 2)[0], DataDir: cfg.DataDir, LANBridge: cfg.LANBridge,
			Templates: func() []string {
				var l []string
				for _, t := range cfg.Templates {
					l = append(l, t.Name)
				}
				return l
			},
			LAN: svc.LANAvailable,
		},
	}
	if cts != nil {
		apiSrv.Node.CTTemplates = func() []string {
			var l []string
			for _, t := range cts.Templates(context.Background()) {
				l = append(l, t.Name)
			}
			return l
		}
	}
	if cfg.SSHPortsOn() { // server:2201.. -> VM/container:22 for guests on the private network
		base := cfg.SSHPortBase
		if base <= 0 {
			base = 2201
		}
		pf := &portfwd.Manager{File: filepath.Join(cfg.DataDir, "ssh-ports.json"), Base: base, Log: log,
			Guests: func(ctx context.Context) ([]portfwd.Guest, error) {
				var gs []portfwd.Guest
				vms, err := svc.List(ctx)
				if err != nil {
					return nil, err
				}
				for _, v := range vms {
					gs = append(gs, portfwd.Guest{Name: v.Name, IP: v.IP, NAT: v.Network == "nat"})
				}
				if cts != nil {
					l, err := cts.List(ctx)
					if err != nil {
						return nil, err
					}
					for _, c := range l {
						gs = append(gs, portfwd.Guest{Name: c.Name, IP: c.IP, NAT: c.Network == "nat"})
					}
				}
				return gs, nil
			}}
		apiSrv.SSHPorts = pf.Ports
		go pf.Run(ctx)
		// Remote Desktop for Windows VMs on the private network: server:3391.. -> VM:3389
		// (much smoother than the browser console for daily use)
		rdp := &portfwd.Manager{File: filepath.Join(cfg.DataDir, "rdp-ports.json"), Base: 3391, Target: 3389, Log: log,
			Guests: func(ctx context.Context) ([]portfwd.Guest, error) {
				vms, err := svc.List(ctx)
				if err != nil {
					return nil, err
				}
				var gs []portfwd.Guest
				for _, v := range vms {
					if v.OSType == "windows" || v.OSType == "windows11" {
						gs = append(gs, portfwd.Guest{Name: v.Name, IP: v.IP, NAT: v.Network == "nat"})
					}
				}
				return gs, nil
			}}
		apiSrv.RDPPorts = rdp.Ports
		go rdp.Run(ctx)
	}
	go func() { // P8S Cloud billing: hourly charges, low/negative balance, suspension
		for {
			if err := apiSrv.BillingTick(ctx); err != nil {
				log.Error("billing", "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Minute):
			}
		}
	}()
	// Cluster: this server as master (with workers) or as a worker of another.
	_, apiPort, _ := net.SplitHostPort(cfg.Listen)
	clu := &cluster.Manager{Dir: filepath.Join(cfg.DataDir, "cluster"), APIPort: apiPort, Run: run, Log: log,
		LocalCapacity: func(ctx context.Context) cluster.Capacity {
			in := hostinfo.Collect(ctx, run, cfg.VMDataset)
			c := cluster.Capacity{CPUs: in.CPUs, MemTotalMiB: in.MemTotalMiB, PoolFreeGB: in.PoolFreeGB, Networks: svc.Networks(), Version: version}
			if l, err := svc.List(ctx); err == nil {
				for _, v := range l {
					c.MemUsedMiB += v.MemoryMiB
					c.VMs++
				}
			}
			if cts != nil {
				if l, err := cts.List(ctx); err == nil {
					for _, x := range l {
						c.MemUsedMiB += x.MemoryMiB
					}
				}
			}
			return c
		}}
	if err := clu.Apply(ctx); err != nil {
		log.Error("cluster tunnel", "err", err)
	}
	cvms := &cluster.VMs{Local: svc, M: clu, Jobs: svc.Jobs}
	apiSrv.VMs, apiSrv.Cluster, apiSrv.ClusterVMs = cvms, clu, cvms
	apiSrv.Migrate, apiSrv.LocalMoves = cvms, svc // moving VMs between servers
	apiSrv.Dial, apiSrv.RemoteMetrics = cvms.Dial, cvms.RemoteMetrics
	go clu.Loop(ctx, 15*time.Second)
	ha := &cluster.HA{C: cvms, Local: svc, Dir: filepath.Join(cfg.DataDir, "cluster"), Log: log,
		Notify: func(sev, title, detail string) { mon.Notice("ha", sev, title, detail) }}
	apiSrv.HA, apiSrv.LocalHA = ha, svc                                              // high availability
	apiSrv.SSO = &sso.Manager{Dir: cfg.DataDir, HTTP: netx.Client(15 * time.Second)} // company login
	apiSrv.LDAP = &ldap.Manager{Dir: cfg.DataDir}                                    // company directory (AD / LDAP)
	apiSrv.Security = &secpol.Store{Dir: cfg.DataDir}                                // allowed networks, password policy, staff 2FA
	apiSrv.Security.Load()
	apiSrv.Encryption = disks.Encryption // encryption at rest (Server page → Storage)
	remoteVPN := &vpn.Manager{Dir: filepath.Join(cfg.DataDir, "vpn"), Run: run}
	if err := remoteVPN.Apply(ctx); err != nil {
		log.Error("remote-access vpn", "err", err)
	}
	apiSrv.Remote = &api.Remote{VPN: remoteVPN, Run: run, PublicIP: cluster.PublicIP, FindRouter: vpn.FindRouter}
	go ha.Loop(ctx, 15*time.Second)
	if cfg.HostShellOn() {
		apiSrv.HostShell = ct.HostShell // Server → Shell (admins), like Proxmox
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           apiSrv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// VM create can take a while (clone + seed + define + start).
		WriteTimeout: 5 * time.Minute,
	}
	if cfg.TLSCert == "" && cfg.TLSAuto {
		if cfg.TLSCert, cfg.TLSKey, err = tlsutil.Ensure(filepath.Join(cfg.DataDir, "tls")); err != nil {
			return fmt.Errorf("self-signed TLS: %w", err)
		}
	}
	clu.CertFile = cfg.TLSCert // pinned by the other cluster servers
	go func() {                // answer "which P8S OS servers are on this network?" (UDP 51821)
		if err := clu.Respond(ctx, version); err != nil {
			log.Warn("cluster discovery off", "err", err)
		}
	}()
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

// scriptsDir is where install.sh puts the host scripts (P8S_SCRIPTS overrides, for tests).
func scriptsDir() string {
	if d := os.Getenv("P8S_SCRIPTS"); d != "" {
		return d
	}
	return "/usr/local/share/p8s"
}

// newBoot reports whether the server booted since the last call (boot_id changed).
func newBoot(stamp string) bool {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return false
	}
	id := strings.TrimSpace(string(b))
	if old, _ := os.ReadFile(stamp); strings.TrimSpace(string(old)) == id {
		return false
	}
	_ = os.WriteFile(stamp, []byte(id+"\n"), 0o600)
	return true
}

// offsiteTransport: netx dialing (system resolver fallback) for S3 targets.
func offsiteTransport() *http.Transport {
	t := netx.Transport()
	t.ResponseHeaderTimeout = 2 * time.Minute
	return t
}

// watchPaths: the filesystems to watch for free space (each once).
func watchPaths(paths ...string) []string {
	var out []string
	seen := map[uint64]bool{}
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if ok && seen[st.Dev] {
			continue
		}
		if ok {
			seen[st.Dev] = true
		}
		out = append(out, p)
	}
	return out
}

// stopAllGuests shuts every running VM and container down cleanly (power
// failing / overheating), waiting up to the context's deadline, then forces
// what is still running.
func stopAllGuests(ctx context.Context, svc *vm.Service, cts *ct.Service) error {
	vms, _ := svc.List(ctx)
	for _, v := range vms {
		if v.State == "running" {
			_, _ = svc.Stop(ctx, v.Name, false)
		}
	}
	if cts != nil {
		if l, err := cts.List(ctx); err == nil {
			for _, c := range l {
				if c.State == "running" {
					_, _ = cts.Stop(ctx, c.Name, false)
				}
			}
		}
	}
	for {
		running := 0
		if l, err := svc.List(ctx); err == nil {
			for _, v := range l {
				if v.State == "running" || v.State == "stopping" {
					running++
				}
			}
		}
		if running == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			if l, err := svc.List(context.Background()); err == nil {
				for _, v := range l {
					if v.State != "stopped" {
						_, _ = svc.Stop(context.Background(), v.Name, true)
					}
				}
			}
			return fmt.Errorf("%d VM(s) didn't shut down in time; forced off", running)
		case <-time.After(5 * time.Second):
		}
	}
}
