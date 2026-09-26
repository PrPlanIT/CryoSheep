package remote

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/PrPlanIT/CryoSheep/src/core"
)

// TokenAuth is a Proxmox API token.
//
// The privilege that matters is Sys.PowerMgmt, and a token can be limited to it:
// power a machine off, and nothing else. That is the whole reason to prefer this
// over the root SSH key it replaces — the key could do anything on every
// hypervisor, and a key that can do anything is a key that eventually does.
type TokenAuth struct {
	// TokenID is the full identifier, e.g. "cryosheep@pve!shutdown".
	TokenID string
	Secret  string
}

func (t TokenAuth) Apply(r *http.Request) {
	r.Header.Set("Authorization", "PVEAPIToken="+t.TokenID+"="+t.Secret)
}

// Describe names the token without its secret, so a run can record which
// credential it used.
func (t TokenAuth) Describe() string { return "proxmox token " + t.TokenID }

// Proxmox drives one node of a Proxmox cluster over its API.
//
// It satisfies core.Hypervisor and core.Host, the same interfaces the local
// adapter satisfies, so nothing above it knows which is in use.
type Proxmox struct {
	Client *Client
	Node   string // the node this instance acts on
}

type pveList struct {
	Data []struct {
		VMID   int    `json:"vmid"`
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"data"`
}

// Guests lists both VMs and containers.
//
// Both, because a guest that is missed is a guest that gets killed when the host
// halts, and "we only handled QEMU" is not a distinction the filesystem on a
// container's disk cares about.
func (p *Proxmox) Guests(ctx context.Context) ([]core.Guest, error) {
	var out []core.Guest
	for _, kind := range []string{"qemu", "lxc"} {
		var l pveList
		if err := p.Client.do(ctx, http.MethodGet, "/api2/json/nodes/"+p.Node+"/"+kind, nil, &l); err != nil {
			return nil, err
		}
		for _, g := range l.Data {
			out = append(out, core.Guest{
				ID:     strconv.Itoa(g.VMID),
				Name:   g.Name,
				Status: g.Status,
				Order:  core.OrderUnset,
			})
		}
	}
	return out, nil
}

// kindOf asks which endpoint family a guest belongs to. Proxmox ids are unique
// across both, but the paths are not shared, so this is looked up rather than
// guessed.
func (p *Proxmox) kindOf(ctx context.Context, id string) (string, error) {
	for _, kind := range []string{"qemu", "lxc"} {
		var l pveList
		if err := p.Client.do(ctx, http.MethodGet, "/api2/json/nodes/"+p.Node+"/"+kind, nil, &l); err != nil {
			return "", err
		}
		for _, g := range l.Data {
			if strconv.Itoa(g.VMID) == id {
				return kind, nil
			}
		}
	}
	return "", fmt.Errorf("guest %s on node %s: %w", id, p.Node, errUnknownGuest)
}

// errUnknownGuest is local rather than in core: it is a detail of how this API
// is addressed, not something the planner or executor should learn to handle.
var errUnknownGuest = errors.New("not found")

// Shutdown asks a guest to stop and bounds how long it may take.
//
// forceStop is deliberately set: the timeout is the guest's whole budget, and a
// guest still running when it expires is one that will otherwise be killed by
// the host halting moments later. Forcing at a deadline we chose is better than
// being cut at one we did not.
func (p *Proxmox) Shutdown(ctx context.Context, id string, timeout time.Duration) error {
	kind, err := p.kindOf(ctx, id)
	if err != nil {
		return err
	}
	secs := int(timeout.Seconds())
	if secs <= 0 {
		secs = 90
	}
	return p.Client.do(ctx, http.MethodPost,
		"/api2/json/nodes/"+p.Node+"/"+kind+"/"+id+"/status/shutdown",
		map[string]string{"timeout": strconv.Itoa(secs), "forceStop": "1"}, nil)
}

// Start puts a guest back, so a shutdown abandoned when mains returned can be
// undone rather than completed.
func (p *Proxmox) Start(ctx context.Context, id string) error {
	kind, err := p.kindOf(ctx, id)
	if err != nil {
		return err
	}
	return p.Client.do(ctx, http.MethodPost,
		"/api2/json/nodes/"+p.Node+"/"+kind+"/"+id+"/status/start", nil, nil)
}

// Poweroff halts the node itself.
func (p *Proxmox) Poweroff(ctx context.Context) error {
	return p.Client.do(ctx, http.MethodPost,
		"/api2/json/nodes/"+p.Node+"/status",
		map[string]string{"command": "shutdown"}, nil)
}
