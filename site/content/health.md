---
title: Health checks
group: Reference
description: Three endpoints. They are not interchangeable. A deploy waits on ready.
---

Your app should serve three HTTP endpoints. A deploy succeeds only after readiness passes.

**`GET /startupz` — has this process finished booting?**

Kubernetes will not run the other two until this succeeds. Return 200 once the listen loop is up. A slow migration or cache warm belongs here, not on liveness: a slow boot is not a deadlock.

**`GET /readyz` — can this instance accept traffic?**

The rollout waits on this. Return 200 only when you can serve a real request, including required backends (a Postgres ping). Return 503 to take yourself out of the load balancer without being restarted.

**`GET /livez` — is this process still making progress?**

A failure restarts the container. Keep it cheap: if the process is running, return 200. Do not check Postgres. A database blip would restart every replica at once.

HTTP 200–399 is success. Return a tiny body (`ok`). Probes hit the container's named `http` port.

If you already have one endpoint (Rails `/up`, Kamal), point all three at it:

```yaml
deploy:
  healthcheck:
    path: /up
```

Override a single probe with `readiness`, `liveness`, or `startup`. Workers set `command` instead of HTTP paths. buidl does not guess another path if these fail: a timed-out deploy names the configured readiness URL.

Postgres and Redis accessories get `pg_isready` / `redis-cli ping` with the same timing split: a frequent startup probe, a stricter readiness check, and a more forgiving liveness check.
