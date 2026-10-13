package vm

import (
	"fmt"
	"slices"
)

// Where a VM's disks live: "local" (this server's ZFS pool) or a network
// storage (a ZFS pool on an iSCSI disk, see package storages).

type diskPlacer interface {
	Place(vm, storage string) error
	StorageOf(vm string) string
	Storages() []string
}

// Storages lists the storages new VMs can be put on.
func (s *Service) Storages() []string {
	if p, ok := s.Disks.(diskPlacer); ok {
		return p.Storages()
	}
	return []string{"local"}
}

func (s *Service) checkStorage(name string) string {
	if name == "" || name == "local" {
		return ""
	}
	if !slices.Contains(s.Storages(), name) {
		return fmt.Sprintf("storage %q isn't on this server (have: %v)", name, s.Storages())
	}
	return ""
}

func (s *Service) place(dom, storage string) error {
	p, ok := s.Disks.(diskPlacer)
	if !ok {
		if storage == "" || storage == "local" {
			return nil
		}
		return fmt.Errorf("%w: this server has only local storage", ErrInvalid)
	}
	return p.Place(dom, storage)
}

func (s *Service) storageOf(dom string) string {
	if p, ok := s.Disks.(diskPlacer); ok {
		if st := p.StorageOf(dom); st != "local" {
			return st
		}
	}
	return ""
}
