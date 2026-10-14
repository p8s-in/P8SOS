package api

import (
	"context"
	"net/http"

	"p8sos/internal/users"
	"p8sos/internal/virt"
	"p8sos/internal/vm"
)

// PCI / USB passthrough (Hardware tab):
// GET /vms/{name}/devices → what the VM has and what its server offers (staff);
// POST /vms/{name}/devices {"kind":"pci"|"usb","id":"0000:01:00.0"|"046d:c52b"};
// DELETE /vms/{name}/devices/{kind}/{id} (admins only: it takes hardware
// away from the server).

// DeviceVMs is the passthrough part of the VM service (local or cluster).
type DeviceVMs interface {
	Devices(ctx context.Context, name string) (vm.Devices, error)
	AddDevice(ctx context.Context, name, kind, id string) (vm.VM, string, error)
	RemoveDevice(ctx context.Context, name, kind, id string) (vm.VM, error)
}

func (s *Server) devicesAPI(w http.ResponseWriter, r *http.Request, change bool) (DeviceVMs, vm.VM, bool) {
	p := who(r)
	if !p.Admin || (change && p.Role != users.Admin) {
		writeErr(w, http.StatusForbidden, "forbidden", "only an admin can give the server's PCI/USB devices to VMs")
		return nil, vm.VM{}, false
	}
	cur, ok := s.vmFor(w, r)
	if !ok {
		return nil, vm.VM{}, false
	}
	d, ok := s.VMs.(DeviceVMs)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "passthrough isn't available here")
	}
	return d, cur, ok
}

func (s *Server) getDevices(w http.ResponseWriter, r *http.Request) {
	d, cur, ok := s.devicesAPI(w, r, false)
	if !ok {
		return
	}
	out, err := d.Devices(r.Context(), cur.Name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) addDevice(w http.ResponseWriter, r *http.Request) {
	d, cur, ok := s.devicesAPI(w, r, true)
	if !ok {
		return
	}
	var req struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	}
	if !decode(w, r, &req) {
		return
	}
	v, note, err := d.AddDevice(r.Context(), cur.Name, req.Kind, req.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, VMWithNote{v, note})
}

func (s *Server) removeDevice(w http.ResponseWriter, r *http.Request) {
	d, cur, ok := s.devicesAPI(w, r, true)
	if !ok {
		return
	}
	v, err := d.RemoveDevice(r.Context(), cur.Name, r.PathValue("kind"), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// Disk / network speed limits (staff): PUT /vms/{name}/limits
// {"read_mbs":200,"write_mbs":100,"iops":5000,"net_down_mbit":100,"net_up_mbit":50} (0 = no limit).

// LimitVMs is the speed-limit part of the VM service (local or cluster).
type LimitVMs interface {
	SetIOLimits(ctx context.Context, name string, l virt.IOLimits) (vm.VM, error)
}

func (s *Server) setLimits(w http.ResponseWriter, r *http.Request) {
	if !who(r).Admin {
		writeErr(w, http.StatusForbidden, "forbidden", "only an admin or operator can change a VM's speed limits")
		return
	}
	cur, ok := s.vmFor(w, r)
	if !ok {
		return
	}
	lv, ok := s.VMs.(LimitVMs)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "speed limits aren't available here")
		return
	}
	var l virt.IOLimits
	if !decode(w, r, &l) {
		return
	}
	v, err := lv.SetIOLimits(r.Context(), cur.Name, l)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// CPU / RAM hot-plug (staff): POST /vms/{name}/hotplug {"on":true,"max_vcpus":8,"max_mib":16384}
// (0 = defaults: twice the current size). The VM must be shut down.

// HotplugVMs is the hot-plug part of the VM service (local or cluster).
type HotplugVMs interface {
	SetHotplug(ctx context.Context, name string, on bool, maxVCPUs, maxMiB int) (vm.VM, error)
}

func (s *Server) setHotplug(w http.ResponseWriter, r *http.Request) {
	if !who(r).Admin {
		writeErr(w, http.StatusForbidden, "forbidden", "only an admin or operator can turn hot-plug on or off")
		return
	}
	cur, ok := s.vmFor(w, r)
	if !ok {
		return
	}
	hv, ok := s.VMs.(HotplugVMs)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "hot-plug isn't available here")
		return
	}
	var req struct {
		On       bool `json:"on"`
		MaxVCPUs int  `json:"max_vcpus"`
		MaxMiB   int  `json:"max_mib"`
	}
	if !decode(w, r, &req) {
		return
	}
	v, err := hv.SetHotplug(r.Context(), cur.Name, req.On, req.MaxVCPUs, req.MaxMiB)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// Move a VM's disks to another storage on its server (staff):
// POST /vms/{name}/storage {"storage":"ceph","drop_snapshots":true} → job (202).

// StorageVMs is the disk-move part of the VM service (local or cluster).
type StorageVMs interface {
	MoveStorage(ctx context.Context, name, target string, dropSnaps bool) (vm.Job, error)
}

func (s *Server) moveStorage(w http.ResponseWriter, r *http.Request) {
	if !who(r).Admin {
		writeErr(w, http.StatusForbidden, "forbidden", "only an admin or operator can move a VM's disks")
		return
	}
	cur, ok := s.vmFor(w, r)
	if !ok {
		return
	}
	sv, ok := s.VMs.(StorageVMs)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "moving disks isn't available here")
		return
	}
	var req struct {
		Storage   string `json:"storage"`
		DropSnaps bool   `json:"drop_snapshots"`
	}
	if !decode(w, r, &req) {
		return
	}
	j, err := sv.MoveStorage(r.Context(), cur.Name, req.Storage, req.DropSnaps)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.noteJob(r, j.ID)
	w.Header().Set("Location", "/api/v1/jobs/"+j.ID)
	writeJSON(w, http.StatusAccepted, j)
}
