package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"p8sos/internal/vm"
)

func TestWorkerSeesMasterOnline(t *testing.T) {
	ctx := context.Background()
	mo := &ownedVMs{fakeVMs: &fakeVMs{}, vms: map[string]vm.VM{}}
	wo := &ownedVMs{fakeVMs: &fakeVMs{}, vms: map[string]vm.VM{}}
	master := newClusterNode(t, 4096, mo, mo)
	worker := newClusterNode(t, 8192, wo, wo)
	b := master.req(t, "POST", "/api/v1/cluster/join-code", secret, fmt.Sprintf(`{"master_url":%q}`, master.srv.URL), 201)
	code := strings.SplitN(b[strings.Index(b, `"code":"`)+8:], `"`, 2)[0]
	worker.req(t, "POST", "/api/v1/cluster/join", secret, fmt.Sprintf(`{"url":%q,"code":%q}`, master.srv.URL, code), 200)
	master.m.Poll(ctx) // the master's regular call
	var out struct {
		Nodes []struct {
			Role   string `json:"role"`
			Self   bool   `json:"self"`
			Status struct {
				Online bool `json:"online"`
			} `json:"status"`
		} `json:"nodes"`
	}
	json.Unmarshal([]byte(worker.req(t, "GET", "/api/v1/cluster", secret, "", 200)), &out)
	found := false
	for _, n := range out.Nodes {
		if n.Role == "master" && !n.Self {
			found = true
			if !n.Status.Online {
				t.Fatalf("the worker shows its master offline right after the master called: %+v", out)
			}
		}
	}
	if !found {
		t.Fatalf("no master in %+v", out)
	}
}
