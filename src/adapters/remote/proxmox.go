package remote

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
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
			id := strconv.Itoa(g.VMID)

			// The listing does not carry `startup`, so each guest's config is
			// read for it. Without this the declared policy never reaches a
			// remote host: every guest arrives unordered on the fallback budget,
			// which puts a router in the same wave as the Kubernetes nodes that
			// route through it.
			order, down := core.OrderUnset, time.Duration(0)
			var cfg pveConfig
			if err := p.Client.do(ctx, http.MethodGet,
				"/api2/json/nodes/"+p.Node+"/"+kind+"/"+id+"/config", nil, &cfg); err == nil {
				order, down = parseStartup(cfg.Data.Startup)
			}

			out = append(out, core.Guest{
				ID:     id,
				Name:   g.Name,
				Status: g.Status,
				Order:  order,
				Down:   down,
			})
		}
	}
	return out, nil
}

// pveConfig is the part of a guest config this needs.
type pveConfig struct {
	Data struct {
		Startup string `json:"startup"`
	} `json:"data"`
}

// parseStartup reads Proxmox's `startup` value — "order=4,up=200,down=240".
//
// The local adapter parses the same field out of `qm config` text; here it
// arrives as one JSON string, so the scanning differs even though the grammar
// does not. A value that will not parse yields the unset default rather than a
// guess: an invented order is worse than none, because none is visible.
func parseStartup(v string) (int, time.Duration) {
	order, down := core.OrderUnset, time.Duration(0)
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		switch {
		case strings.HasPrefix(part, "order="):
			if n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(part, "order="))); err == nil {
				order = n
			}
		case strings.HasPrefix(part, "down="):
			if n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(part, "down="))); err == nil {
				down = time.Duration(n) * time.Second
			}
		}
	}
	return order, down
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
		secs = int(core.DefaultGuestTimeout.Seconds())
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
