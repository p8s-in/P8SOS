package diskmgr

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const lsblk = `{"blockdevices":[
 {"name":"sda","path":"/dev/sda","type":"disk","size":1000204886016,"model":"ST1000LM035","serial":"WL1","tran":"sata","rota":true,"rm":false,"fstype":null,"label":null,"mountpoints":[null],
  "children":[{"name":"sda1","path":"/dev/sda1","type":"part","size":1000,"fstype":"ntfs","label":"DATA","mountpoints":[null]}]},
 {"name":"nvme0n1","path":"/dev/nvme0n1","type":"disk","size":256060514304,"model":"WD SN520","serial":"N1","tran":"nvme","rota":false,"rm":false,"fstype":null,"mountpoints":[null],
  "children":[{"name":"nvme0n1p1","path":"/dev/nvme0n1p1","type":"part","size":1,"fstype":"vfat","mountpoints":["/boot/efi"]},
              {"name":"nvme0n1p3","path":"/dev/nvme0n1p3","type":"part","size":1,"fstype":"zfs_member","label":"rpool","mountpoints":[null]}]},
 {"name":"sdb","path":"/dev/sdb","type":"disk","size":500107862016,"model":"Samsung 870","serial":"S2","tran":"sata","rota":false,"rm":false,"fstype":null,"mountpoints":[null]},
 {"name":"sdc","path":"/dev/sdc","type":"disk","size":500107862016,"model":"Samsung 870","serial":"S3","tran":"sata","rota":false,"rm":false,"fstype":null,"mountpoints":[null]},
 {"name":"sdd","path":"/dev/sdd","type":"disk","size":500107862016,"model":"Old","serial":"S4","tran":"sata","rota":true,"rm":false,"fstype":null,"mountpoints":[null],
  "children":[{"name":"sdd1","path":"/dev/sdd1","type":"part","size":1,"fstype":"zfs_member","label":"tank","mountpoints":[null]}]},
 {"name":"sde","path":"/dev/sde","type":"disk","size":500107862016,"model":"Ceph","serial":"S5","tran":"sata","rota":true,"rm":false,"fstype":"LVM2_member","mountpoints":[null],
  "children":[{"name":"ceph--x","path":"/dev/mapper/ceph--x","type":"lvm","size":1,"fstype":"ceph_bluestore","mountpoints":[null]}]},
 {"name":"sdf","path":"/dev/sdf","type":"disk","size":32000000000,"model":"Stick","serial":"U1","tran":"usb","rota":false,"rm":true,"fstype":null,"mountpoints":[null]}
]}`

const zstatus = `  pool: rpool
 state: ONLINE
config:

	NAME                         STATE     READ WRITE CKSUM
	rpool                        ONLINE       0     0     0
	  /dev/disk/by-id/nvme-WD-part3  ONLINE   0     0     0

  pool: tank
 state: ONLINE
config:
	NAME          STATE     READ WRITE CKSUM
	tank          ONLINE       0     0     0
	  /dev/sdd1   ONLINE       0     0     0
`

type fake struct {
	cmds []string
	vols string
}

func (f *fake) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	c := name + " " + strings.Join(args, " ")
	f.cmds = append(f.cmds, c)
	switch {
	case name == "lsblk" && args[0] == "-J":
		return []byte(lsblk), nil
	case c == "zpool status -P":
		return []byte(zstatus), nil
	case strings.HasPrefix(c, "lsblk -lnpo NAME,TYPE /dev/sda"):
		return []byte("/dev/sda disk\n/dev/sda1 part\n"), nil
	case strings.HasPrefix(c, "smartctl"):
		if strings.HasSuffix(c, "/dev/sdb") {
			return []byte(`{"smart_status":{"passed":true},"temperature":{"current":34},"power_on_time":{"hours":1200},"ata_smart_attributes":{"table":[{"id":5,"raw":{"value":2}},{"id":197,"raw":{"value":0}}]}}`), nil
		}
		return []byte(`{"smartctl":{"messages":[{"string":"Unable to detect device type"}]}}`), errors.New("exit 1")
	case strings.HasPrefix(c, "zfs list -H -o name -t volume -r p8s-fast/vms"):
		return []byte(f.vols), nil
	case strings.HasPrefix(c, "zpool list -H -o name p8s-fast"):
		return []byte("p8s-fast\n"), nil
	case strings.HasPrefix(c, "zpool list -Hp"):
		return []byte("1000 900 ONLINE\n"), nil
	}
	return nil, nil
}

