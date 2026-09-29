package remote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PrPlanIT/CryoSheep/src/core"
)

// fakePVE is enough of the Proxmox API to exercise the adapter without a
// hypervisor. Every test here runs on a laptop; nothing needs hardware, which is
// the only way a shutdown path that fires once a year gets exercised at all.
func fakePVE(t *testing.T, rec *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		*rec = append(*rec, r.Method+" "+r.URL.Path+" "+r.Form.Encode()+" auth="+r.Header.Get("Authorization"))
		switch {
		case strings.HasSuffix(r.URL.Path, "/qemu"):
			_ = json.NewEncoder(w).Encode(pveList{Data: []struct {
				VMID   int    `json:"vmid"`
				Name   string `json:"name"`
				Status string `json:"status"`
			}{{VMID: 101, Name: "chest-001", Status: "running"}}})
		case strings.HasSuffix(r.URL.Path, "/lxc"):
			_ = json.NewEncoder(w).Encode(pveList{Data: []struct {
				VMID   int    `json:"vmid"`
				Name   string `json:"name"`
				Status string `json:"status"`
			}{{VMID: 202, Name: "pve-ups", Status: "running"}}})
		default:
			_, _ = w.Write([]byte(`{"data":null}`))
		}
	}))
}

func newProxmox(t *testing.T, rec *[]string) *Proxmox {
	t.Helper()
	srv := fakePVE(t, rec)
	t.Cleanup(srv.Close)
	return &Proxmox{
		Client: &Client{Base: srv.URL, Auth: TokenAuth{TokenID: "cryosheep@pve!shutdown", Secret: "s3cret"}},
		Node:   "eggplant",
	}
}

// Containers are guests too. Missing them means they are killed when the host
// halts rather than stopped.
func TestGuestsIncludesContainersNotJustVMs(t *testing.T) {
	var rec []string
	p := newProxmox(t, &rec)
	gs, err := p.Guests(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(gs) != 2 {
		t.Fatalf("guests = %v, want one VM and one container", gs)
	}
	var ids []string
	for _, g := range gs {
		ids = append(ids, g.ID)
	}
	if ids[0] != "101" || ids[1] != "202" {
		t.Fatalf("ids = %v, want 101 and 202", ids)
	}
}

func TestShutdownAddressesTheRightEndpointFamily(t *testing.T) {
	for _, tc := range []struct{ id, want string }{
		{"101", "/api2/json/nodes/eggplant/qemu/101/status/shutdown"},
		{"202", "/api2/json/nodes/eggplant/lxc/202/status/shutdown"},
	} {
		var rec []string
		p := newProxmox(t, &rec)
		if err := p.Shutdown(context.Background(), tc.id, 90*time.Second); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(rec, "\n"), "POST "+tc.want) {
			t.Fatalf("guest %s: calls %v, want POST %s", tc.id, rec, tc.want)
		}
	}
}

// The timeout is the guest's whole budget. Forcing at a deadline we chose beats
// being cut at one we did not, moments later, by the host halting.
func TestShutdownPassesTheBudgetAndForcesAtIt(t *testing.T) {
	var rec []string
	p := newProxmox(t, &rec)
	if err := p.Shutdown(context.Background(), "101", 185*time.Second); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(rec, "\n")
	if !strings.Contains(all, "timeout=185") {
		t.Fatalf("calls %v, want the 185s budget passed through", rec)
	}
	if !strings.Contains(all, "forceStop=1") {
		t.Fatalf("calls %v, want forceStop at the deadline", rec)
	}
}

func TestPoweroffTargetsTheNode(t *testing.T) {
	var rec []string
	p := newProxmox(t, &rec)
	if err := p.Poweroff(context.Background()); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(rec, "\n")
	if !strings.Contains(all, "POST /api2/json/nodes/eggplant/status") || !strings.Contains(all, "command=shutdown") {
		t.Fatalf("calls %v, want a node shutdown", rec)
	}
}

// Start is what makes a shutdown abandonable when mains return.
func TestStartPutsAGuestBack(t *testing.T) {
	var rec []string
	p := newProxmox(t, &rec)
	if err := p.Start(context.Background(), "101"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(rec, "\n"), "/qemu/101/status/start") {
		t.Fatalf("calls %v, want a start", rec)
	}
}

func TestTokenIsSentAsAProxmoxAPIToken(t *testing.T) {
	var rec []string
	p := newProxmox(t, &rec)
	if _, err := p.Guests(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec[0], "auth=PVEAPIToken=cryosheep@pve!shutdown=s3cret") {
		t.Fatalf("auth header not sent as a PVE token: %q", rec[0])
	}
}

// A credential should be nameable in the journal without being disclosed in it.
func TestDescribeNamesTheTokenWithoutTheSecret(t *testing.T) {
	d := TokenAuth{TokenID: "cryosheep@pve!shutdown", Secret: "s3cret"}.Describe()
	if !strings.Contains(d, "cryosheep@pve!shutdown") {
		t.Fatalf("Describe() = %q, want the token id", d)
	}
	if strings.Contains(d, "s3cret") {
		t.Fatalf("Describe() = %q, must not contain the secret", d)
	}
}

