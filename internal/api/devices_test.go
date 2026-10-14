package api

import (
	"context"
	"testing"

	"p8sos/internal/virt"
	"p8sos/internal/vm"
)

type devVMs struct {
	*ownedVMs
	added, removed []string
}

func (d *devVMs) Devices(_ context.Context, name string) (vm.Devices, error) {
	return vm.Devices{PCI: []string{"0000:01:00.0"}}, nil
}
func (d *devVMs) AddDevice(_ context.Context, name, kind, id string) (vm.VM, string, error) {
	d.added = append(d.added, name+" "+kind+" "+id)
	return d.vms[name], "given", nil
}
func (d *devVMs) RemoveDevice(_ context.Context, name, kind, id string) (vm.VM, error) {
	d.removed = append(d.removed, name+" "+kind+" "+id)
	return d.vms[name], nil
}

func TestDevicesAPI(t *testing.T) {
	m := newMultiUserCfg(t, nil)
	dv := &devVMs{ownedVMs: m.vms}
	m.server.VMs = dv
	m.req("POST", "/api/v1/users", secret, `{"name":"ops","role":"operator","password":"operator-pass-1"}`, 201)
	ops := m.login("ops", "operator-pass-1", "")
	m.req("GET", "/api/v1/vms/web1/devices", ops, "", 200)                                    // staff may look
	m.req("POST", "/api/v1/vms/web1/devices", ops, `{"kind":"pci","id":"0000:01:00.0"}`, 403) // but only admins give hardware away
	if v := m.req("POST", "/api/v1/vms/web1/devices", secret, `{"kind":"pci","id":"0000:01:00.0"}`, 201); v["note"] != "given" {
		t.Fatalf("add %v", v)
	}
	m.req("DELETE", "/api/v1/vms/web1/devices/usb/046d:c52b", secret, "", 200)
	if len(dv.added) != 1 || dv.added[0] != "web1 pci 0000:01:00.0" || len(dv.removed) != 1 || dv.removed[0] != "web1 usb 046d:c52b" {
		t.Fatalf("%v %v", dv.added, dv.removed)
	}
}

type limVMs struct {
	*ownedVMs
	got []virt.IOLimits
}

func (l *limVMs) SetIOLimits(_ context.Context, name string, x virt.IOLimits) (vm.VM, error) {
	l.got = append(l.got, x)
	return l.vms[name], nil
}

func TestLimitsAPI(t *testing.T) {
	m := newMultiUserCfg(t, nil)
	lv := &limVMs{ownedVMs: m.vms}
	m.server.VMs = lv
	m.req("POST", "/api/v1/users", secret, `{"name":"alice","password":"alice-password"}`, 201)
	alice := m.login("alice", "alice-password", "")
	m.req("PUT", "/api/v1/vms/shop/limits", alice, `{}`, 403) // customers can't lift their own limits
	m.req("PUT", "/api/v1/vms/web1/limits", secret, `{"write_mbs":100,"net_up_mbit":50}`, 200)
	if len(lv.got) != 1 || lv.got[0].WriteMBs != 100 || lv.got[0].NetUpMbit != 50 {
		t.Fatalf("%+v", lv.got)
	}
}
