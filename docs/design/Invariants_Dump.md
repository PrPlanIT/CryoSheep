

1. We need to ensure we can restore any actions beneath that are taken during a power outage that does not last long enough that we are able to reverse cancel the shutdown actions/plans.

┌───────────────────────┬──────────────────────────────────┐
│        action         │            reversed?             │
├───────────────────────┼──────────────────────────────────┤
│ UPS deadline armed    │ yes — cancelled first            │
├───────────────────────┼──────────────────────────────────┤
│ guests stopped        │ yes — restarted from the journal │
├───────────────────────┼──────────────────────────────────┤
│ ceph osd set noout    │ yes — unset                      │
├───────────────────────┼──────────────────────────────────┤
│ nodes cordoned        │ yes — uncordoned on cancel/wake  │
├───────────────────────┼──────────────────────────────────┤
│ workloads scaled down │ no — not built                   │
> Scenario just for reminder:  Our case is we dont want partial shutdown case where the shutdown starts after 2-3 minutes, The power comes on at 4 minutes etc, Operator goes phew, nothing actually went out thats convenient, no downtime. Continues working on stuff. Hears a click, the Node actually just reset... Everything else did too. Awww shit... DOWNTIME (Awful scenario, that has happened many times at random!)

2. Normal shutdowns - k8s shutdowns with stock kubeadm + Ubuntu/Debian it tends to stall during poweroff and take much longer than expected to clear... There is attempts at resolving this in Dungeon repository within the ansible playbooks I believe as well as we did fine with the ansible kubelet update script in handling some of the shared problem space perhaps.
Hitting power off in proxmox dashboard fpr the k8s nodes pending this work we hope should do the right thing with our kubeadm Ubuntu/Debian cluster and not stall for many many minutes and eventually failover to HARD RESET, should be graceful...

- Theme: CryoSheep is a play on the idea of CrioSleep. As if we are counting sheep as the power goes out to hibernate them in cryogenic sleep chambers.
We run production workloads, stopped is never the desired state, here we do not think in shutdown, we put things in stasis, or conserve energy so they are safe during the event and then wake them up after its resoved.
As such for shutdown we simply have `cryosheep sleep`, and to cancel a shutdown if we scheduled or want to abort etc `cryosheep cancel`, then also we have `cryosheep conserve` for pre shutdown state when on battery but not low enough to warrant shutdown yet (the point of alerting and "conserving" energy). This philosophy is also why it is important we orchestrate revival when sleep is no longer needed.


settle should be like chill or what they would call it when they put on backup power or reserve power and turn the lights low etc... I cant think of the right scifi term. I think theres one that actually suits the immediate mind map and isnt too confusing... when you looked at  pre-drain-guard.py i want to mention the bits involved with the k8s kubelet update worked fine... The script we made to gracefully shutdown k8s we ended up not wiring it was so much worse. It was shutting down aggressively at times. Just making sure you are aware of the things... If we also encode shutdown reason, say shutdown, loss of power vs shutdown, user requests shutdown fuck the system, then theres a different reaction to, hey power to UPS changed, no one cares. Or the operator didnt ask its a power off event, the ups reports the power is restored, if the % goes up 1 percent or stops going down with input power seen that we can fire the cancel path that replays everything we took down and puts it all back....

cryo sleep should not fucking shell out to an ansible script, it is the power-off and hibernate, awaken orchestrator, we adopted this to make it self reliant not a chaotic mess that couldve been ansible anyways. We might need to port the primary-switchover into CryoSheep (most correct, most work) but idk if it works when we need to take everything out and not do a restart tho that would be nice to allow this to be hooked so we can isolate the perfect time to do like updates that require reboots and let whatever commands be ran per host serialized maybe as a feature to be used by things like that upgrade playbook might be nice as a sideeffect...