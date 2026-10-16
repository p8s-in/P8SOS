package ippool

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Cluster: the master's pools are the cluster's. On a worker the Store
// follows the master (Leader): it reads the master's pools file (pushed to
// every server with the cluster's settings) and asks the master for every
// change, so one address is never handed out twice and VMs and containers
// on any server can have public addresses. Pools and leases the worker had
// before it joined stay usable (they are kept in its own file).

// Op kinds.
const (
	OpAllocate    = "allocate"
	OpRelease     = "release"
	OpAddExtra    = "add-extra"
	OpRemoveExtra = "remove-extra"
	OpAddV6       = "add-v6"
	OpChange      = "change"
)

// Op is one change asked of the master's pools.
type Op struct {
	Kind string `json:"kind"`
	Pool string `json:"pool,omitempty"`
	VM   string `json:"vm"`
	IP   string `json:"ip,omitempty"` // prefer / want / the extra address
}

// Result is what the master answers: the lease after the change (Old: before
// a Change), its pools file now, and the error if the change was refused.
type Result struct {
	Lease   Lease  `json:"lease"`
	Old     Lease  `json:"old"`
	File    []byte `json:"file,omitempty"`
	Error   string `json:"error,omitempty"`
	ErrKind string `json:"err_kind,omitempty"`
}

// Leader is the master's side as a worker sees it (cluster.PoolsLeader).
type Leader interface {
	Following() bool              // this server is a cluster worker now
	MasterPools() ([]byte, error) // the master's pools file as last received (nil: none yet)
	Apply(op Op) (Result, error)  // run op on the master
}

// ErrNotMaster: pools are managed on the master.
var ErrNotMaster = fmt.Errorf("%w: this server is a cluster worker: its public IP pools are the master's (manage them on the master's dashboard)", ErrInvalid)

var errKinds = map[string]error{"invalid": ErrInvalid, "not_found": ErrNotFound, "exists": ErrExists, "full": ErrFull, "in_use": ErrInUse}

// ErrKindOf names err for the wire ("" = other).
func ErrKindOf(err error) string {
	for k, e := range errKinds {
		if errors.Is(err, e) {
			return k
		}
	}
	return ""
}

// ResultErr turns a refused Result back into the error.
func ResultErr(r Result) error {
	if r.Error == "" {
		return nil
	}
	if e, ok := errKinds[r.ErrKind]; ok {
		return fmt.Errorf("%w (on the master: %s)", e, r.Error)
	}
	return fmt.Errorf("on the master: %s", r.Error)
}

func (s *Store) following() Leader {
	if s.Leader != nil && s.Leader.Following() {
		return s.Leader
	}
	return nil
}

func parse(b []byte) ([]Pool, error) {
	var l []Pool
	if len(b) == 0 {
		return []Pool{}, nil
	}
	if err := json.Unmarshal(b, &l); err != nil {
		return nil, err
	}
	for i := range l {
		fill(&l[i])
	}
	return l, nil
}

// master is the master's pools (nil, false: not following, or nothing received yet).
func (s *Store) master() ([]Pool, bool) {
	f := s.following()
	if f == nil {
		return nil, false
	}
	b, err := f.MasterPools()
	if err != nil || b == nil {
		return nil, false
	}
	l, err := parse(b)
	if err != nil {
		return nil, false
	}
	return l, true
}

// read is the pools to look things up in: the master's, with what this
// server had before it joined (caller holds mu).
func (s *Store) read() ([]Pool, error) {
	local, err := s.load()
	m, ok := s.master()
	if !ok {
		return local, err
	}
	at := map[string]int{}
	has := map[string]bool{} // VMs with a master lease
	for i, p := range m {
		at[p.Name] = i
		for _, v := range p.Used {
			has[v] = true
		}
	}
	for _, p := range local {
		i, same := at[p.Name]
		if !same {
			m = append(m, p)
			continue
		}
		for ip, v := range p.Used {
			if !has[v] && !taken(m[i], ip) {
				m[i].Used[ip] = v
				for x, w := range p.Extra {
					if w == v && !taken(m[i], x) {
						m[i].Extra[x] = v
					}
				}
				for x, w := range p.Used6 {
					if _, t := m[i].Used6[x]; w == v && !t {
						m[i].Used6[x] = v
					}
				}
			}
		}
	}
	return m, nil
}

