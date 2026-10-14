package browse

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestParseLsblk(t *testing.T) {
	out := []byte(`{"blockdevices":[{"path":"/dev/zd16","fstype":null,"label":null,"size":"20G","type":"disk","children":[
	 {"path":"/dev/zd16p1","fstype":"ext4","label":"root","size":"19.9G","type":"part"},
	 {"path":"/dev/zd16p14","fstype":null,"label":null,"size":"3M","type":"part"},
	 {"path":"/dev/zd16p15","fstype":"vfat","label":"UEFI","size":"124M","type":"part"},
	 {"path":"/dev/zd16p5","fstype":"LVM2_member","label":null,"size":"1G","type":"part"},
	 {"path":"/dev/zd16p6","fstype":"swap","label":null,"size":"1G","type":"part"}]}]}`)
	p, err := ParseLsblk(out, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(p) != 3 || p[0].ID != "d0p1" || p[0].FS != "ext4" || p[0].dev != "/dev/zd16p1" || p[1].FS != "vfat" || p[2].Error == "" {
		t.Fatalf("%+v", p)
	}
	whole, _ := ParseLsblk([]byte(`{"blockdevices":[{"path":"/dev/loop3","fstype":"xfs","label":"data","size":"10G","type":"loop"}]}`), 1)
	if len(whole) != 1 || whole[0].ID != "d1p0" {
		t.Fatalf("%+v", whole)
	}
}

func TestBrowseStaysInside(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("host file"), 0o600)
	os.MkdirAll(filepath.Join(root, "etc"), 0o755)
	os.WriteFile(filepath.Join(root, "etc", "hostname"), []byte("web1\n"), 0o644)
	os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "evil"))
	os.Symlink("../../../../../../"+outside, filepath.Join(root, "up"))
	m := &Manager{s: map[string]*Session{"s1": {ID: "s1", State: "ready", Parts: []Part{{ID: "d0p1", FS: "ext4", mount: root}}}}}
	l, err := m.Ls("s1", "d0p1", "/")
	if err != nil || len(l) != 3 || l[0].Name != "etc" || !l[0].Dir {
		t.Fatalf("ls %v %+v", err, l)
	}
	f, st, err := m.OpenFile("s1", "d0p1", "/etc/../etc/hostname")
	if err != nil || st.Size() != 5 {
		t.Fatalf("open %v", err)
	}
	b, _ := io.ReadAll(f)
	f.Close()
	if string(b) != "web1\n" {
		t.Fatalf("%q", b)
	}
	for _, bad := range []string{"evil", "up/secret", "../../" + outside + "/secret"} {
		if _, _, err := m.OpenFile("s1", "d0p1", bad); err == nil {
			t.Errorf("%s escaped the backup's filesystem", bad)
		}
	}
	if _, err := m.Ls("s1", "d0p1", "up"); err == nil {
		t.Error("listing through a symlink out of it")
	}
	if _, err := m.Ls("nope", "d0p1", "/"); !errors.Is(err, ErrNotFound) {
		t.Fatal("unknown session")
	}
	m.s["s1"].State = "preparing"
	if _, err := m.Ls("s1", "d0p1", "/"); !errors.Is(err, ErrBusy) {
		t.Fatal("not ready yet")
	}
}
