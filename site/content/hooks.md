---
title: Hooks
group: Reference
description: Executables in .buidl/hooks. Each one gets the release identity and every resolved secret.
---

| Hook | When | Failure aborts the deploy? |
|---|---|---|
| `pre-build` | before the image is built | yes |
| `post-build` | after the image is pushed (`BUIDL_DIGEST` set) | yes |
| `pre-deploy` | after preflight, before apply | yes |
| `post-deploy` | after the release is healthy | no |
| `deploy-failed` | after a failed deploy | no |

`pre-deploy` is the usual place for migrations: the image exists, nothing is serving yet, and the hook can use a credential the app never sees.

Hooks also get `BUIDL_APP`, `BUIDL_ENV`, `BUIDL_RELEASE`, `BUIDL_DIGEST`, `BUIDL_IMAGE`, `BUIDL_NAMESPACE`, `BUIDL_GIT_SHA`, `BUIDL_GIT_BRANCH`, `BUIDL_ACTOR`, `BUIDL_URL`, `BUIDL_VERSION`, and `BUIDL_HOOK=1`. A hook that exists but is not executable is reported, not skipped. `buidl hooks` lists what is enabled.

```
.buidl/hooks/
  pre-deploy            executable
  pre-deploy.sample     inert until you chmod +x
```
