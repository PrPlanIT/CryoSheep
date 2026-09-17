# Architecture

What CryoSheep is, where it runs, what triggers it, and what it must never do.

This is the plan of record. It supersedes scattered decisions made in conversation.
Invariants live in `Invariants_Dump.md`; operator-facing surfaces live in
`Presentation_Layer.md`.

---

## 1. What CryoSheep is

**A shutdown handler for a machine that has complicated things on it.** Nothing else.

Three questions, three owners. CryoSheep owns exactly one:

| question | owner |
|---|---|
| Are the mains out? | The UPS, via NUT |
| Should *this* machine stop now? | `upsmon` on that machine, or systemd, or an operator |
| How does *this* machine stop cleanly? | **CryoSheep** |

It is deliberately local-only: it never reaches across a host boundary. A sequencer
that shuts down its own host cannot finish a loop over other hosts, and a tool that
can stop machines it does not live on is a tool that can stop the wrong one.

### What it is not

- **Not a NUT orchestrator.** It does not elect a master, relay UPS state, or decide
  who monitors what. NUT already answers that.
- **Not a UPS exporter.** The Eaton exporter already collects telemetry.
- **Not a monitoring system.** It emits logs and a run journal; collectors do the rest.
- **Not a bootstrapper.** Quorum services recover themselves from a clean stop. Two
  things electing is how split brain happens.

---

## 2. Where it runs

Two deployments, one binary, differing only in what triggers them.

```
Eaton UPS  (LAN, SNMP)
  │
  ├── avocado ─┐
  ├── bamboo   │  Proxmox hosts: snmp-ups + upsmon + CryoSheep
  ├── cosmos   │  → gate on UPS, stop guests in order=, stop self
  ├── dragonfruit
  └── eggplant ┘
        │
        │  qm shutdown (ACPI)
        ▼
  k8s VMs, workstation, everything else
       systemd + CryoSheep
       → local teardown, stop. Never talks to NUT.
```

### Hosts read the UPS directly

Each Proxmox host runs its **own** `snmp-ups` driver against the Eaton over LAN, with
its own `upsmon`. No master, no slaves, no relaying.

- No single point of failure — the reason the UPS is on LAN rather than USB, applied
  consistently to the readers as well.
- No circular dependency. A NUT server living in k8s runs on a VM on a host it is
  telling to shut down; it dies partway through and gating dies with it. Worse, its
  location is dynamic while shutdown order must be static, so it cannot be pinned last.
- Five readers, but no second authority: each host decides only about **itself**.
  Nobody decides for anybody else.

`nutify` keeps what it is good at — diagnostics, history, and the operator
notification, which is sent at second zero while the cluster is certainly alive.

> nutify announces, because it is alive when announcements matter.
> The hosts decide, because they are alive when decisions matter.

### Guests do not gate

A guest never decides anything, so it never needs to read the UPS. The host drives
`qm shutdown`; if mains return mid-sequence the **host** abandons and restarts its
guests. This is why guests having no route to `upsd` is not a defect.

The host also never inspects what a guest *is*. It issues `qm shutdown` and honours
the guest's own `down=` budget. Each machine knows how to stop itself.

---

## 3. Triggers

One systemd unit catches every path, because systemd does not care why a unit is
being stopped.

| trigger | verb | halts? |
|---|---|---|
| `upsmon` NOTIFYCMD, ONBATT | `conserve` | no — reversible prefix only |
| `upsmon` NOTIFYCMD, ONLINE | `cancel` | no — reverses in LIFO order |
| `upsmon` SHUTDOWNCMD, LOWBATT/FSD | `sleep` | yes |
| `systemctl poweroff` | `sleep` | systemd halts; CryoSheep must not |
| `systemctl reboot` | `sleep` | **no** — the machine is coming back |
| `qm shutdown` (ACPI from the host) | `sleep` | systemd halts; CryoSheep must not |
| `ansible.builtin.reboot` | `sleep` | **no** |
| boot, however the last stop happened | `wake` | — |

### The halt distinction is load-bearing

`plan.Build` currently appends `host.poweroff` unconditionally. Run as a shutdown
handler during a **reboot**, that powers the machine off instead — a remote reboot
that never returns, and on a guest one that needs a hand on the hypervisor to fix.

CryoSheep must do the teardown and let systemd finish the transition it had already
started. Flag name is an open decision (§7).

### Unit shape

```ini
[Unit]
After=network-online.target kubelet.service
Requires=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/true
ExecStop=/usr/local/bin/cryosheep sleep --trigger systemd <no-halt flag>
TimeoutStopSec=120

[Install]
WantedBy=multi-user.target
```