// An API that refuses explains why in the body; a 403 that does not say which
// privilege was missing costs an hour.
func TestErrorsCarryTheServersExplanation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":{"privilege":"Sys.PowerMgmt required"}}`))
	}))
	t.Cleanup(srv.Close)
	p := &Proxmox{Client: &Client{Base: srv.URL, Auth: TokenAuth{TokenID: "t", Secret: "s"}}, Node: "eggplant"}
	err := p.Poweroff(context.Background())
	if err == nil {
		t.Fatal("want an error on 403")
	}
	if !strings.Contains(err.Error(), "Sys.PowerMgmt") {
		t.Fatalf("error = %v, want the server's explanation included", err)
	}
}

// Anything reachable only over the network must be bounded, because the
// sequence has a battery behind it and a call that hangs spends it.
func TestRequestsAreBoundedByDefault(t *testing.T) {
	c := &Client{Base: "http://example.invalid"}
	var rec []string
	_ = rec
	if c.HTTP != nil {
		t.Fatal("precondition: no client configured")
	}
	// do() supplies a bounded client when none was given; a nil timeout here
	// would mean a wedged endpoint holds the shutdown open indefinitely.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := c.do(ctx, http.MethodGet, "/", nil, nil); err == nil {
		t.Fatal("want an error reaching an invalid host")
	}
}

func TestParseStartup(t *testing.T) {
	for _, tc := range []struct {
		name, in  string
		wantOrder int
		wantDown  time.Duration
	}{
		{"empty means unordered", "", core.OrderUnset, 0},
		{"order alone", "order=4", 4, 0},
		{"order and down", "order=4,down=240", 4, 240 * time.Second},
		{"the storage tier", "order=3,up=200,down=180", 3, 180 * time.Second},
		{"down without order", "down=60", core.OrderUnset, 60 * time.Second},
		{"up only", "up=0", core.OrderUnset, 0},
		{"whitespace tolerated", " order=1 , down=120 ", 1, 120 * time.Second},
		{"garbage is not an order", "order=x,down=90", core.OrderUnset, 90 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, d := parseStartup(tc.in)
			if o != tc.wantOrder || d != tc.wantDown {
				t.Fatalf("parseStartup(%q) = (%d, %v), want (%d, %v)",
					tc.in, o, d, tc.wantOrder, tc.wantDown)
			}
		})
	}
}

// The declared policy only reaches a remote host if the adapter reads it. It
// used to hardcode OrderUnset and never look at down=, so every guest landed in
// one wave on the 180s fallback -- stopping a router at the same instant as the
// Kubernetes nodes that route through it.
func TestGuestsCarryTheDeclaredOrderAndBudget(t *testing.T) {
	var rec []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec = append(rec, r.URL.Path)
		switch {
		case strings.HasSuffix(r.URL.Path, "/101/config"):
			_, _ = w.Write([]byte(`{"data":{"startup":"order=4,down=240"}}`))
		case strings.HasSuffix(r.URL.Path, "/202/config"):
			_, _ = w.Write([]byte(`{"data":{"startup":"order=1,down=120"}}`))
		case strings.HasSuffix(r.URL.Path, "/qemu"):
			_, _ = w.Write([]byte(`{"data":[{"vmid":101,"name":"chest-001","status":"running"}]}`))
		case strings.HasSuffix(r.URL.Path, "/lxc"):
			_, _ = w.Write([]byte(`{"data":[{"vmid":202,"name":"pve-ups","status":"running"}]}`))
		default:
			_, _ = w.Write([]byte(`{"data":null}`))
		}
	}))
	t.Cleanup(srv.Close)
	p := &Proxmox{Client: &Client{Base: srv.URL, Auth: TokenAuth{TokenID: "t", Secret: "s"}}, Node: "bamboo"}

	gs, err := p.Guests(context.Background())
	if err != nil {
		t.Fatalf("Guests: %v", err)
	}
	want := map[string]struct {
		order int
		down  time.Duration
	}{
		"101": {4, 240 * time.Second},
		"202": {1, 120 * time.Second},
	}
	if len(gs) != len(want) {
		t.Fatalf("got %d guests, want %d", len(gs), len(want))
	}
	for _, g := range gs {
		w := want[g.ID]
		if g.Order != w.order || g.Down != w.down {
			t.Fatalf("guest %s = (order %d, down %v), want (order %d, down %v)",
				g.ID, g.Order, g.Down, w.order, w.down)
		}
	}
}

// A guest whose config cannot be read is still a guest that must be stopped.
// Dropping it would leave it running when the host halts; the safe degradation
// is to treat it as unordered on the default budget.
func TestAnUnreadableConfigDoesNotLoseTheGuest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/config"):
			w.WriteHeader(http.StatusInternalServerError)
		case strings.HasSuffix(r.URL.Path, "/qemu"):
			_, _ = w.Write([]byte(`{"data":[{"vmid":101,"name":"chest-001","status":"running"}]}`))
		default:
			_, _ = w.Write([]byte(`{"data":[]}`))
		}
	}))
	t.Cleanup(srv.Close)
	p := &Proxmox{Client: &Client{Base: srv.URL, Auth: TokenAuth{TokenID: "t", Secret: "s"}}, Node: "bamboo"}

	gs, err := p.Guests(context.Background())
	if err != nil {
		t.Fatalf("Guests: %v", err)
	}
	if len(gs) != 1 || gs[0].ID != "101" || gs[0].Order != core.OrderUnset {
		t.Fatalf("guests = %+v, want one unordered guest 101", gs)
	}
}
