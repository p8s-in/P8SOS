package imports

import (
	"archive/tar"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const ovf = `<?xml version="1.0" encoding="UTF-8"?>
<Envelope vmw:buildId="build-1" xmlns="http://schemas.dmtf.org/ovf/envelope/1" xmlns:ovf="http://schemas.dmtf.org/ovf/envelope/1"
 xmlns:rasd="http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/CIM_ResourceAllocationSettingData" xmlns:vmw="http://www.vmware.com/schema/ovf">
  <References>
    <File ovf:href="Web Server-disk1.vmdk" ovf:id="file1" ovf:size="1048576"/>
    <File ovf:href="Web Server-disk2.vmdk" ovf:id="file2" ovf:size="524288"/>
  </References>
  <DiskSection>
    <Disk ovf:capacity="20" ovf:capacityAllocationUnits="byte * 2^30" ovf:diskId="vmdisk1" ovf:fileRef="file1" ovf:format="http://www.vmware.com/interfaces/specifications/vmdk.html#streamOptimized"/>
    <Disk ovf:capacity="5368709120" ovf:diskId="vmdisk2" ovf:fileRef="file2"/>
  </DiskSection>
  <VirtualSystem ovf:id="Web Server">
    <Name>Web Server</Name>
    <OperatingSystemSection ovf:id="101" vmw:osType="ubuntu64Guest"><Description>Ubuntu Linux (64-bit)</Description></OperatingSystemSection>
    <VirtualHardwareSection>
      <Item><rasd:AllocationUnits>hertz * 10^6</rasd:AllocationUnits><rasd:ResourceType>3</rasd:ResourceType><rasd:VirtualQuantity>4</rasd:VirtualQuantity></Item>
      <Item><rasd:AllocationUnits>byte * 2^20</rasd:AllocationUnits><rasd:ResourceType>4</rasd:ResourceType><rasd:VirtualQuantity>8192</rasd:VirtualQuantity></Item>
      <Item><rasd:ResourceSubType>VmxNet3</rasd:ResourceSubType><rasd:ResourceType>10</rasd:ResourceType></Item>
      <vmw:Config ovf:required="false" vmw:key="firmware" vmw:value="efi"/>
    </VirtualHardwareSection>
  </VirtualSystem>
</Envelope>`

func TestPlanFromOVF(t *testing.T) {
	p, err := PlanFromOVF([]byte(ovf))
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "web-server" || p.VCPUs != 4 || p.MemoryMiB != 8192 || p.NICModel != "vmxnet3" || p.Firmware != "uefi" || p.OSType != "linux" {
		t.Fatalf("%+v", p)
	}
	if len(p.Disks) != 2 || p.Disks[0].Bytes != 20<<30 || p.Disks[1].Bytes != 5<<30 || p.DiskGB != 20 || p.Disks[1].Href != "Web Server-disk2.vmdk" {
		t.Fatalf("disks %+v %d", p.Disks, p.DiskGB)
	}
	w, _ := PlanFromOVF([]byte(strings.ReplaceAll(strings.ReplaceAll(ovf, "ubuntu64Guest", "windows2019srv_64Guest"), "Ubuntu Linux (64-bit)", "Microsoft Windows Server 2019")))
	if w.OSType != "windows" || w.DiskBus != "sata" {
		t.Fatalf("windows %+v", w)
	}
}

func TestOVAInspectAndExtract(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "web.ova")
	f, _ := os.Create(file)
	tw := tar.NewWriter(f)
	add := func(name string, b []byte) {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg})
		tw.Write(b)
	}
	add("Web Server.ovf", []byte(ovf))
	add("Web Server-disk1.vmdk", []byte("vmdk-one"))
	add("../../evil/Web Server-disk2.vmdk", []byte("vmdk-two")) // a path that tries to climb out
	tw.Close()
	f.Close()
	if KindOf("web.ova") != KindOVA {
		t.Fatal("kind")
	}
	p, err := Inspect(file)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "unpack")
	files, err := ExtractOVA(context.Background(), file, p, out, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(files["Web Server-disk2.vmdk"]); string(b) != "vmdk-two" || filepath.Dir(files["Web Server-disk2.vmdk"]) != out {
		t.Fatalf("extracted %v", files)
	}
	if _, err := os.Stat(filepath.Join(dir, "evil")); err == nil {
		t.Fatal("wrote outside the folder")
	}
}
