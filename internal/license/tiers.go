// Package license is P8S OS editions and subscriptions: which edition a
// server runs and what that edition unlocks.
//
// A licence key is signed by P8S (Ed25519) and names the servers it covers,
// so p8sd checks it offline. Without a key a server runs as Home (free):
// VMs, containers and Windows VMs, up to 3 user accounts, the testing update channel and a
// "no subscription" notice. Paid tiers never stop running VMs: when a key
// expires (after a 14-day grace period) the server simply falls back to Home.
package license

// Editions, cheapest first (like Home / Pro / Enterprise for Windows). One
// installer for all; the licence key decides the edition.
const (
	Home       = "home"       // free: home, lab, learning
	Standard   = "standard"   // companies running their own VMs and containers
	Enterprise = "enterprise" // public hosting: selling servers; small clusters
	Datacenter = "datacenter" // datacenters, MSPs: unlimited clusters, HA; resell
	Internal   = "internal"   // P8S's own servers and datacenter
)

// Features an edition can unlock. VMs, containers and Windows VMs are in every
// edition, Home included.
const (
	FeatStable   = "stable_updates"    // stable update channel
	FeatUsers    = "unlimited_users"   // more than HomeUsers accounts
	FeatOffsite  = "offsite_backups"   // backups to another server / cloud storage
	FeatSelling  = "selling"           // P8S Cloud selling: public sign-up, priced plans, public IP pools for customers
	FeatBrand    = "own_brand"         // your own brand name on the sign-in page and emails
	FeatCluster  = "cluster"           // several servers in one dashboard (ClusterNodes limits how many)
	FeatHA       = "high_availability" // restart VMs on another server; live migration
	FeatSupport  = "support"           // email support from P8S
	FeatPriority = "priority_support"  // priority support
	FeatResell   = "resell"            // resell P8S OS licences at a discount
	FeatNoNotice = "no_notice"         // no "no subscription" notice
)

// HomeUsers is how many accounts a server without a subscription may have.
const HomeUsers = 3

// TierInfo describes an edition for the dashboard and the licence shop.
type TierInfo struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	For          string   `json:"for"`
	Price        int64    `json:"price"`         // suggested yearly price in paise (0 = free); the shop has the real one
	Per          string   `json:"per"`           // "server", "10 servers"
	Servers      int      `json:"servers"`       // servers per licence (0 = one per purchase)
	ClusterNodes int      `json:"cluster_nodes"` // servers in one cluster (0 = unlimited)
	Features     []string `json:"features"`
	Summary      []string `json:"summary"`
}

var everything = []string{FeatStable, FeatUsers, FeatOffsite, FeatSelling, FeatBrand, FeatCluster, FeatHA,
	FeatSupport, FeatPriority, FeatResell, FeatNoNotice}

var tiers = []TierInfo{
	{ID: Home, Name: "Home", For: "Home, lab and learning", Price: 0, Per: "server", ClusterNodes: 1,
		Features: nil,
		Summary:  []string{"VMs, containers and Windows VMs", "Up to 3 user accounts", "Testing update channel", "Community help"}},
	{ID: Standard, Name: "Standard", For: "Companies running their own servers", Price: 499900, Per: "server", ClusterNodes: 1,
		Features: []string{FeatStable, FeatUsers, FeatOffsite, FeatSupport, FeatNoNotice},
		Summary:  []string{"Everything in Home", "Unlimited user accounts", "Stable update channel", "Off-site backups", "Email support"}},
	{ID: Enterprise, Name: "Enterprise", For: "Hosting providers selling servers", Price: 999900, Per: "server", ClusterNodes: 3,
		Features: []string{FeatStable, FeatUsers, FeatOffsite, FeatSupport, FeatNoNotice, FeatSelling, FeatBrand, FeatCluster, FeatPriority},
		Summary:  []string{"Everything in Standard", "P8S Cloud selling: sign-up, plans, billing, invoices, public IPs", "Your own brand name", "Clusters of up to 3 servers", "Priority support"}},
	{ID: Datacenter, Name: "Datacenter", For: "Datacenters, outsourcing and MSPs", Price: 2499900, Per: "10 servers", Servers: 10, ClusterNodes: 0,
		Features: everything,
		Summary:  []string{"Everything in Enterprise, on up to 10 servers", "Unlimited cluster size", "High availability and live migration", "Resell P8S OS licences at a discount"}},
	{ID: Internal, Name: "Internal", For: "P8S's own servers", Price: 0, Per: "server", ClusterNodes: 0,
		Features: everything,
		Summary:  []string{"Everything, for P8S's own servers and datacenter"}},
}

// Tiers lists the tiers that are sold or free (not Internal).
func Tiers() []TierInfo {
	out := []TierInfo{}
	for _, t := range tiers {
		if t.ID != Internal {
			out = append(out, t)
		}
	}
	return out
}

// Tier returns one tier's description.
func Tier(id string) (TierInfo, bool) {
	for _, t := range tiers {
		if t.ID == id {
			return t, true
		}
	}
	return TierInfo{}, false
}

// Allows reports whether a tier unlocks a feature.
func Allows(tier, feature string) bool {
	t, _ := Tier(tier)
	for _, f := range t.Features {
		if f == feature {
			return true
		}
	}
	return false
}