// remote: the change for vm (and pool, for a new address) goes to the master
// (caller holds mu). VMs with a lease from before the cluster keep it here.
func (s *Store) remote(vm, pool string) Leader {
	f := s.following()
	if f == nil {
		return nil
	}
	if local, err := s.load(); err == nil {
		if i, _ := find(local, vm); i >= 0 {
			if m, ok := s.master(); !ok || func() bool { j, _ := find(m, vm); return j < 0 }() {
				return nil
			}
		}
		if pool != "" {
			m, _ := s.master()
			inMaster := false
			for _, p := range m {
				inMaster = inMaster || p.Name == pool
			}
			for _, p := range local {
				if p.Name == pool && !inMaster {
					return nil // a pool only this server has
				}
			}
		}
	}
	return f
}

// masterPool: pool is one of the master's (caller holds mu).
func (s *Store) masterPool(name string) bool {
	m, _ := s.master()
	for _, p := range m {
		if p.Name == name {
			return true
		}
	}
	return false
}

func apply(f Leader, op Op) (Result, error) {
	r, err := f.Apply(op)
	if err != nil {
		return Result{}, err
	}
	return r, ResultErr(r)
}

// Do runs a worker's change on this server's pools (the master's side).
func (s *Store) Do(op Op) Result {
	if s.following() != nil {
		return Result{Error: ErrNotMaster.Error(), ErrKind: "invalid"}
	}
	var r Result
	var err error
	switch op.Kind {
	case OpAllocate:
		r.Lease, err = s.Allocate(op.Pool, op.VM, op.IP)
	case OpRelease:
		err = s.Release(op.VM)
	case OpAddExtra:
		r.Lease, err = s.AddExtra(op.VM, op.IP)
	case OpRemoveExtra:
		r.Lease, err = s.RemoveExtra(op.VM, op.IP)
	case OpAddV6:
		r.Lease, err = s.AddV6(op.VM)
	case OpChange:
		r.Old, r.Lease, err = s.Change(op.VM, op.Pool, op.IP)
	default:
		err = fmt.Errorf("%w: pool change %q", ErrInvalid, op.Kind)
	}
	if err != nil {
		r.Error, r.ErrKind = err.Error(), ErrKindOf(err)
	}
	s.mu.Lock()
	if l, lerr := s.load(); lerr == nil {
		r.File, _ = json.Marshal(l)
	}
	s.mu.Unlock()
	return r
}

// Adopt copies the master's leases of the VMs and containers this server
// has (keep) into its own file, with their pools: a server that leaves the
// cluster keeps its addresses (the master keeps them marked used, so they
// aren't handed out twice). It returns how many VMs were adopted.
func (s *Store) Adopt(keep func(vm string) bool) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.master()
	if !ok {
		return 0, nil
	}
	local, err := s.load()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range m {
		mine := map[string]bool{}
		for _, v := range p.Used {
			if keep(v) {
				mine[v] = true
			}
		}
		if len(mine) == 0 {
			continue
		}
		i := -1
		for k := range local {
			if local[k].Name == p.Name {
				i = k
			}
		}
		if i < 0 {
			np := p
			np.Used, np.Extra, np.Used6 = map[string]string{}, map[string]string{}, map[string]string{}
			np.DNS = append([]string(nil), p.DNS...)
			np.Exclude = append([]string(nil), p.Exclude...)
			local = append(local, np)
			i = len(local) - 1
		}
		for _, pair := range [][2]map[string]string{{p.Used, local[i].Used}, {p.Extra, local[i].Extra}, {p.Used6, local[i].Used6}} {
			for ip, v := range pair[0] {
				if _, t := pair[1][ip]; mine[v] && !t {
					pair[1][ip] = v
				}
			}
		}
		n += len(mine)
	}
	if n == 0 {
		return 0, nil
	}
	return n, s.save(local)
}
