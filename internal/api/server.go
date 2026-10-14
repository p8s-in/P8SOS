// Package api exposes the p8sd REST API (v1).
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"p8sos/internal/creds"
	"p8sos/internal/monitor"
	"sync"
	"time"

	"p8sos/internal/audit"
	"p8sos/internal/billing"
	"p8sos/internal/ceph"
	"p8sos/internal/cloud"
	"p8sos/internal/cluster"
	"p8sos/internal/config"
	"p8sos/internal/console"
	"p8sos/internal/ct"
	"p8sos/internal/firewall"
	"p8sos/internal/ippool"
	"p8sos/internal/ldap"
	"p8sos/internal/license"
	"p8sos/internal/mail"
	"p8sos/internal/metrics"
	"p8sos/internal/offsite"
	"p8sos/internal/secpol"
	"p8sos/internal/sso"
	"p8sos/internal/storage"
	"p8sos/internal/storages"
	"p8sos/internal/support"
	"p8sos/internal/tasks"
	"p8sos/internal/tunnel"
	"p8sos/internal/updates"
	"p8sos/internal/users"
	"p8sos/internal/vm"
)

// VMService is what the API needs from the vm layer (faked in tests).
type VMService interface {
	Create(ctx context.Context, r vm.CreateRequest) (vm.VM, error)
	Get(ctx context.Context, name string) (vm.VM, error)
	List(ctx context.Context) ([]vm.VM, error)
	Start(ctx context.Context, name string) (vm.VM, error)
	Stop(ctx context.Context, name string, force bool) (vm.VM, error)
	Reboot(ctx context.Context, name string) (vm.VM, error)
	Delete(ctx context.Context, name string) error
	Resize(ctx context.Context, name string, r vm.ResizeRequest) (vm.VM, error)
	Snapshots(ctx context.Context, name string) ([]vm.Snapshot, error)
	CreateSnapshot(ctx context.Context, name, snap string) (vm.Snapshot, error)
	Rollback(ctx context.Context, name, snap string, destroyNewer bool) (vm.VM, error)
	DeleteSnapshot(ctx context.Context, name, snap string) error
	ConsoleAddr(ctx context.Context, name string) (string, error)
	StartBackup(ctx context.Context, name string) (vm.Job, error)
	ListBackups(name string) ([]vm.Backup, error)
	DeleteBackup(name, id string) error
	StartRestore(ctx context.Context, name, id, newName string) (vm.Job, error)
	GetSchedule(name string) (vm.Schedule, error)
	SetSchedule(name string, s vm.Schedule) error
	Job(id string) (vm.Job, bool)
	ListJobs() []vm.Job
	ListISOs() ([]vm.ISO, error)
	SaveISO(name string, r io.Reader) (vm.ISO, error)
	StartISODownload(ctx context.Context, url, name, sha256 string) (vm.Job, error)
	DeleteISO(ctx context.Context, name string) error
	EjectISO(ctx context.Context, name string) (vm.VM, error)
	Screenshot(ctx context.Context, name string) ([]byte, error)
	SetPassword(ctx context.Context, name, user, password string) error
	SetOwner(ctx context.Context, name, owner string) (vm.VM, error)
	GetFirewall(ctx context.Context, name string) (firewall.Settings, error)
	SetFirewall(ctx context.Context, name string, fw firewall.Settings) (firewall.Settings, error)
}

type Server struct {
	VMs       VMService
	Templates []config.Template
	Tokens    []config.Token
	Version   string
	Log       *slog.Logger
	Host      func(ctx context.Context) any // host capacity for the dashboard; nil = 404
	UI        http.Handler                  // web dashboard served at /; nil = none
	Tickets   *console.Tickets              // console passes; nil = created on first use
	// Metrics serves the usage charts; nil = no charts.
	Metrics interface {
		VM(name string) []metrics.Point
		Host() []metrics.HostPoint
	}
	// BackupKeep/BackupTime are the host defaults shown with backup schedules.
	BackupKeep int
	BackupTime string
	// Dial connects to a VM's VNC port; nil = net.Dialer (tests replace it).
	Dial func(ctx context.Context, addr string) (net.Conn, error)

	// Accounts. Users nil = only API tokens (one admin), as before v0.3.
	Users    *users.Store
	Sessions *users.Sessions
	Limiter  *users.Limiter  // failed sign-in lockout; nil = defaults
	Audit    *audit.Log      // who changed what; nil = not kept
	Issuer   string          // name shown in authenticator apps
	Tunnel   *tunnel.Manager // public websites via Cloudflare Tunnel; nil = off
	Pools    *ippool.Store   // public IP pools; nil = none
	// P8S Cloud (selling): settings and plans, wallet billing, email, support.
	Cloud    *cloud.Store
	Billing  *billing.Store
	Mail     *mail.Mailer
	Support  *support.Store
	Razorpay func(cloud.Settings) *billing.Razorpay // nil = real API
	// License is this server's P8S OS subscription; nil = everything allowed (tests).
	License *license.Manager
	// Updates installs new P8S OS versions from the update server; nil = not managed.
	Updates *updates.Manager
	// Node is the Server page: host tasks (templates, storage, network, Debian updates); nil = none.
	Node *tasks.Node
	// CTs are the containers (LXC); nil = not available.
	CTs *ct.Service
	// RemoteMetrics serves charts of VMs on other cluster servers (ok=false: local).
	RemoteMetrics func(name string) (any, bool)
	// Cluster joins servers (master/worker); nil = standalone only.
	Cluster *cluster.Manager
	// ClusterVMs is s.VMs when it spans the cluster (placement for the dashboard).
	ClusterVMs *cluster.VMs
	// Offsite copies backups to other places; nil = not available.
	Offsite *offsite.Manager
	// HostShell opens the server's root shell for admins; nil = turned off.
	HostShell func() (*ct.Terminal, error)
	// SSHPorts lists guest -> SSH port on this server; nil = off.
	SSHPorts func() map[string]int
	// Creds keeps the login details shown to owners (nil = none kept);
	// AutoPasswords makes a root password for VMs/containers created without one.
	Creds         *creds.Store
	AutoPasswords bool
	// Migrate moves VMs between cluster servers (cluster.VMs); LocalMoves is
	// this server's side of a move (vm.Service). nil = off.
	Migrate    Migrator
	LocalMoves cluster.LocalMover
	// LocalLive / LocalCPU: this server's side of live moves and CPU types (vm.Service).
	LocalLive cluster.LocalLive
	LocalCPU  cluster.LocalCPU
	// HA is high availability (cluster.HA); LocalHA this server's copies (vm.Service).
	HA      *cluster.HA
	LocalHA cluster.LocalHA
	// SSO is the company login (OpenID Connect); nil = off.
	SSO *sso.Manager
	// LDAP is sign-in with the company directory (Active Directory, OpenLDAP); nil = off.
	LDAP *ldap.Manager
	// Encryption reports the VM storage's encryption at rest (storage.ZFS); nil = unknown.
	Encryption func(ctx context.Context) (storage.Encryption, error)
	// Remote is remote access (VPN, Tailscale, tunnel status); nil = off.
	Remote *Remote
	// Storages are network storages (NFS, iSCSI); nil = off.
	Storages *storages.Manager
	// CephLocal is this server's Ceph part; Ceph drives all servers (master or
	// a server alone; nil on a worker). nil = Ceph off.
	CephLocal *ceph.Manager
	Ceph      *ceph.Cluster
	// LocalVMs lists this server's own VMs (not the cluster's); nil = VMs.List.
	LocalVMs func(ctx context.Context) ([]vm.VM, error)
	// Security: allowed networks, password policy, 2FA for staff; nil = defaults.
	Security *secpol.Store
	// Monitor watches the server's health and sends alerts; nil = off.
	Monitor *monitor.Monitor
	// Imports brings VMs in from Proxmox backups / disk images; nil = off.
	Imports      Importer
	ImportFolder string // shown so admins can scp files there
	// RDPPorts: Windows VM -> Remote Desktop port on this server; nil = off.
	RDPPorts func() map[string]int

	once      sync.Once
	lmOnce    sync.Once
	qlocks    sync.Map // user -> *sync.Mutex (quota checks)
	jobOwners sync.Map // job id -> who started it
}

