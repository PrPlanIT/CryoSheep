package main

import (
	"flag"
	"os"
	"testing"
	"time"

	"github.com/PrPlanIT/CryoSheep/src/audit"
	"github.com/PrPlanIT/CryoSheep/src/plan"
)

// The flags exist to reach the plan. A flag that parses but never arrives is the
// same as no flag, and nothing else in the tree would notice.
func TestFlagsReachTheOptions(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		env  map[string]string
		want plan.Options
	}{
		{
			name: "defaults leave concurrency and the barrier unset",
			want: plan.Options{},
		},
		{
			name: "flags are taken",
			args: []string{"--guest-concurrency=-1", "--hold-until=4m"},
			want: plan.Options{GuestConcurrency: -1, HoldUntil: 4 * time.Minute},
		},
		{
			name: "environment supplies the default, so a unit file can carry it",
			env:  map[string]string{"CRYOSHEEP_GUEST_CONCURRENCY": "2", "CRYOSHEEP_HOLD_UNTIL": "90s"},
			want: plan.Options{GuestConcurrency: 2, HoldUntil: 90 * time.Second},
		},
		{
			name: "a flag beats the environment",
			args: []string{"--guest-concurrency=1"},
			env:  map[string]string{"CRYOSHEEP_GUEST_CONCURRENCY": "-1"},
			want: plan.Options{GuestConcurrency: 1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			var f runFlags
			fs := flag.NewFlagSet("run", flag.ContinueOnError)
			f.bind(fs)
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			if f.guestConcurrency != tc.want.GuestConcurrency {
				t.Errorf("GuestConcurrency = %d, want %d", f.guestConcurrency, tc.want.GuestConcurrency)
			}
			if f.holdUntil != tc.want.HoldUntil {
				t.Errorf("HoldUntil = %v, want %v", f.holdUntil, tc.want.HoldUntil)
			}
		})
	}
}

// A value that is not a number must not silently become one. Ignoring it keeps
// the documented default; taking it as zero would quietly turn a host's policy
// into something nobody chose.
func TestUnparseableEnvIsIgnoredNotGuessed(t *testing.T) {
	t.Setenv("CRYOSHEEP_GUEST_CONCURRENCY", "lots")
	if got := envInt("CRYOSHEEP_GUEST_CONCURRENCY"); got != 0 {
		t.Fatalf("envInt = %d, want 0 so withDefaults applies the documented default", got)
	}
}

// A half-configured remote target must not fall back to the local machine:
// stopping the wrong host is worse than refusing to stop any.
func TestPartialRemoteConfigDoesNotSilentlyActLocally(t *testing.T) {
	t.Setenv("CRYOSHEEP_PROXMOX_URL", "https://eggplant:8006")
	t.Setenv("CRYOSHEEP_PROXMOX_TOKEN_ID", "cryosheep@pve!shutdown")
	// secret and node deliberately absent
	if _, _, ok := remoteTarget(); ok {
		t.Fatal("an incomplete remote configuration was accepted")
	}
}

func TestCompleteRemoteConfigIsUsed(t *testing.T) {
	t.Setenv("CRYOSHEEP_PROXMOX_URL", "https://eggplant:8006/")
	t.Setenv("CRYOSHEEP_PROXMOX_TOKEN_ID", "cryosheep@pve!shutdown")
	t.Setenv("CRYOSHEEP_PROXMOX_TOKEN_SECRET", "s3cret")
	t.Setenv("CRYOSHEEP_PROXMOX_NODE", "eggplant")

	px, node, ok := remoteTarget()
	if !ok {
		t.Fatal("a complete remote configuration was rejected")
	}
	if node != "eggplant" || px.Node != "eggplant" {
		t.Fatalf("node = %q/%q, want eggplant", node, px.Node)
	}
	// The trailing slash must go, or every path becomes a double slash.
	if px.Client.Base != "https://eggplant:8006" {
		t.Fatalf("base = %q, want the trailing slash trimmed", px.Client.Base)
	}
}

func TestNoRemoteConfigMeansLocal(t *testing.T) {
	for _, k := range []string{"CRYOSHEEP_PROXMOX_URL", "CRYOSHEEP_PROXMOX_TOKEN_ID",
		"CRYOSHEEP_PROXMOX_TOKEN_SECRET", "CRYOSHEEP_PROXMOX_NODE"} {
		_ = os.Unsetenv(k)
	}
	if _, _, ok := remoteTarget(); ok {
		t.Fatal("no configuration should mean the local machine")
	}
}

