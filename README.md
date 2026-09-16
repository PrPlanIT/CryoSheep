# CryoSheep

Shuts a host and its guests down in order, on power loss or a normal reboot.

> **Not GA.** This is being actively experimented with on one estate. Interfaces,
> flags and on-disk layout will change without ceremony, and the UPS power-off
> path has not yet been exercised against real hardware. Read it, borrow from it,
> but do not point it at anything you are not prepared to lose.

<!-- sf:project:start -->
[![GitHub](https://img.shields.io/badge/GitHub-mirror-181717?logo=github)](https://github.com/PrPlanIT/CryoSheep) [![GitLab](https://img.shields.io/badge/GitLab-source-FC6D26?logo=gitlab)](https://gitlab.prplanit.com/PrPlanIT/CryoSheep) [![license](https://raw.githubusercontent.com/PrPlanIT/CryoSheep/main/.stagefreight/scribe/license.svg)](https://github.com/PrPlanIT/CryoSheep/blob/main/LICENSE) [![Open Issues](https://img.shields.io/github/issues/PrPlanIT/CryoSheep)](https://github.com/PrPlanIT/CryoSheep/issues) [![Open PRs](https://img.shields.io/github/issues-pr/PrPlanIT/CryoSheep)](https://github.com/PrPlanIT/CryoSheep/pulls) [![Contributors](https://img.shields.io/github/contributors/PrPlanIT/CryoSheep)](https://github.com/PrPlanIT/CryoSheep/graphs/contributors) [![donate](https://img.shields.io/badge/donate-FF5E5B?logo=ko-fi&logoColor=white)](https://ko-fi.com/T6T41IT163) [![sponsor](https://img.shields.io/badge/sponsor-EA4AAA?logo=githubsponsors&logoColor=white)](https://github.com/sponsors/PrPlanIT)
<!-- sf:project:end -->
<!-- sf:badges:start -->
[![release](https://raw.githubusercontent.com/PrPlanIT/CryoSheep/main/.stagefreight/scribe/release.svg)](https://github.com/PrPlanIT/CryoSheep/releases) [![build](https://raw.githubusercontent.com/PrPlanIT/CryoSheep/main/.stagefreight/scribe/build.svg)](https://gitlab.prplanit.com/PrPlanIT/CryoSheep/-/pipelines) [![Go Version](https://img.shields.io/github/go-mod/go-version/PrPlanIT/CryoSheep?logo=go)](https://github.com/PrPlanIT/CryoSheep) [![Go Reference](https://pkg.go.dev/badge/github.com/PrPlanIT/CryoSheep.svg)](https://pkg.go.dev/github.com/PrPlanIT/CryoSheep) [![Last Commit](https://img.shields.io/github/last-commit/PrPlanIT/CryoSheep)](https://github.com/PrPlanIT/CryoSheep/commits) [![StageFreight](https://img.shields.io/badge/StageFreight-0.12.0--dev+e2c558a-310937?logo=readthedocs&logoColor=white)](https://stagefreight.prplanit.com)
<!-- sf:badges:end -->

A UPS gives you minutes. Spending them badly is how databases end up corrupt: the
usual failure is a hypervisor halted with `shutdown now`, which yanks its guests
rather than asking them to stop, so nothing inside ever receives `SIGTERM`.

CryoSheep is **invoked, never resident**. `upsmon` owns the event stream and
decides when a sequence starts; CryoSheep owns the sequence.

## What it does

Stops guests over ACPI — `qm shutdown`, not `qm stop` — so a guest runs its own
shutdown and kubelet's `shutdownGracePeriod` can drain pods. Quiesces Ceph before
the machines using it disappear. Arms the UPS's own power-off first, as a backstop
for a sequence that cannot finish. Journals every decision locally, because Loki
and Prometheus are usually inside the estate being shut down.

Guests stop in **reverse Proxmox `startup: order=`**, so `order=1` boots first and
survives longest. That field is read rather than duplicated: Proxmox already
honours it, and a second list would drift.

## Install

```sh
curl -fsSL -o /tmp/cryosheep.tar.gz \
  https://gitlab.prplanit.com/PrPlanIT/CryoSheep/-/releases/latest-dev/downloads/CryoSheep-latest-dev-linux-amd64.tar.gz
tar xzf /tmp/cryosheep.tar.gz -C /usr/local/bin cryosheep
rm /tmp/cryosheep.tar.gz
cryosheep version
```

Replace `latest-dev` with a release tag for a pinned build, e.g.
`/-/releases/v1.2.3/downloads/CryoSheep-v1.2.3-linux-amd64.tar.gz`.

## Start here

`plan` reads the host and prints what would happen. It changes nothing, needs no
configuration, and is safe on a production hypervisor:

```sh
cryosheep plan --ups-addr <nut-server>
```

It reports the roles it detected, every guest with its startup order, the ordered
sequence, where the point of no return falls, and whether the worst case fits
inside the battery you actually have.

## Commands

| | |
|---|---|
| `plan` | show what would happen; changes nothing |
| `quiesce` | the reversible prefix — `upsmon` `NOTIFYCMD`, `ONBATT` |
| `restore` | undo the last quiesce — `NOTIFYCMD`, `ONLINE` |
| `halt` | the whole sequence, ungated — `SHUTDOWNCMD`, or systemd |
| `report` | emit undelivered run journals, once |
| `calibrate` | recommend a trigger from what past runs actually cost |

Every acting command takes `--dry-run`.

## How it decides

Three rules, each because the alternative fails badly during a power cut.

**The sequence always reaches the halt.** A step that errors is recorded and the
walk continues — a host left running until the battery is flat is worse than any
single step failing.

**Only a positive reading aborts.** Mains observed back before the point of no
return abandons the sequence and reverses it. Being *unable to read* the UPS
changes nothing, in either direction: blindness must never alter a decision.

**Gates stop once committed.** They run before every reversible step and before
the committing one, then cease. Re-checking after that could only add a way to hang.

## Files it creates

| path | what |
|---|---|
| `/usr/local/bin/cryosheep` | the binary |
| `/var/lib/cryosheep/runs/` | run journals, ~900 bytes each, 20 retained |

Nothing else. No daemon, no unit files of its own, no state outside those two paths.

## Uninstall

```sh
rm -f /usr/local/bin/cryosheep
rm -rf /var/lib/cryosheep
```

If you wired it into `upsmon` or systemd, remove those references too — CryoSheep
does not install them and will not remove them:

```sh
grep -rl cryosheep /etc/nut /etc/systemd/system 2>/dev/null
```

## Calibration

The shutdown trigger should be measured, not chosen. Every run journals its
per-step timings, so the history answers it:

```sh
cryosheep calibrate --ups-addr <nut-server>
```

It recommends `override.battery.runtime.low` from the worst observed run plus a
margin, excludes runs that were cut short, and refuses a recommendation that does
not fit the battery. Runtime rather than charge percent, because runtime already
folds load in.

## License

AGPL-3.0-only.