func (s *Server) tickets() *console.Tickets {
	s.once.Do(func() {
		if s.Tickets == nil {
			s.Tickets = &console.Tickets{}
		}
	})
	return s.Tickets
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", s.health)
	mux.HandleFunc("GET /api/v1/console", s.consoleSocket)   // authorized by its one-time ticket
	mux.HandleFunc("GET /api/v1/terminal", s.terminalSocket) // container shell; one-time ticket
	mux.HandleFunc("GET /api/v1/auth", s.authInfo)
	mux.HandleFunc("POST /api/v1/login", s.login)
	mux.HandleFunc("GET /api/v1/sso/start", s.ssoStart)       // company login: to the provider
	mux.HandleFunc("GET /api/v1/sso/callback", s.ssoCallback) // ...and back
	mux.HandleFunc("POST /api/v1/signup", s.signup)
	mux.HandleFunc("POST /api/v1/verify", s.verifyEmail)
	mux.HandleFunc("POST /api/v1/verify/resend", s.resendVerify)
	mux.HandleFunc("POST /api/v1/password/forgot", s.forgotPassword)
	mux.HandleFunc("POST /api/v1/password/reset", s.resetPassword)
	mux.HandleFunc("POST /api/v1/billing/razorpay/webhook", s.razorpayWebhook)
	mux.HandleFunc("POST /api/v1/cluster/register", s.clusterRegister) // the join code is the pass
	mux.HandleFunc("POST /api/v1/cluster/invite", s.clusterInvite)     // waits for this server's admin

	auth := http.NewServeMux()
	auth.HandleFunc("GET /api/v1/templates", s.listTemplates)
	auth.HandleFunc("DELETE /api/v1/templates/{name}", s.deleteTemplate)
	auth.HandleFunc("POST /api/v1/vms/{name}/clone", s.cloneVM)
	auth.HandleFunc("POST /api/v1/vms/{name}/template", s.templateFromVM)
	auth.HandleFunc("GET /api/v1/vms", s.listVMs)
	auth.HandleFunc("POST /api/v1/vms", s.createVM)
	auth.HandleFunc("GET /api/v1/vms/{name}", s.getVM)
	auth.HandleFunc("POST /api/v1/vms/{name}/start", s.startVM)
	auth.HandleFunc("POST /api/v1/vms/{name}/stop", s.stopVM)
	auth.HandleFunc("POST /api/v1/vms/{name}/reboot", s.rebootVM)
	auth.HandleFunc("DELETE /api/v1/vms/{name}", s.deleteVM)
	auth.HandleFunc("PATCH /api/v1/vms/{name}", s.resizeVM)
	auth.HandleFunc("POST /api/v1/vms/{name}/disks", s.addDisk)
	auth.HandleFunc("PATCH /api/v1/vms/{name}/disks/{index}", s.resizeDisk)
	auth.HandleFunc("DELETE /api/v1/vms/{name}/disks/{index}", s.removeDisk)
	auth.HandleFunc("GET /api/v1/vms/{name}/snapshots", s.listSnapshots)
	auth.HandleFunc("POST /api/v1/vms/{name}/snapshots", s.createSnapshot)
	auth.HandleFunc("POST /api/v1/vms/{name}/snapshots/{snap}/rollback", s.rollback)
	auth.HandleFunc("DELETE /api/v1/vms/{name}/snapshots/{snap}", s.deleteSnapshot)
	auth.HandleFunc("GET /api/v1/host", s.hostInfo)
	auth.HandleFunc("POST /api/v1/vms/{name}/console", s.consoleTicket)
	auth.HandleFunc("POST /api/v1/vms/{name}/password", s.setPassword)
	auth.HandleFunc("GET /api/v1/vms/{name}/login", s.vmLogin)
	auth.HandleFunc("GET /api/v1/backups", s.listBackups)
	auth.HandleFunc("GET /api/v1/vms/{name}/backups", s.listBackups)
	auth.HandleFunc("POST /api/v1/vms/{name}/backups", s.startBackup)
	auth.HandleFunc("DELETE /api/v1/backups/{name}/{id}", s.deleteBackup)
	auth.HandleFunc("POST /api/v1/backups/{name}/{id}/restore", s.restoreBackup)
	auth.HandleFunc("POST /api/v1/backups/{name}/{id}/verify", s.verifyBackup)
	auth.HandleFunc("GET /api/v1/vms/{name}/backup-schedule", s.getSchedule)
	auth.HandleFunc("PUT /api/v1/vms/{name}/backup-schedule", s.putSchedule)
	auth.HandleFunc("GET /api/v1/vms/{name}/metrics", s.vmMetrics)
	auth.HandleFunc("GET /api/v1/host/metrics", s.hostMetrics)
	auth.HandleFunc("GET /api/v1/isos", s.listISOs)
	auth.HandleFunc("PUT /api/v1/isos/{name}", s.uploadISO)
	auth.HandleFunc("GET /api/v1/vms/{name}/move", s.moveInfo)
	auth.HandleFunc("POST /api/v1/vms/{name}/move", s.startMove)
	auth.HandleFunc("GET /api/v1/vms/{name}/move-check", s.moveCheck)
	auth.HandleFunc("GET /api/v1/vms/{name}/live-check", s.liveCheck)
	auth.HandleFunc("PUT /api/v1/vms/{name}/cpu", s.putCPU)
	auth.HandleFunc("GET /api/v1/cluster/cpu", s.clusterCPU)
	auth.HandleFunc("GET /api/v1/cluster/live-header/{name}", s.liveHeader)
	auth.HandleFunc("POST /api/v1/cluster/live-prepare", s.livePrepare)
	auth.HandleFunc("POST /api/v1/cluster/live-send/{name}", s.liveSend)
	auth.HandleFunc("POST /api/v1/cluster/live-finish/{name}", s.liveFinish)
	auth.HandleFunc("POST /api/v1/cluster/live-abort/{name}", s.liveAbort)
	auth.HandleFunc("POST /api/v1/cluster/live-cleanup/{name}", s.liveCleanup)
	auth.HandleFunc("GET /api/v1/cluster/vm-export/{name}", s.vmExport)
	auth.HandleFunc("POST /api/v1/cluster/vm-import", s.vmImport)
	auth.HandleFunc("PUT /api/v1/vms/{name}/autostart", s.setAutostart)
	auth.HandleFunc("GET /api/v1/ha", s.haStatus)
	auth.HandleFunc("PUT /api/v1/ha/settings", s.haSettings)
	auth.HandleFunc("POST /api/v1/ha/replicas/{name}/start", s.haStartCopy)
	auth.HandleFunc("DELETE /api/v1/ha/replicas/{name}", s.haDropCopy)
	auth.HandleFunc("GET /api/v1/vms/{name}/ha", s.haInfo)
	auth.HandleFunc("PUT /api/v1/vms/{name}/ha", s.haSet)
	auth.HandleFunc("POST /api/v1/vms/{name}/ha/failover", s.haFailover)
	auth.HandleFunc("GET /api/v1/vms/{name}/ha-check", s.haCheck)
	auth.HandleFunc("GET /api/v1/cluster/ha-sync", s.haSync)
	auth.HandleFunc("POST /api/v1/cluster/book", s.clusterBook)
	auth.HandleFunc("GET /api/v1/cluster/peer", s.clusterPeer)
	auth.HandleFunc("POST /api/v1/cluster/vote", s.clusterVote)
	auth.HandleFunc("POST /api/v1/cluster/promote", s.clusterPromote)
	auth.HandleFunc("GET /api/v1/cluster/replica/{name}", s.replicaSnaps)
	auth.HandleFunc("DELETE /api/v1/cluster/replica/{name}", s.haDropCopy)
	auth.HandleFunc("GET /api/v1/cluster/replica-send/{name}", s.replicaSend)
	auth.HandleFunc("POST /api/v1/cluster/replica-recv", s.replicaRecv)
	auth.HandleFunc("POST /api/v1/cluster/replica/{name}/promote", s.replicaPromote)
	auth.HandleFunc("POST /api/v1/cluster/replica/{name}/retire", s.replicaRetire)
	auth.HandleFunc("GET /api/v1/alerts", s.getAlerts)
	auth.HandleFunc("PUT /api/v1/alerts/settings", s.putAlertSettings)
	auth.HandleFunc("POST /api/v1/alerts/test", s.testAlert)
	auth.HandleFunc("GET /api/v1/imports", s.listImports)
	auth.HandleFunc("GET /api/v1/imports/{file}/plan", s.importPlan)
	auth.HandleFunc("PUT /api/v1/imports/{file}", s.uploadImport)
	auth.HandleFunc("DELETE /api/v1/imports/{file}", s.deleteImport)
	auth.HandleFunc("POST /api/v1/imports/{file}/start", s.startImport)
	auth.HandleFunc("POST /api/v1/isos/download", s.downloadISO)
	auth.HandleFunc("DELETE /api/v1/isos/{name}", s.deleteISO)
	auth.HandleFunc("POST /api/v1/vms/{name}/eject", s.ejectISO)
	auth.HandleFunc("GET /api/v1/vms/{name}/screenshot", s.screenshot)
	auth.HandleFunc("GET /api/v1/jobs", s.listJobs)
	auth.HandleFunc("GET /api/v1/jobs/{id}", s.getJob)
	auth.HandleFunc("POST /api/v1/logout", s.logout)
	auth.HandleFunc("GET /api/v1/me", s.me)
	auth.HandleFunc("POST /api/v1/me/password", s.changeMyPassword)
	auth.HandleFunc("PUT /api/v1/me/ssh-keys", s.setMyKeys)
	auth.HandleFunc("POST /api/v1/me/2fa/setup", s.setup2FA)
	auth.HandleFunc("POST /api/v1/me/2fa/enable", s.enable2FA)
	auth.HandleFunc("POST /api/v1/me/2fa/disable", s.disable2FA)
	auth.HandleFunc("GET /api/v1/users", s.listUsers)
	auth.HandleFunc("POST /api/v1/users", s.addUser)
	auth.HandleFunc("PATCH /api/v1/users/{user}", s.updateUser)
	auth.HandleFunc("DELETE /api/v1/users/{user}", s.deleteUser)
	auth.HandleFunc("PUT /api/v1/vms/{name}/owner", s.setOwner)
	auth.HandleFunc("PUT /api/v1/me/profile", s.setProfile)
	auth.HandleFunc("GET /api/v1/me/tokens", s.listTokens)
	auth.HandleFunc("POST /api/v1/me/tokens", s.newToken)
	auth.HandleFunc("DELETE /api/v1/me/tokens/{id}", s.deleteToken)
	auth.HandleFunc("GET /api/v1/plans", s.listPlans)
	auth.HandleFunc("PUT /api/v1/plans/{id}", s.putPlan)
	auth.HandleFunc("DELETE /api/v1/plans/{id}", s.deletePlan)
	auth.HandleFunc("GET /api/v1/cloud/settings", s.getCloudSettings)
	auth.HandleFunc("PUT /api/v1/cloud/settings", s.putCloudSettings)
	auth.HandleFunc("POST /api/v1/cloud/test-email", s.testEmail)
	auth.HandleFunc("GET /api/v1/cloud/outbox", s.outbox)
	auth.HandleFunc("GET /api/v1/billing", s.getBilling)
	auth.HandleFunc("GET /api/v1/billing/entries", s.billingEntries)
	auth.HandleFunc("POST /api/v1/billing/topup", s.topup)
	auth.HandleFunc("POST /api/v1/billing/topup/verify", s.topupVerify)
	auth.HandleFunc("GET /api/v1/billing/invoices", s.listInvoices)
	auth.HandleFunc("GET /api/v1/billing/invoices/{number}", s.getInvoice)
	auth.HandleFunc("POST /api/v1/billing/adjust", s.adjust)
	auth.HandleFunc("GET /api/v1/billing/customers", s.customers)
	auth.HandleFunc("GET /api/v1/tickets", s.listTickets)
	auth.HandleFunc("POST /api/v1/tickets", s.openTicket)
	auth.HandleFunc("GET /api/v1/tickets/{id}", s.getTicket)
	auth.HandleFunc("POST /api/v1/tickets/{id}/reply", s.replyTicket)
	auth.HandleFunc("POST /api/v1/tickets/{id}/close", s.closeTicket)
	auth.HandleFunc("GET /api/v1/ippools", s.listPools)
	auth.HandleFunc("POST /api/v1/ippools", s.addPool)
	auth.HandleFunc("PATCH /api/v1/ippools/{pool}", s.updatePool)
	auth.HandleFunc("DELETE /api/v1/ippools/{pool}", s.deletePool)
	auth.HandleFunc("GET /api/v1/tunnel", s.getTunnel)
	auth.HandleFunc("POST /api/v1/tunnel/setup", s.setupTunnel)
	auth.HandleFunc("DELETE /api/v1/tunnel", s.disconnectTunnel)
	auth.HandleFunc("PUT /api/v1/tunnel/dashboard", s.setDashboardHost)
	auth.HandleFunc("GET /api/v1/sites", s.listSites)
	auth.HandleFunc("GET /api/v1/vms/{name}/sites", s.listSites)
	auth.HandleFunc("POST /api/v1/vms/{name}/sites", s.addSite)
	auth.HandleFunc("DELETE /api/v1/sites/{host}", s.deleteSite)
	auth.HandleFunc("GET /api/v1/vms/{name}/firewall", s.getFirewall)
	auth.HandleFunc("PUT /api/v1/vms/{name}/firewall", s.putFirewall)
	auth.HandleFunc("GET /api/v1/node/encryption", s.getEncryption)
	auth.HandleFunc("POST /api/v1/node/encryption-key", s.encryptionKey)
	auth.HandleFunc("GET /api/v1/remote", s.getRemote)
	auth.HandleFunc("GET /api/v1/storages", s.listStorages)
	auth.HandleFunc("POST /api/v1/storages", s.addStorage)
	auth.HandleFunc("POST /api/v1/storages/discover", s.discoverISCSI)
	auth.HandleFunc("POST /api/v1/storages/{name}/connect", s.connectStorage)
	auth.HandleFunc("DELETE /api/v1/storages/{name}", s.removeStorage)
	auth.HandleFunc("GET /api/v1/ceph", s.getCeph)
	auth.HandleFunc("POST /api/v1/ceph/install", s.cephStep(ceph.ActInstall))
	auth.HandleFunc("POST /api/v1/ceph/init", s.cephStep(ceph.ActInit))
	auth.HandleFunc("POST /api/v1/ceph/mons", s.cephStep(ceph.ActMon))
	auth.HandleFunc("POST /api/v1/ceph/mgrs", s.cephStep(ceph.ActMgr))
	auth.HandleFunc("POST /api/v1/ceph/osds", s.cephStep(ceph.ActOSD))
	auth.HandleFunc("DELETE /api/v1/ceph/osds/{id}", s.removeCephOSD)
	auth.HandleFunc("POST /api/v1/ceph/pool", s.createCephPool)
	auth.HandleFunc("GET /api/v1/ceph/node", s.getCephNode)
	auth.HandleFunc("POST /api/v1/ceph/node/config", s.setCephConfig)
	auth.HandleFunc("POST /api/v1/ceph/node/{action}", s.cephNodeStep)
	auth.HandleFunc("PUT /api/v1/remote/vpn", s.putVPN)
	auth.HandleFunc("POST /api/v1/remote/vpn/devices", s.addVPNDevice)
	auth.HandleFunc("DELETE /api/v1/remote/vpn/devices/{id}", s.deleteVPNDevice)
	auth.HandleFunc("POST /api/v1/remote/forward", s.forwardVPN)
	auth.HandleFunc("GET /api/v1/security", s.getSecurity)
	auth.HandleFunc("PUT /api/v1/security", s.putSecurity)
	auth.HandleFunc("GET /api/v1/ldap", s.getLDAP)
	auth.HandleFunc("PUT /api/v1/ldap", s.putLDAP)
	auth.HandleFunc("POST /api/v1/ldap/test", s.testLDAP)
	auth.HandleFunc("GET /api/v1/sso", s.getSSO)
	auth.HandleFunc("PUT /api/v1/sso", s.putSSO)
	auth.HandleFunc("GET /api/v1/audit", s.auditLog)
	auth.HandleFunc("GET /api/v1/audit/export", s.auditExport)
	auth.HandleFunc("GET /api/v1/audit/verify", s.auditVerify)
	auth.HandleFunc("GET /api/v1/license", s.getLicense)
	auth.HandleFunc("PUT /api/v1/license", s.putLicense)
	auth.HandleFunc("DELETE /api/v1/license", s.deleteLicense)
	auth.HandleFunc("POST /api/v1/license/check", s.checkLicense)
	auth.HandleFunc("GET /api/v1/containers", s.listCTs)
	auth.HandleFunc("POST /api/v1/containers", s.createCT)
	auth.HandleFunc("GET /api/v1/ct-templates", s.ctTemplates)
	auth.HandleFunc("GET /api/v1/containers/{name}", s.getCT)
	auth.HandleFunc("DELETE /api/v1/containers/{name}", s.deleteCT)
	auth.HandleFunc("PATCH /api/v1/containers/{name}", s.resizeCT)
	auth.HandleFunc("POST /api/v1/containers/{name}/{action}", s.ctPower)
	auth.HandleFunc("POST /api/v1/containers/{name}/console", s.ctConsole)
	auth.HandleFunc("POST /api/v1/containers/{name}/password", s.ctPassword)
	auth.HandleFunc("GET /api/v1/containers/{name}/login", s.ctLogin)
	auth.HandleFunc("POST /api/v1/containers/{name}/setup-ssh", s.ctSetupSSH)
	auth.HandleFunc("PUT /api/v1/containers/{name}/owner", s.ctOwner)
	auth.HandleFunc("GET /api/v1/containers/{name}/firewall", s.ctFirewall)
	auth.HandleFunc("GET /api/v1/containers/{name}/backups", s.ctBackups)
	auth.HandleFunc("POST /api/v1/containers/{name}/backups", s.ctBackup)
	auth.HandleFunc("GET /api/v1/ct-backups", s.ctBackups)
	auth.HandleFunc("GET /api/v1/cluster", s.getCluster)
	auth.HandleFunc("POST /api/v1/cluster/join-code", s.clusterJoinCode)
	auth.HandleFunc("POST /api/v1/cluster/join", s.clusterJoin)
	auth.HandleFunc("POST /api/v1/cluster/leave", s.clusterLeave)
	auth.HandleFunc("DELETE /api/v1/cluster/nodes/{id}", s.clusterRemove)
	auth.HandleFunc("GET /api/v1/cluster/capacity", s.clusterCapacity)
	auth.HandleFunc("GET /api/v1/cluster/candidates", s.clusterCandidates)
	auth.HandleFunc("GET /api/v1/cluster/vnc/{name}", s.clusterVNC)
	auth.HandleFunc("GET /api/v1/cluster/shell", s.clusterShell)
	auth.HandleFunc("GET /api/v1/cluster/discover", s.clusterDiscover)
	auth.HandleFunc("POST /api/v1/cluster/invite-send", s.clusterInviteSend)
	auth.HandleFunc("GET /api/v1/cluster/invites", s.clusterInvites)
	auth.HandleFunc("POST /api/v1/cluster/invites/{id}/{answer}", s.clusterInviteAnswer)
	auth.HandleFunc("GET /api/v1/cluster/public-ip", s.clusterPublicIP)
	auth.HandleFunc("POST /api/v1/cluster/check", s.clusterCheck)
	auth.HandleFunc("GET /api/v1/offsite", s.getOffsite)
	auth.HandleFunc("PUT /api/v1/offsite/targets/{name}", s.putOffsite)
	auth.HandleFunc("DELETE /api/v1/offsite/targets/{name}", s.deleteOffsite)
	auth.HandleFunc("GET /api/v1/offsite/targets/{name}/backups", s.offsiteBackups)
	auth.HandleFunc("POST /api/v1/offsite/targets/{name}/{action}", s.offsiteAction)
	auth.HandleFunc("GET /api/v1/containers/{name}/backup-schedule", s.ctSchedule)
	auth.HandleFunc("PUT /api/v1/containers/{name}/backup-schedule", s.ctSchedule)
	auth.HandleFunc("DELETE /api/v1/ct-backups/{name}/{id}", s.ctDeleteBackup)
	auth.HandleFunc("POST /api/v1/ct-backups/{name}/{id}/restore", s.ctRestore)
	auth.HandleFunc("PUT /api/v1/containers/{name}/firewall", s.ctFirewall)
	auth.HandleFunc("GET /api/v1/containers/{name}/snapshots", s.ctSnapshots)
	auth.HandleFunc("POST /api/v1/containers/{name}/snapshots", s.ctSnapshot)
	auth.HandleFunc("POST /api/v1/containers/{name}/snapshots/{snap}/rollback", s.ctRollback)
	auth.HandleFunc("DELETE /api/v1/containers/{name}/snapshots/{snap}", s.ctDeleteSnapshot)
	auth.HandleFunc("GET /api/v1/ssh-ports", s.sshPorts)
	auth.HandleFunc("GET /api/v1/node", s.getNode)
	auth.HandleFunc("GET /api/v1/node/memory", s.getMemory)
	auth.HandleFunc("GET /api/v1/node/tasks", s.listTasks)
	auth.HandleFunc("POST /api/v1/node/shell", s.hostShellTicket)
	auth.HandleFunc("POST /api/v1/node/tasks", s.startTask)
	auth.HandleFunc("GET /api/v1/node/tasks/{id}", s.getTask)
	auth.HandleFunc("GET /api/v1/updates", s.getUpdates)
	auth.HandleFunc("POST /api/v1/updates/check", s.checkUpdates)
	auth.HandleFunc("POST /api/v1/updates/install", s.installUpdate)
	auth.HandleFunc("PUT /api/v1/updates/channel", s.setUpdateChannel)
	mux.Handle("/api/v1/", s.requireAuth(s.audited(s.roleGuard(auth))))
	if s.UI != nil {
		mux.Handle("/", s.UI) // the page itself is public; every API call it makes needs the token
	}

	return s.logRequests(s.ipAllow(mux))
}

