---
title: Watch
group: Operate
description: A boxed live dashboard of the stack. Health, RAM, CPU, uptime.
---

```
buidl watch
buidl watch api
buidl watch --once
```

A boxed live dashboard: stack and cluster cards, per-app CPU/RAM sparklines, gauges, and the selected app's instances. Health, ready counts, uptime, restarts, and the live release sit in the same frame.

RAM and CPU come from metrics-server. k3s bundles it unless disabled; without it those series show `—` and everything else still updates.

## Keys

| Key | Action |
|---|---|
| `q` / Ctrl+C | quit |
| `j` / `k` or arrows | select an app |
| `r` | refresh now |

The selected app expands to its instances and URL. History is the last two minutes of samples (~60 ticks at 2s), so the graphs fill in while the session is open.

## One-shot

`--once` and non-TTY stdout print one snapshot and exit. The same frame, no growing series.

The default 30m command timeout does not apply to a live session unless you pass `--timeout`.

## Interval

`--interval` defaults to 2s.

```
buidl watch --interval 5s
```
