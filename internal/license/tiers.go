// Package license is P8S OS subscriptions: which tier a server runs on and
// what that tier unlocks.
//
// A licence key is signed by P8S (Ed25519) and names the servers it covers,
// so p8sd checks it offline. Without a key a server runs as Home (free):
// every VM feature, up to 3 user accounts, the testing update channel and a
// "no subscription" notice. Paid tiers never stop running VMs: when a key
// expires (after a 14-day grace period) the server simply falls back to Home.
package license

// Tiers, cheapest first.
const (
	Home     = "home"     // free: home, lab, learning
	Business = "business" // companies running their own VMs
	Hosting  = "hosting"  // public use: selling servers to customers
	Partner  = "partner"  // outsourcing / MSPs; resell
	Internal = "internal" // P8S's own servers and datacenter
)

// Features a tier can unlock.
const (
	FeatStable   = "stable_updates"   // stable update channel
	FeatUsers    = "unlimited_users"  // more than HomeUsers accounts
	FeatSelling  = "selling"          // P8S Cloud selling: public sign-up, priced plans
	FeatBrand    = "own_brand"        // your own brand name on the sign-in page and emails
	FeatSupport  = "support"          // email support from P8S
	FeatPriority = "priority_support" // priority support
	FeatResell   = "resell"           // resell P8S OS licences at a discount
	FeatNoNotice = "no_notice"        // no "no subscription" notice
)

// HomeUsers is how many accounts a server without a subscription may have.
const HomeUsers = 3

// TierInfo describes a tier for the dashboard and the licence shop.
type TierInfo struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	For      string   `json:"for"`
	Price    int64    `json:"price"`   // suggested yearly price in paise (0 = free); the shop has the real one
	Per      string   `json:"per"`     // "server", "10 servers"
	Servers  int      `json:"servers"` // servers per licence (0 = one per purchase)
	Features []string `json:"features"`
	Summary  []string `json:"summary"`
}

var tiers = []TierInfo{
	{ID: Home, Name: "Home", For: "Home, lab and learning", Price: 0, Per: "server",
		Features: nil,
		Summary:  []string{"Every VM feature", "Up to 3 user accounts", "Testing update channel", "Community help"}},
	{ID: Business, Name: "Business", For: "Companies running their own VMs", Price: 499900, Per: "server",
		Features: []string{FeatStable, FeatUsers, FeatSupport, FeatNoNotice},
		Summary:  []string{"Everything in Home", "Unlimited user accounts", "Stable update channel", "Email support"}},
	{ID: Hosting, Name: "Hosting", For: "Public use: selling servers to customers", Price: 999900, Per: "server",
		Features: []string{FeatStable, FeatUsers, FeatSupport, FeatNoNotice, FeatSelling, FeatBrand},
		Summary:  []string{"Everything in Business", "P8S Cloud selling: sign-up, plans, billing, invoices", "Your own brand name"}},
	{ID: Partner, Name: "Partner", For: "Outsourcing and MSPs", Price: 2499900, Per: "10 servers", Servers: 10,
		Features: []string{FeatStable, FeatUsers, FeatSupport, FeatNoNotice, FeatSelling, FeatBrand, FeatPriority, FeatResell},
		Summary:  []string{"Everything in Hosting, on up to 10 servers", "Priority support", "Resell P8S OS licences at a discount"}},
	{ID: Internal, Name: "Internal", For: "P8S's own servers", Price: 0, Per: "server",
		Features: []string{FeatStable, FeatUsers, FeatSupport, FeatNoNotice, FeatSelling, FeatBrand, FeatPriority, FeatResell},
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
