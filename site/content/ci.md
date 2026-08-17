---
title: CI
group: Guides
description: init writes the workflow. Output is plain in CI, JSON if you ask.
---

Output is plain in CI, colored on a terminal, or newline-delimited JSON with `-o json`. Warnings and errors become CI annotations. A dirty working tree warns locally and fails in CI.

`buidl init` asks whether you want GitHub Actions. A yes writes a workflow that deploys the app on every push to `main`. If you also want staging, the workflow deploys staging on push and promotes that digest to production. Review apps add a preview per pull request and tear it down when the PR closes.

```sh
buidl init --ci
buidl init --ci --staging --preview
buidl init --no-ci
```

`--ci`, `--staging`, and `--preview` answer the same questions without a prompt.

`deploy --dry-run --detailed-exitcode` returns 2 when something would change, so a pipeline can require approval only when needed.

This is the workflow `init` writes for *your* app. It is not the CI that tests buidl itself.
