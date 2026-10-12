package metrics

import (
	"context"
	"testing"
	"time"
)

const sample = `Domain: 'p8s-web1'
  cpu.time=10000000000
  cpu.user=1
  balloon.current=2097152
  balloon.rss=1048576
  vcpu.current=2
  vcpu.maximum=2
  block.count=2
  block.0.name=vda
  block.0.rd.bytes=1000
  block.0.wr.bytes=5000
  block.1.name=sda
  block.1.rd.bytes=24
  block.1.wr.bytes=0
  net.count=1
  net.0.rx.bytes=700
  net.0.tx.bytes=300

Domain: 'p8s-db1'
  cpu.time=1
`

func TestParseDomStats(t *testing.T) {
	m := ParseDomStats(sample)
	w := m["p8s-web1"]
	if len(m) != 2 || w.CPUTimeNs != 1e10 || w.VCPUs != 2 || w.RSSKiB != 1048576 || w.RdBytes != 1024 || w.WrBytes != 5000 || w.RxBytes != 700 || w.TxBytes != 300 {
		t.Fatalf("%+v", m)
	}
}

func TestCollectorRates(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	cur := map[string]Raw{"web1": {CPUTimeNs: 0, VCPUs: 2, RSSKiB: 512 * 1024}}
	busy, total := uint64(100), uint64(1000)
	c := &Collector{
		Now:     func() time.Time { return now },
		Stats:   func(context.Context) (map[string]Raw, error) { return cur, nil },
		HostCPU: func() (uint64, uint64, bool) { return busy, total, true },
		HostMem: func() (int, int) { return 3000, 8000 },
		Window:  time.Minute,
	}
	ctx := context.Background()
	c.Sample(ctx) // first: no rates yet
	if len(c.VM("web1")) != 0 || len(c.Host()) != 0 {
		t.Fatal("first sample must not produce points")
	}
	now = now.Add(10 * time.Second)
	// 10 s with 2 vCPUs = 20 cpu-seconds possible; 5 used -> 25 %
	cur = map[string]Raw{"web1": {CPUTimeNs: 5e9, VCPUs: 2, RSSKiB: 512 * 1024, RdBytes: 10000, WrBytes: 20000, RxBytes: 1000, TxBytes: 3000}}
	busy, total = 400, 2000
	c.Sample(ctx)
	p := c.VM("web1")
	if len(p) != 1 || p[0].CPU != 25 || p[0].MemMiB != 512 || p[0].DiskRead != 1000 || p[0].DiskWrite != 2000 || p[0].NetRx != 100 || p[0].NetTx != 300 {
		t.Fatalf("point %+v", p)
	}
	if h := c.Host(); len(h) != 1 || h[0].CPU != 30 || h[0].MemUsedMiB != 3000 {
		t.Fatalf("host %+v", h)
	}
	// VM restarted: counters went down -> no bogus spike
	now = now.Add(10 * time.Second)
	cur = map[string]Raw{"web1": {CPUTimeNs: 1e9, VCPUs: 2}}
	c.Sample(ctx)
	if len(c.VM("web1")) != 1 {
		t.Fatal("reset counters must be skipped")
	}
	// VM gone; after the window its history is dropped.
	cur = map[string]Raw{}
	now = now.Add(2 * time.Minute)
	c.Sample(ctx)
	if len(c.VM("web1")) != 0 {
		t.Fatal("old points must expire")
	}
}

func TestParseCPULine(t *testing.T) {
	b, tot, ok := parseCPULine("cpu  10 0 20 60 10 0 0 0 0 0")
	if !ok || b != 30 || tot != 100 {
		t.Fatalf("%d %d %v", b, tot, ok)
	}
	if _, _, ok := parseCPULine("intr 1 2"); ok {
		t.Fatal("not a cpu line")
	}
}