func TestDisks(t *testing.T) {
	ctx := context.Background()
	f := &fake{}
	m := &Manager{Run: f, DataDir: t.TempDir(), ByID: func() map[string]string {
		return map[string]string{"/dev/nvme0n1": "/dev/disk/by-id/nvme-WD", "/dev/sdb": "/dev/disk/by-id/ata-S2"}
	}, Resolve: func(p string) (string, error) {
		if p == "/dev/disk/by-id/nvme-WD-part3" {
			return "/dev/nvme0n1p3", nil
		}
		return p, nil
	}, Taken: func() []string { return []string{"san1"} }}
	l, err := m.List(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Disk{}
	for _, d := range l {
		got[d.Name] = d
	}
	for name, want := range map[string]string{"sda": "data", "nvme0n1": "system", "sdb": "empty", "sdd": "ZFS pool tank", "sde": "LVM / Ceph", "sdf": "empty"} {
		if got[name].Usage != want {
			t.Errorf("%s: %q, want %q", name, got[name].Usage, want)
		}
	}
	if s := got["sdb"].SMART; s == nil || !s.Available || !s.Passed || s.TempC != 34 || s.Reallocated != 2 {
		t.Fatalf("smart %+v", s)
	}
	if s := got["sda"].SMART; s == nil || s.Available || s.Note == "" {
		t.Fatalf("no smart %+v", s)
	}
	// wiping: the system, a pool member, Ceph's disk: never
	for _, n := range []string{"nvme0n1", "sdd", "sde"} {
		if err := m.Wipe(ctx, n, n); !errors.Is(err, ErrRefused) {
			t.Errorf("wipe %s: %v", n, err)
		}
	}
	// a disk with data: only with its name typed
	if err := m.Wipe(ctx, "sda", ""); !errors.Is(err, ErrConfirm) {
		t.Fatal(err)
	}
	// protected: not even with the name
	if err := m.Protect(ctx, "sda", true); err != nil {
		t.Fatal(err)
	}
	if err := m.Wipe(ctx, "sda", "sda"); !errors.Is(err, ErrRefused) {
		t.Fatalf("protected wiped: %v", err)
	}
	if d, _ := m.find(ctx, "sda"); !d.Protected {
		t.Fatal("not protected")
	}
	for _, c := range f.cmds {
		if strings.HasPrefix(c, "wipefs") {
			t.Fatalf("wiped something: %s", c)
		}
	}
	m.Protect(ctx, "sda", false)
	if err := m.Wipe(ctx, "sda", "sda"); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(f.cmds, "\n")
	if !strings.Contains(all, "wipefs -af /dev/sda1") || !strings.Contains(all, "wipefs -af /dev/sda\n") {
		t.Fatalf("%s", all)
	}

	// pools
	if _, err := m.CreatePool(ctx, "fast", "mirror", []string{"sdb"}); !errors.Is(err, ErrInvalid) {
		t.Fatal("mirror of one", err)
	}
	if _, err := m.CreatePool(ctx, "san1", "single", []string{"sdb"}); !errors.Is(err, ErrInvalid) {
		t.Fatal("taken name", err)
	}
	if _, err := m.CreatePool(ctx, "fast", "mirror", []string{"sdb", "sda"}); !errors.Is(err, ErrRefused) {
		t.Fatal("disk with data", err)
	}
	if _, err := m.CreatePool(ctx, "fast", "mirror", []string{"sdb", "sdf"}); !errors.Is(err, ErrRefused) {
		t.Fatal("usb", err)
	}
	p, err := m.CreatePool(ctx, "fast", "mirror", []string{"sdb", "sdc"})
	if err != nil {
		t.Fatal(err)
	}
	all = strings.Join(f.cmds, "\n")
	if !strings.Contains(all, "zpool create -o ashift=12 -O compression=lz4 -O atime=off -O xattr=sa -m none p8s-fast mirror /dev/disk/by-id/ata-S2 /dev/sdc") ||
		!strings.Contains(all, "zfs create -o canmount=off p8s-fast/vms") || p.Pool != "p8s-fast" {
		t.Fatalf("%s", all)
	}
	if v := m.VMPools(); v["fast"] != "p8s-fast/vms" {
		t.Fatal(v)
	}
	if _, err := m.CreatePool(ctx, "fast", "single", []string{"sdf"}); !errors.Is(err, ErrInvalid) {
		t.Fatal("same name twice")
	}
	info := m.PoolsInfo(ctx)
	if len(info) != 1 || info[0].Health != "ONLINE" || info[0].Size != 1000 {
		t.Fatalf("%+v", info)
	}
	f.vols = "p8s-fast/vms/web1-disk0\n"
	if err := m.DestroyPool(ctx, "fast", "fast"); !errors.Is(err, ErrRefused) {
		t.Fatal("destroyed with VM disks", err)
	}
	f.vols = ""
	if err := m.DestroyPool(ctx, "fast", ""); !errors.Is(err, ErrConfirm) {
		t.Fatal(err)
	}
	if err := m.DestroyPool(ctx, "fast", "fast"); err != nil || len(m.Pools()) != 0 {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(f.cmds, "\n"), "zpool destroy p8s-fast") {
		t.Fatal("not destroyed")
	}
}