// --- handlers ---

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.Version})
}

func (s *Server) listTemplates(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"templates": s.templateList(r.Context())})
}

func (s *Server) hostInfo(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	if s.Host == nil {
		writeErr(w, http.StatusNotFound, "not_found", "host info not available")
		return
	}
	writeJSON(w, http.StatusOK, s.Host(r.Context()))
}

func (s *Server) listVMs(w http.ResponseWriter, r *http.Request) {
	vms, err := s.VMs.List(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	p, out := who(r), []vm.VM{}
	for _, v := range vms {
		if mayUse(p, v.Owner) {
			if v.MemUsedMiB == 0 && s.Metrics != nil && v.State == "running" {
				if pts := s.Metrics.VM(v.Name); len(pts) > 0 {
					v.MemUsedMiB = pts[len(pts)-1].GuestMiB
				}
			}
			out = append(out, v)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"vms": out})
}

func (s *Server) createVM(w http.ResponseWriter, r *http.Request) {
	var req vm.CreateRequest
	if !decode(w, r, &req) {
		return
	}
	p := who(r)
	if s.CTs != nil && s.CTs.Exists(req.Name) {
		writeErr(w, http.StatusConflict, "exists", fmt.Sprintf("a container is already called %q", req.Name))
		return
	}
	plan, ok := s.applyPlan(w, r, &req)
	if !ok {
		return
	}
	if !p.Admin {
		req.Owner = p.Name
		if req.Network == vm.NetLAN && !p.Quota.LAN {
			writeErr(w, http.StatusForbidden, "forbidden", "your plan doesn't include the LAN network; use nat")
			return
		}
		d := Usage{VMs: 1, VCPUs: max(req.VCPUs, 1), MemoryMiB: max(req.MemoryMiB, 1024), DiskGB: s.diskFor(req)}
		if req.IPPool != "" {
			if !s.customerPool(req.IPPool) {
				writeErr(w, http.StatusNotFound, "not_found", fmt.Sprintf("no ip pool %q for customers", req.IPPool))
				return
			}
			d.PublicIPs = 1
		}
		defer s.quotaLock(p.Name)()
		if err := s.checkQuota(r.Context(), p, d); err != nil {
			s.fail(w, r, err)
			return
		}
		if !s.canAfford(w, r, p.Name, plan) {
			return
		}
	} else if req.Owner != "" && !p.Cluster { // the master checked its own accounts
		if s.Users == nil {
			writeErr(w, http.StatusBadRequest, "invalid", "user accounts are not enabled on this host")
			return
		}
		if _, err := s.Users.Get(req.Owner); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	if req.Node != "" && !p.Admin {
		req.Node = "" // customers get the best server
	}
	if req.Owner != "" {
		req.NoDefaultKeys = true
		if len(req.SSHKeys) == 0 && s.Users != nil {
			if u, err := s.Users.Get(req.Owner); err == nil {
				req.SSHKeys = u.SSHKeys
			}
		}
	}
	generated := false
	if req.ISO == "" { // installer VMs set their passwords in the installer
		generated = s.autoPassword(r, &req.RootPassword)
	}
	v, err := s.VMs.Create(r.Context(), req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.afterCreate(r, v, plan)
	var login *loginView
	if req.RootPassword != "" && !who(r).Cluster {
		l := creds.Login{User: "root", Password: req.RootPassword, SSH: req.SSHPasswordAuth == nil || *req.SSHPasswordAuth}
		if err := s.Creds.Set("vm", v.Name, l); err != nil && s.Log != nil {
			s.Log.Error("store vm login", "vm", v.Name, "err", err)
		}
		if generated { // a password the caller typed is never sent back
			lv := viewOf(l, true)
			login = &lv
		}
	}
	w.Header().Set("Location", "/api/v1/vms/"+v.Name)
	writeJSON(w, http.StatusCreated, struct {
		vm.VM
		Login *loginView `json:"login,omitempty"`
	}{v, login})
}

func (s *Server) getVM(w http.ResponseWriter, r *http.Request) {
	v, ok := s.vmFor(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) startVM(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.vmFor(w, r); !ok {
		return
	}
	if s.suspended(w, r) {
		return
	}
	v, err := s.VMs.Start(r.Context(), r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) stopVM(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.vmFor(w, r); !ok {
		return
	}
	var body struct {
		Force bool `json:"force"`
	}
	if r.ContentLength != 0 && !decode(w, r, &body) {
		return
	}
	v, err := s.VMs.Stop(r.Context(), r.PathValue("name"), body.Force)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, v)
}

func (s *Server) rebootVM(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.vmFor(w, r); !ok {
		return
	}
	v, err := s.VMs.Reboot(r.Context(), r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, v)
}

func (s *Server) deleteVM(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.vmFor(w, r); !ok {
		return
	}
	if err := s.VMs.Delete(r.Context(), r.PathValue("name")); err != nil {
		s.fail(w, r, err)
		return
	}
	_ = s.Creds.Delete("vm", r.PathValue("name"))
	if s.Billing != nil { // stop charging
		if err := s.Billing.Unsubscribe(r.PathValue("name")); err != nil && s.Log != nil {
			s.Log.Error("stop billing of deleted vm", "vm", r.PathValue("name"), "err", err)
		}
	}
	if s.Tunnel != nil { // its websites go with it
		if err := s.Tunnel.RemoveVM(context.WithoutCancel(r.Context()), r.PathValue("name")); err != nil && s.Log != nil {
			s.Log.Error("remove websites of deleted vm", "vm", r.PathValue("name"), "err", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) resizeVM(w http.ResponseWriter, r *http.Request) {
	cur, ok := s.vmFor(w, r)
	if !ok {
		return
	}
	if s.Billing != nil && !who(r).Admin {
		if _, billed := s.Billing.Sub(cur.Name); billed {
			writeErr(w, http.StatusBadRequest, "invalid", "this server is on a plan; to change its size, ask support (plan changes are coming)")
			return
		}
	}
	var req vm.ResizeRequest
	if !decode(w, r, &req) {
		return
	}
	if p := who(r); !p.Admin {
		defer s.quotaLock(p.Name)()
		d := Usage{}
		if req.VCPUs > 0 {
			d.VCPUs = req.VCPUs - cur.VCPUs
		}
		if req.MemoryMiB > 0 {
			d.MemoryMiB = req.MemoryMiB - cur.MemoryMiB
		}
		if req.DiskGB > 0 {
			d.DiskGB = req.DiskGB - cur.DiskGB
		}
		if err := s.checkQuota(r.Context(), p, d); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	v, err := s.VMs.Resize(r.Context(), r.PathValue("name"), req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) listSnapshots(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.vmFor(w, r); !ok {
		return
	}
	snaps, err := s.VMs.Snapshots(r.Context(), r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": snaps})
}

func (s *Server) createSnapshot(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.vmFor(w, r); !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if r.ContentLength != 0 && !decode(w, r, &body) {
		return
	}
	sn, err := s.VMs.CreateSnapshot(r.Context(), r.PathValue("name"), body.Name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, sn)
}

func (s *Server) rollback(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.vmFor(w, r); !ok {
		return
	}
	var body struct {
		DestroyNewer bool `json:"destroy_newer"`
	}
	if r.ContentLength != 0 && !decode(w, r, &body) {
		return
	}
	v, err := s.VMs.Rollback(r.Context(), r.PathValue("name"), r.PathValue("snap"), body.DestroyNewer)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.vmFor(w, r); !ok {
		return
	}
	if err := s.VMs.DeleteSnapshot(r.Context(), r.PathValue("name"), r.PathValue("snap")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) consoleTicket(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.vmFor(w, r); !ok {
		return
	}
	name := r.PathValue("name")
	if _, err := s.VMs.ConsoleAddr(r.Context(), name); err != nil {
		s.fail(w, r, err)
		return
	}
	t, ttl := s.tickets().Issue(name)
	writeJSON(w, http.StatusCreated, map[string]any{
		"ticket": t, "path": "/api/v1/console?ticket=" + t, "expires_in": int(ttl.Seconds()),
	})
}

// consoleSocket bridges the browser (noVNC over WebSocket) to the VM's VNC port.
func (s *Server) consoleSocket(w http.ResponseWriter, r *http.Request) {
	if o := r.Header.Get("Origin"); o != "" {
		if u, err := url.Parse(o); err != nil || u.Host != r.Host {
			writeErr(w, http.StatusForbidden, "forbidden", "console must be opened from this host's dashboard")
			return
		}
	}
	name, ok := s.tickets().Redeem(r.URL.Query().Get("ticket"))
	if !ok {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "console ticket is invalid, used or expired")
		return
	}
	addr, err := s.VMs.ConsoleAddr(r.Context(), name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	dial := s.Dial
	if dial == nil {
		d := &net.Dialer{Timeout: 5 * time.Second}
		dial = func(ctx context.Context, addr string) (net.Conn, error) { return d.DialContext(ctx, "tcp", addr) }
	}
	tcp, err := dial(r.Context(), addr)
	if err != nil {
		s.fail(w, r, fmt.Errorf("connect to the VM's screen at %s: %w", addr, err))
		return
	}
	ws, err := console.Upgrade(w, r)
	if err != nil {
		tcp.Close()
		return
	}
	if s.Log != nil {
		s.Log.Info("console opened", "vm", name, "remote", r.RemoteAddr)
	}
	console.Proxy(ws, tcp)
}

func (s *Server) listBackups(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name != "" {
		if _, err := s.VMs.Get(r.Context(), name); err != nil && !errors.Is(err, vm.ErrNotFound) {
			s.fail(w, r, err)
			return
		}
	}
	b, err := s.VMs.ListBackups(name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	p, out := who(r), []vm.Backup{}
	for _, m := range b {
		if mayUse(p, m.Owner) {
			out = append(out, m)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"backups": out})
}

func (s *Server) startBackup(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.vmFor(w, r); !ok {
		return
	}
	j, err := s.VMs.StartBackup(r.Context(), r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.noteJob(r, j.ID)
	w.Header().Set("Location", "/api/v1/jobs/"+j.ID)
	writeJSON(w, http.StatusAccepted, j)
}

func (s *Server) deleteBackup(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.backupFor(w, r); !ok {
		return
	}
	if err := s.VMs.DeleteBackup(r.PathValue("name"), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) restoreBackup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"` // new VM name; empty = restore over the original VM
	}
	if r.ContentLength != 0 && !decode(w, r, &body) {
		return
	}
	m, ok := s.backupFor(w, r)
	if !ok {
		return
	}
	p := who(r)
	target := body.Name
	if target == "" {
		target = m.VM
	}
	if !p.Admin {
		defer s.quotaLock(p.Name)()
		v, err := s.VMs.Get(r.Context(), target)
		switch {
		case err == nil && !mayUse(p, v.Owner):
			writeErr(w, http.StatusConflict, "exists", fmt.Sprintf("the name %q is taken; restore under another name", target))
			return
		case errors.Is(err, vm.ErrNotFound): // becomes a new VM: must fit the plan
			d := Usage{VMs: 1, VCPUs: m.VCPUs, MemoryMiB: m.MemoryMiB, DiskGB: m.DiskGB}
			if m.Pool != "" && target == m.VM {
				d.PublicIPs = 1 // a recreated VM gets its public address back
			}
			if err := s.checkQuota(r.Context(), p, d); err != nil {
				s.fail(w, r, err)
				return
			}
		case err != nil:
			s.fail(w, r, err)
			return
		}
	}
	j, err := s.VMs.StartRestore(r.Context(), r.PathValue("name"), r.PathValue("id"), body.Name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.noteJob(r, j.ID)
	w.Header().Set("Location", "/api/v1/jobs/"+j.ID)
	writeJSON(w, http.StatusAccepted, j)
}

type scheduleView struct {
	Daily bool   `json:"daily"`
	Keep  int    `json:"keep"`
	At    string `json:"at"` // host time of the daily run (informational)
}

func (s *Server) getSchedule(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.vmFor(w, r); !ok {
		return
	}
	s.showSchedule(w, r, r.PathValue("name"))
}

func (s *Server) putSchedule(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.vmFor(w, r); !ok {
		return
	}
	s.saveSchedule(w, r, r.PathValue("name"))
}

// showSchedule / saveSchedule serve the daily backup of a VM (key = name)
// or a container (key = "ct:NAME"); the caller checked access.
func (s *Server) showSchedule(w http.ResponseWriter, r *http.Request, key string) {
	sch, err := s.VMs.GetSchedule(key)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	keep := sch.Keep
	if keep == 0 {
		keep = s.BackupKeep
	}
	writeJSON(w, http.StatusOK, scheduleView{Daily: sch.Daily, Keep: keep, At: s.BackupTime})
}

func (s *Server) saveSchedule(w http.ResponseWriter, r *http.Request, key string) {
	var body struct {
		Daily bool `json:"daily"`
		Keep  int  `json:"keep"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Daily && body.Keep == 0 {
		body.Keep = s.BackupKeep
	}
	if err := s.VMs.SetSchedule(key, vm.Schedule{Daily: body.Daily, Keep: body.Keep}); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, scheduleView{Daily: body.Daily, Keep: body.Keep, At: s.BackupTime})
}

func (s *Server) vmMetrics(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := s.vmFor(w, r); !ok {
		return
	}
	if s.RemoteMetrics != nil { // a VM on another cluster server
		if out, ok := s.RemoteMetrics(name); ok {
			writeJSON(w, http.StatusOK, out)
			return
		}
	}
	pts := []metrics.Point{}
	if s.Metrics != nil {
		pts = s.Metrics.VM(name)
	}
	writeJSON(w, http.StatusOK, map[string]any{"interval": 10, "points": pts})
}

func (s *Server) hostMetrics(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	pts := []metrics.HostPoint{}
	if s.Metrics != nil {
		pts = s.Metrics.Host()
	}
	writeJSON(w, http.StatusOK, map[string]any{"interval": 10, "points": pts})
}

func (s *Server) listISOs(w http.ResponseWriter, r *http.Request) {
	l, err := s.VMs.ListISOs()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"isos": l})
}

// uploadISO takes the raw image as the request body (PUT, any size up to the
// library's limit). Uploads can take long, so this request gets hours, not minutes.
func (s *Server) uploadISO(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(6 * time.Hour))
	_ = rc.SetWriteDeadline(time.Now().Add(6 * time.Hour))
	iso, err := s.VMs.SaveISO(r.PathValue("name"), r.Body)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, iso)
}

func (s *Server) downloadISO(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	var body struct {
		URL    string `json:"url"`
		Name   string `json:"name"`
		SHA256 string `json:"sha256"`
	}
	if !decode(w, r, &body) {
		return
	}
	j, err := s.VMs.StartISODownload(r.Context(), body.URL, body.Name, body.SHA256)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.noteJob(r, j.ID)
	w.Header().Set("Location", "/api/v1/jobs/"+j.ID)
	writeJSON(w, http.StatusAccepted, j)
}

func (s *Server) deleteISO(w http.ResponseWriter, r *http.Request) {
	if !s.adminOnly(w, r) {
		return
	}
	if err := s.VMs.DeleteISO(r.Context(), r.PathValue("name")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) screenshot(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.vmFor(w, r); !ok {
		return
	}
	b, err := s.VMs.Screenshot(r.Context(), r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

func (s *Server) ejectISO(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.vmFor(w, r); !ok {
		return
	}
	v, err := s.VMs.EjectISO(r.Context(), r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	owned, out := s.ownedNames(r.Context(), who(r)), []vm.Job{}
	for _, j := range s.VMs.ListJobs() {
		if s.mayJob(r, j, owned) {
			out = append(out, j)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out})
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	j, ok := s.VMs.Job(r.PathValue("id"))
	if ok && !who(r).Admin {
		ok = s.mayJob(r, j, s.ownedNames(r.Context(), who(r)))
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "no such job")
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (s *Server) setPassword(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.vmFor(w, r); !ok {
		return
	}
	var body struct {
		User     string `json:"user"`
		Password string `json:"password"`
		Generate bool   `json:"generate"` // make a random one and return it
	}
	if !decode(w, r, &body) {
		return
	}
	if body.User == "" {
		body.User = "root"
	}
	if body.Generate {
		body.Password = creds.Generate(PasswordLength)
	}
	if err := s.VMs.SetPassword(r.Context(), r.PathValue("name"), body.User, body.Password); err != nil {
		s.fail(w, r, err)
		return
	}
	s.rememberPassword("vm", r.PathValue("name"), body.User, body.Password)
	if !body.Generate {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"user": body.User, "password": body.Password})
}

// --- middleware ---

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int) { w.code = c; w.ResponseWriter.WriteHeader(c) }

// Unwrap lets http.ResponseController reach the real writer (WebSocket hijack).
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(sw, r)
		if s.Log != nil {
			s.Log.Info("http", "method", r.Method, "path", r.URL.Path, "status", sw.code,
				"dur_ms", time.Since(start).Milliseconds(), "remote", r.RemoteAddr)
		}
	})
}

// --- helpers ---

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errQuota):
		writeErr(w, http.StatusForbidden, "quota", err.Error())
	case errors.Is(err, users.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, users.ErrExists):
		writeErr(w, http.StatusConflict, "exists", err.Error())
	case errors.Is(err, users.ErrInvalid):
		writeErr(w, http.StatusBadRequest, "invalid", err.Error())
	case errors.Is(err, users.ErrBadOTP):
		writeErr(w, http.StatusBadRequest, "bad_otp", err.Error())
	case errors.Is(err, vm.ErrInvalid):
		writeErr(w, http.StatusBadRequest, "invalid", err.Error())
	case errors.Is(err, vm.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, vm.ErrExists):
		writeErr(w, http.StatusConflict, "exists", err.Error())
	case errors.Is(err, vm.ErrConflict):
		writeErr(w, http.StatusConflict, "conflict", err.Error())
	default:
		ref := newRef()
		if s.Log != nil {
			s.Log.Error("request failed", "err", err, "ref", ref, "user", who(r).Name)
		}
		if !who(r).Admin {
			// Customers don't get to read the host's internals (zfs/libvirt output).
			writeErr(w, http.StatusInternalServerError, "internal",
				"the server couldn't do that right now; try again, or contact support with reference "+ref)
			return
		}
		// Admins see the real cause (libvirt/zfs stderr), so problems can be
		// fixed without digging through the journal.
		writeErr(w, http.StatusInternalServerError, "internal", "host error: "+err.Error()+" (ref "+ref+")")
	}
}

func writeErr(w http.ResponseWriter, code int, kind, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]string{"code": kind, "message": msg}})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// diskFor is the disk a create request will get (a template's size when none is asked).
func (s *Server) diskFor(req vm.CreateRequest) int {
	if req.DiskGB > 0 || req.ISO != "" {
		return req.DiskGB
	}
	for i, t := range s.templateList(context.Background()) {
		if t.Name == req.Template || (req.Template == "" && i == 0) {
			return t.MinDiskGB
		}
	}
	return 0
}

// newRef is a short id tying an error message to its log line.
func newRef() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) getFirewall(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.vmFor(w, r); !ok {
		return
	}
	fw, err := s.VMs.GetFirewall(r.Context(), r.PathValue("name"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, fw)
}

func (s *Server) putFirewall(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.vmFor(w, r); !ok {
		return
	}
	var body firewall.Settings
	if !decode(w, r, &body) {
		return
	}
	fw, err := s.VMs.SetFirewall(r.Context(), r.PathValue("name"), body)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, fw)
}

func decodeBytes(b []byte, v any) error { return json.Unmarshal(b, v) }

func auditEntry(user, detail string) audit.Entry {
	return audit.Entry{User: user, Action: "billing", Status: 200, Detail: detail}
}

// sshPorts: which port on this server reaches each VM/container's SSH (only
// the caller's own guests for customers).
func (s *Server) sshPorts(w http.ResponseWriter, r *http.Request) {
	out := map[string]int{}
	if s.SSHPorts == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ports": out, "enabled": false})
		return
	}
	all := s.SSHPorts()
	p := who(r)
	mine := map[string]bool{}
	if !p.Admin {
		mine = s.ownedNames(r.Context(), p)
		if s.CTs != nil {
			if l, err := s.CTs.List(r.Context()); err == nil {
				for _, c := range l {
					if c.Owner == p.Name {
						mine[c.Name] = true
					}
				}
			}
		}
	}
	for n, port := range all {
		if p.Admin || mine[n] {
			out[n] = port
		}
	}
	rdp := map[string]int{}
	if s.RDPPorts != nil {
		for n, port := range s.RDPPorts() {
			if p.Admin || mine[n] {
				rdp[n] = port
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ports": out, "rdp": rdp, "enabled": true})
}
