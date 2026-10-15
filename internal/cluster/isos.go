package cluster

import (
	"context"
	"fmt"
	"net/url"
	"os"

	"p8sos/internal/vm"
)

// One ISO library for the cluster: the master's. A VM installed from an ISO
// on a worker gets a copy of the ISO there first (once; it is kept for the
// next install), and deleting an ISO on the master deletes the copies too.

type isoFiles interface {
	ISOFile(name string) (string, int64, error)
}

// ISOInfo is what GET /cluster/iso/{name} answers.
type ISOInfo struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
}

// pushISO makes sure server n has the master's ISO name (same size).
func (c *VMs) pushISO(ctx context.Context, n Node, name string) error {
	lf, ok := c.Local.(isoFiles)
	if !ok {
		return fmt.Errorf("%w: this server can't share its ISO library", vm.ErrConflict)
	}
	path, size, err := lf.ISOFile(name)
	if err != nil {
		return fmt.Errorf("%w: no ISO %q in the library", vm.ErrInvalid, name)
	}
	p := "/cluster/iso/" + url.PathEscape(name)
	var have ISOInfo
	if err := c.call(ctx, n, "GET", p, nil, &have); err == nil && have.Bytes == size {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	resp, err := c.M.stream(ctx, n, "PUT", p, f)
	if err != nil {
		return fmt.Errorf("copying %s to %s: %w", name, n.Name, err)
	}
	resp.Body.Close()
	return nil
}

// DeleteISO deletes an ISO from the library, and its copies on the workers
// (those a VM there still uses are kept).
func (c *VMs) DeleteISO(ctx context.Context, name string) error {
	if err := c.Local.DeleteISO(ctx, name); err != nil {
		return err
	}
	if c.M == nil || c.M.Role() != "master" {
		return nil
	}
	for _, n := range c.workers() {
		if !c.M.NodeStatus(n.ID).Online {
			continue
		}
		_ = c.call(ctx, n, "DELETE", "/cluster/iso/"+url.PathEscape(name), nil, nil)
	}
	return nil
}