`After=` at startup means **stopped before** at shutdown, so the teardown runs while
the API server is reachable and the mounts still exist.

A second unit runs `cryosheep wake` at boot. It must run however the node went down —
a node cut mid-sequence comes back **still cordoned**, and nothing else will fix that.

`kubectl` is invoked through bare `exec.Command`, so it inherits the ambient
environment. Under systemd there is no `KUBECONFIG` and no `HOME`; the unit must set
it explicitly or cordon and the quorum record silently fail.

---

## 4. Two regimes

The same teardown serves both. Only the evacuation policy differs, and it differs for
a reason.

| | **power event** | **maintenance** |
|---|---|---|
| driver | `upsmon` → CryoSheep | ansible (`maintenance/update-hosts.yml`) |
| scope | the whole estate | one node, `serial: 1` |
| evacuation | **cordon only** | **drain**, PDB-respecting |
| why | nowhere to go — every node is stopping | somewhere to go — the others are up |
| budget | 7m41s of battery | `--grace-period=300 --timeout=600s` |

Draining during a power event would force failovers nobody wanted — moving a database
primary off a node that is stopping, onto a node that is also stopping — and forcing
past a PodDisruptionBudget is how a primary gets killed. Draining during maintenance
is correct for exactly the opposite reason.

**Drain belongs to ansible. Cordon belongs to CryoSheep.** Neither needs the other's
policy.

---

## 5. Ansible integration

**No playbook changes are required.** `ansible.builtin.reboot` stops the machine, the
systemd unit fires, the teardown happens. The integration is that there is no
integration — which is the point.

What CryoSheep contributes to the existing cycle is the part ansible does not do:
releasing CSI mounts and terminating pods in place, so the reboot inside the drain
window does not stall for minutes on mounts whose backing network is already gone.

```
ansible: cordon → drain → os-upgrade → reboot ────┐
                                                   │  systemd stops the unit
                                                   ▼
                          CryoSheep: record → (cordon) → unmount → let systemd reboot
                                                   │
ansible: wait Ready → uncordon ◄───────────────────┘  boot: cryosheep wake
```

### The conflict this creates, and the rule that resolves it

Ansible cordons before CryoSheep ever runs, and uncordons only after
`wait --for=condition=Ready`. If `wake` uncordoned unconditionally at boot, it would
make the node schedulable **before ansible intended**, and would defeat the fail-open
rescue that deliberately leaves a node cordoned when a patch fails.

The rule is the one already stated for the quorum record: **CryoSheep undoes its own
actions and nothing else.**

> If the node was already cordoned when CryoSheep started, CryoSheep did not cordon
> it — so CryoSheep must never uncordon it.

The cordon step records whether it was a no-op, and `wake` and `cancel` skip the
reverse when it was. This resolves the conflict with no coordination between the two
tools, and it generalises: anything CryoSheep finds already in the desired state is
left exactly as it found it.

### Post-maintenance quorum records are empty, and that is correct

On a drained node there are no stateful workloads left to record. An empty record is
an accurate statement about a node that has already been emptied, not a failure.

---

## 6. Observability

CryoSheep logs to stderr. systemd captures it, journald here is persistent, and
collectors read journald. There is no shipping protocol, no spool, and no
`shipped` flag — that was journald reimplemented badly.

The run journal remains, as one thing only: **`calibrate`'s own state**, plus local
evidence readable with everything else down. It is written as it goes and fsynced per
step, so it survives the cut that ends it.

Surfaces, in full, are specified in `Presentation_Layer.md`. In summary: one loud
notification channel carrying three or four messages per event, one muted channel
carrying per-host detail, and a Grafana dashboard that is blank on an ordinary day.

---

## 7. Open decisions

1. **The no-halt flag name.** `--no-poweroff`, `--teardown-only`, or a word in the
   cryo idiom. Blocks the systemd units and Drill B.
2. **Operator-facing wording** — notification text, `cut`/`graceful` vocabulary,
   channel names. Pending sign-off; `Presentation_Layer.md` is deliberately empty
   until then.
3. **`shutdownGracePeriod` is `0s`** fleet-wide. Until it is set, pods are killed
   rather than sent SIGTERM, so no shutdown is clean and the platform's own recovery
   assumptions do not hold.
4. **Per-guest `order=` / `down=`** is set only on pfSense and Zebrina. Everything
   else falls back to a flat 90s.
5. **Eaton NMC polling** — confirm it tolerates five SNMP pollers.
6. **Nothing has run outside `--dry-run`** except the two reversible drills on
   `dungeon-chest-004`. `load.off.delay` and `shutdown.stop` have never touched
   hardware.