func TestParseOrderGroups(t *testing.T) {
	for _, tc := range []struct {
		name, spec string
		want       []plan.OrderGroup
	}{
		{"empty is no grouping", "", nil},
		{"the emergency shape", "5-99,4,2-3,1", []plan.OrderGroup{{Lo: 5, Hi: 99}, {Lo: 4, Hi: 4}, {Lo: 2, Hi: 3}, {Lo: 1, Hi: 1}}},
		{"single orders need no dash", "9,4,1", []plan.OrderGroup{{Lo: 9, Hi: 9}, {Lo: 4, Hi: 4}, {Lo: 1, Hi: 1}}},
		{"spaces are tolerated", " 5-99 , 4 ", []plan.OrderGroup{{Lo: 5, Hi: 99}, {Lo: 4, Hi: 4}}},
		{"reversed range is refused whole", "5-99,9-4", nil},
		{"non-numeric is refused whole", "5-99,four", nil},
		{"zero is refused whole", "0-3", nil},
		{"a hold rides on its range", "4-99,1-3@4m30s", []plan.OrderGroup{{Lo: 4, Hi: 99}, {Lo: 1, Hi: 3, NotBefore: 4*time.Minute + 30*time.Second}}},
		{"a malformed hold is refused whole", "4-99,1-3@soon", nil},
		{"a negative hold is refused whole", "4-99,1-3@-1m", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseOrderGroups(tc.spec)
			if len(got) != len(tc.want) {
				t.Fatalf("parseOrderGroups(%q) = %v, want %v", tc.spec, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("parseOrderGroups(%q) = %v, want %v", tc.spec, got, tc.want)
				}
			}
		})
	}
}

// Whether power is the reason for stopping decides whether mains returning
// abandons the run. It used to be inferred from the launcher's environment,
// which cannot work: proved against NUT 2.8.3, upsmon passes NOTIFYTYPE to
// NOTIFYCMD and nothing at all to SHUTDOWNCMD — and since upsmon itself runs
// under systemd, its children inherit INVOCATION_ID and the sniff concluded
// "an operator asked for this" on precisely the path where nobody did.
func TestPowerIsTheReason(t *testing.T) {
	for _, tc := range []struct {
		name, status, trigger string
		want                  bool
	}{
		{"on battery", "OB", audit.TriggerManual, true},
		{"on battery, low", "OB LB", audit.TriggerManual, true},
		{"forced shutdown", "FSD", audit.TriggerManual, true},
		{"forced shutdown while discharging", "OB LB FSD", audit.TriggerManual, true},
		{"online", "OL", audit.TriggerManual, false},
		{"online and charging", "OL CHRG", audit.TriggerManual, false},
		{"unreadable UPS", "unknown", audit.TriggerManual, false},
		{"no UPS at all", "", audit.TriggerManual, false},
		// upsmon runs SHUTDOWNCMD only on a power decision; endpoints gone by then
		// must not turn it into a plain stop.
		{"upsmon, UPS unreadable", "unknown", audit.TriggerUPS, true},
		{"upsmon, mains back", "OL", audit.TriggerUPS, false},
		// A substring is not a flag: NUT reports a space-separated set, and
		// "OBSCURE" must not read as "OB".
		{"substring is not membership", "OBSCURE", audit.TriggerManual, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := powerIsTheReason(tc.status, tc.trigger); got != tc.want {
				t.Fatalf("powerIsTheReason(%q, %q) = %v, want %v", tc.status, tc.trigger, got, tc.want)
			}
		})
	}
}

// upsmon runs under systemd, so its SHUTDOWNCMD inherits INVOCATION_ID. Only our
// own ExecStop gets SERVICE_RESULT; reading INVOCATION_ID as "systemd is stopping
// this machine" made the UPS path decline to act and omit the halt.
func TestDetectTrigger(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"the stasis unit's ExecStop", map[string]string{"SERVICE_RESULT": "success", "INVOCATION_ID": "x"}, audit.TriggerSystemd},
		{"upsmon SHUTDOWNCMD, a child of upsmon.service", map[string]string{"INVOCATION_ID": "x"}, audit.TriggerUPS},
		{"upsmon NOTIFYCMD", map[string]string{"NOTIFYTYPE": "ONBATT", "INVOCATION_ID": "x"}, audit.TriggerUPS},
		{"an operator at a shell", nil, audit.TriggerManual},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"SERVICE_RESULT", "INVOCATION_ID", "NOTIFYTYPE"} {
				t.Setenv(k, tc.env[k])
			}
			if got := detectTrigger(); got != tc.want {
				t.Fatalf("detectTrigger() = %q, want %q", got, tc.want)
			}
		})
	}
}
