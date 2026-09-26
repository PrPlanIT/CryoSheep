// Package remote reaches a machine CryoSheep does not run on.
//
// The local adapter is always preferable: running on the host means `qm` and
// `systemctl` are simply there, and a sequence that needs no credential cannot
// leak one. That is not a small difference — the shutdown path on this estate
// historically held a root SSH key to every hypervisor, in a pod, because the
// actuator lived somewhere other than the thing it was actuating.
//
// This package is for what is left over: an appliance, a NAS, a hypervisor that
// does not admit an agent, anything on the same power domain that still has to
// stop. Those need a credential, so the point of the design is to make it the
// smallest credential that can do the job and nothing else.
//
// Nothing here is Proxmox-shaped. Backends satisfy core.Hypervisor and
// core.Host, the same interfaces the local adapter satisfies, so the planner and
// the executor cannot tell the difference and never grow a special case.
package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Auth adds whatever a backend needs to prove itself to a request.
//
// An interface rather than a token string because the credential differs by
// platform and the difference should not reach the caller: a Proxmox API token,
// a Redfish session, a bearer token and HTTP basic are all one line here and
// none of them are the transport's business.
type Auth interface {
	Apply(*http.Request)
	// Describe names the credential without disclosing it, for the audit
	// journal. A run that used a credential should say which, not what.
	Describe() string
}

// Client is the HTTP plumbing shared by every backend: one place that sets
// deadlines, reads bodies fully and turns a status code into an error, so no
// backend reinvents it and forgets one of them.
type Client struct {
	Base string // scheme://host[:port], no trailing slash
	Auth Auth
	HTTP *http.Client
}

func (c *Client) do(ctx context.Context, method, path string, form map[string]string, out any) error {
	var body io.Reader
	if form != nil {
		vals := make([]string, 0, len(form))
		for k, v := range form {
			vals = append(vals, k+"="+v)
		}
		body = strings.NewReader(strings.Join(vals, "&"))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, body)
	if err != nil {
		return err
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if c.Auth != nil {
		c.Auth.Apply(req)
	}
	h := c.HTTP
	if h == nil {
		// A shutdown that hangs is worse than one that fails: the sequence has a
		// battery behind it, so every call is bounded even when the caller
		// forgot to bound it.
		h = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := h.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// The body is included because these APIs explain refusals there, and a
		// 403 that does not say which privilege was missing costs an hour.
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(raw)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}
