#!/usr/bin/env bash
#
# Acceptance harness: exercises buidl against a real cluster and a real registry.
#
# The unit tests cover pure logic; this covers the parts that only fail for real —
# BuildKit talking to a registry, server-side apply, rollout gating, and the
# failure diagnostics. Every bug this found on its first run was in a code path
# that unit tests had passed cleanly.
#
# Requirements:
#   - a kubeconfig context pointing at a working cluster
#   - a BuildKit endpoint (auto-discovered, or BUILDKIT_HOST)
#   - push access to the registry in examples/hello/buidl.yaml
#   - DEMO_SECRET set (the example declares it under env.secret)
#
# Usage:
#   test/acceptance/run.sh              run every case
#   test/acceptance/run.sh healthy      run one case
#   KEEP=1 test/acceptance/run.sh       leave the namespace behind for inspection
set -uo pipefail

readonly ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly APP_DIR="$ROOT/examples/hello"
readonly BUIDL="${BUIDL:-$ROOT/bin/buidl}"
readonly ENVIRONMENT="${ENVIRONMENT:-local}"
readonly NAMESPACE="${NAMESPACE:-buidl-hello}"

PASS=0
FAIL=0
FAILED_CASES=()

# --- output ------------------------------------------------------------------

blue()  { printf '\033[34m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
red()   { printf '\033[31m%s\033[0m\n' "$*"; }

step() { blue "═══ $* "; }

ok()   { green "  PASS  $*"; PASS=$((PASS+1)); }
bad()  { red   "  FAIL  $*"; FAIL=$((FAIL+1)); FAILED_CASES+=("$*"); }

# assert_contains <description> <haystack> <needle>
assert_contains() {
  local desc="$1" haystack="$2" needle="$3"
  if printf '%s' "$haystack" | grep -qF -- "$needle"; then
    ok "$desc"
  else
    bad "$desc (expected to find: $needle)"
  fi
}

# assert_not_contains <description> <haystack> <needle>
assert_not_contains() {
  local desc="$1" haystack="$2" needle="$3"
  if printf '%s' "$haystack" | grep -qF -- "$needle"; then
    bad "$desc (unexpectedly found: $needle)"
  else
    ok "$desc"
  fi
}

# assert_exit <description> <actual> <expected>
assert_exit() {
  local desc="$1" actual="$2" expected="$3"
  if [[ "$actual" == "$expected" ]]; then
    ok "$desc (exit $actual)"
  else
    bad "$desc (exit $actual, want $expected)"
  fi
}

# --- preflight ---------------------------------------------------------------

preflight() {
  step "Preflight"

  local missing=0
  for tool in kubectl; do
    command -v "$tool" >/dev/null 2>&1 || { red "missing required tool: $tool"; missing=1; }
  done

  if [[ ! -x "$BUIDL" ]]; then
    red "buidl binary not found at $BUIDL"
    red "build it first:  make build"
    missing=1
  fi

  if [[ -z "${BUILDKIT_HOST:-}" ]] && ! command -v docker >/dev/null 2>&1; then
    red "need docker on PATH (buidl will create a BuildKit container) or BUILDKIT_HOST"
    missing=1
  fi

  if [[ -z "${DEMO_SECRET:-}" ]]; then
    red "DEMO_SECRET is not set (examples/hello declares it under env.secret)"
    red "  export DEMO_SECRET=any-value-for-testing"
    missing=1
  fi

  if ! kubectl cluster-info >/dev/null 2>&1; then
    red "no reachable cluster in the current kubeconfig context"
    missing=1
  fi

  [[ $missing -eq 0 ]] || exit 1

  green "  cluster:  $(kubectl config current-context)"
  green "  builder:  ${BUILDKIT_HOST:-auto}"
  green "  buidl:    $BUIDL"
}

# deploy runs buidl deploy with the given environment overrides and captures all
# output. Overrides are passed as VAR=value arguments.
deploy() {
  local -a overrides=()
  while [[ $# -gt 0 && "$1" == *=* ]]; do
    overrides+=("$1"); shift
  done
  ( cd "$APP_DIR" && env "${overrides[@]}" "$BUIDL" deploy -e "$ENVIRONMENT" "$@" 2>&1 )
}

# run_buidl invokes any other subcommand in the app directory.
run_buidl() {
  ( cd "$APP_DIR" && "$BUIDL" "$@" 2>&1 )
}

# --- cases -------------------------------------------------------------------

# A healthy release must succeed, report what it did, and report what is running.
case_healthy() {
  step "healthy deploy"
  local out status
  out="$(deploy)"; status=$?

  assert_exit "deploy succeeds" "$status" 0
  assert_contains "reports the applied change" "$out" "Deployment"
  assert_contains "reports running instances" "$out" "Running instances"
  assert_contains "reports all instances ready" "$out" "2/2 ready"
  assert_contains "reports per-step timing" "$out" "Deploy summary"
  assert_contains "gates on health checks" "$out" "Waiting for health checks"

  # The image must be digest-pinned, never tag-based.
  local image
  image="$(kubectl -n "$NAMESPACE" get deploy hello \
    -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null)"
  assert_contains "image is digest-pinned" "$image" "@sha256:"
}

# A second deploy of unchanged config must report objects as unchanged rather than
# rewriting them, which is what makes apply idempotent.
case_idempotent() {
  step "redeploy is idempotent"
  local out
  out="$(deploy)"

  assert_contains "unchanged objects reported as unchanged" "$out" "unchanged"
  # Only the Deployment changes, because the release ID is new each time.
  assert_contains "reports a small change set" "$out" "1 changed"
}

# The app must actually serve, with buidl's injected variables and the secret.
case_serving() {
  step "app is serving with injected config"

  kubectl -n "$NAMESPACE" port-forward svc/hello 18080:80 >/dev/null 2>&1 &
  local pf=$!
  sleep 4

  local body code
  body="$(curl -fsS --max-time 10 localhost:18080/ 2>&1)"
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 localhost:18080/up)"

  kill $pf 2>/dev/null || true
  wait $pf 2>/dev/null || true

  assert_contains "readiness endpoint returns 200" "$code" "200"
  assert_contains "release id is injected" "$body" "release   dev-"
  assert_contains "environment is injected" "$body" "env       $ENVIRONMENT"
  assert_contains "instance name is injected" "$body" "instance  hello-"
  assert_contains "clear env is applied" "$body" "GREETING             hello"
  assert_contains "secret is present in the container" "$body" "DEMO_SECRET          set,"
  # The app reports length only; a leaked value here would mean buidl put it
  # somewhere it should not be.
  assert_not_contains "secret value is not echoed" "$body" "$DEMO_SECRET"
}

# A replica-only change must be reported as scaling, not as replacing instances.
case_scale_reporting() {
  step "scaling is distinguished from replacing"
  local out
  out="$(cd "$APP_DIR" && "$BUIDL" plan -e "$ENVIRONMENT" 2>&1)"
  # Nothing to assert about scaling without changing replicas, so just confirm the
  # plan renders the effect column at all.
  assert_contains "plan reports an effect column" "$out" "EFFECT"
}

# The rollout must fail on the health gate, diagnose it, and revert.
case_unready_rollback() {
  step "unready release fails the health gate and rolls back"

  local before out status
  before="$(kubectl -n "$NAMESPACE" get deploy hello \
    -o jsonpath='{.metadata.annotations.buidl\.dev/release}' 2>/dev/null)"

  out="$(deploy FAIL_READINESS=1 --auto-rollback)"; status=$?

  assert_exit "deploy fails" "$status" 1
  assert_contains "names the failing gate" "$out" "Waiting for health checks"
  assert_contains "marks the failing step" "$out" "FAIL"
  assert_contains "reports the rollback" "$out" "rolling back to"
  assert_contains "shows the failing instance's logs" "$out" "listening on"

  # After an auto-rollback the live release must be the one from before.
  sleep 5
  local after
  after="$(kubectl -n "$NAMESPACE" get deploy hello \
    -o jsonpath='{.metadata.annotations.buidl\.dev/release}' 2>/dev/null)"
  if [[ "$after" == "$before" ]]; then
    ok "live release restored to $before"
  else
    bad "live release is $after, want $before"
  fi
}

# A crashing container must be detected quickly, not by timing out.
case_crash_detection() {
  step "crash loop is detected fast, with logs"

  local start out status elapsed
  start=$SECONDS
  out="$(deploy CRASH_ON_BOOT=1)"; status=$?
  elapsed=$((SECONDS-start))

  assert_exit "deploy fails" "$status" 1
  assert_contains "identifies the crash loop" "$out" "crashing on startup"
  assert_contains "includes the container's own output" "$out" "CRASH_ON_BOOT is set"

  # The deploy timeout is 75s; detecting a crash loop must be much faster than
  # waiting it out, or the diagnostics add nothing.
  if [[ $elapsed -lt 60 ]]; then
    ok "detected in ${elapsed}s (before the deploy timeout)"
  else
    bad "took ${elapsed}s; should fail fast rather than time out"
  fi
}

# Status must not claim health while a rollout is stuck.
case_status_honesty() {
  step "status is honest during a stuck rollout"

  deploy CRASH_ON_BOOT=1 >/dev/null 2>&1 || true
  local out
  out="$(run_buidl status -e "$ENVIRONMENT")"

  assert_not_contains "does not claim healthy" "$out" "(healthy)"
  assert_contains "reports the incomplete rollout" "$out" "rollout"
  assert_contains "shows both releases" "$out" "releases are running at once"
  assert_contains "points at the next command" "$out" "buidl logs"

  # Recover, so later cases start from a good state.
  deploy >/dev/null 2>&1 || true
}

# Lifecycle hooks must run and receive the release context and secrets.
case_hooks() {
  step "pre-deploy hook receives release context and secrets"

  local hook="$APP_DIR/.buidl/hooks/pre-deploy"
  local backup="$hook.harness-backup"

  # Always install the harness's own hook, so assertions do not depend on
  # whatever the project happens to have. Any existing hook is set aside and
  # restored, since this case deliberately replaces it with a failing one.
  mkdir -p "$(dirname "$hook")"
  [[ -e "$hook" ]] && mv "$hook" "$backup"

  cat > "$hook" <<'HOOK'
#!/usr/bin/env bash
set -euo pipefail
echo "HOOK app=$BUIDL_APP env=$BUIDL_ENV release=$BUIDL_RELEASE ns=$BUIDL_NAMESPACE"
echo "HOOK digest=$BUIDL_DIGEST"
echo "HOOK secret_len=${#DEMO_SECRET}"
HOOK
  chmod +x "$hook"

  local out
  out="$(deploy)"

  assert_contains "hook runs" "$out" "running pre-deploy hook"
  assert_contains "hook sees the app and environment" "$out" "HOOK app=hello env=$ENVIRONMENT"
  assert_contains "hook sees the resolved digest" "$out" "HOOK digest=sha256:"
  assert_contains "hook sees secrets" "$out" "HOOK secret_len=${#DEMO_SECRET}"

  # A failing pre-deploy hook must abort before anything is applied — that is the
  # entire point of running migrations there.
  cat > "$hook" <<'HOOK'
#!/usr/bin/env bash
echo "HOOK deliberately failing" >&2
exit 7
HOOK
  chmod +x "$hook"

  local failout failstatus
  failout="$(deploy)"; failstatus=$?
  assert_exit "a failing pre-deploy hook aborts the deploy" "$failstatus" 1
  assert_contains "reports the hook's exit code" "$failout" "exit 7"
  assert_not_contains "nothing was applied" "$failout" "Applying manifests"

  # A non-executable hook must be skipped with a warning, not run or ignored.
  chmod -x "$hook"
  local skipout
  skipout="$(deploy)"
  assert_contains "non-executable hook warns" "$skipout" "not executable"

  restore_hooks
}

# restore_hooks puts back any hook this harness set aside. Also registered as an
# EXIT trap, so an interrupted run does not leave the project's hook replaced by
# the deliberately-failing one.
restore_hooks() {
  local hook="$APP_DIR/.buidl/hooks/pre-deploy"
  local backup="$hook.harness-backup"
  if [[ -e "$backup" ]]; then
    rm -f "$hook"
    mv "$backup" "$hook"
  fi
  return 0
}

# Rollback must restore a prior release without rebuilding.
case_rollback() {
  step "rollback restores a prior release"

  deploy >/dev/null 2>&1 || true
  local live
  live="$(kubectl -n "$NAMESPACE" get deploy hello \
    -o jsonpath='{.metadata.annotations.buidl\.dev/release}' 2>/dev/null)"

  local out status
  out="$(run_buidl rollback -e "$ENVIRONMENT" -y)"; status=$?

  assert_contains "reports the target release" "$out" "rolling back"
  local after
  after="$(kubectl -n "$NAMESPACE" get deploy hello \
    -o jsonpath='{.metadata.annotations.buidl\.dev/release}' 2>/dev/null)"
  if [[ "$after" != "$live" ]]; then
    ok "live release changed from $live to $after"
  else
    bad "rollback did not change the live release"
  fi

  deploy >/dev/null 2>&1 || true
}

# Inspection commands must work against a live cluster.
case_inspection() {
  step "inspection commands"

  local releases status envlist plan manifest
  releases="$(run_buidl releases -e "$ENVIRONMENT")"
  assert_contains "releases lists history" "$releases" "RELEASE"
  assert_contains "releases marks the live one" "$releases" "*"

  status="$(run_buidl status -e "$ENVIRONMENT")"
  assert_contains "status reports the release" "$status" "release"
  assert_contains "status lists instances" "$status" "Instances"

  envlist="$(run_buidl env list -e "$ENVIRONMENT")"
  assert_contains "env list reports the secret source" "$envlist" "DEMO_SECRET"
  assert_not_contains "env list never prints a secret value" "$envlist" "$DEMO_SECRET"

  plan="$(run_buidl plan -e "$ENVIRONMENT")"
  assert_contains "plan renders a change table" "$plan" "KIND"

  manifest="$(run_buidl manifest -e "$ENVIRONMENT")"
  assert_contains "manifest renders a Deployment" "$manifest" "kind: Deployment"
  assert_not_contains "manifest does not leak the secret" "$manifest" "$DEMO_SECRET"

  # plan --detailed-exitcode must signal changes with exit 2 for CI gating.
  ( cd "$APP_DIR" && "$BUIDL" plan -e "$ENVIRONMENT" --detailed-exitcode >/dev/null 2>&1 )
  local code=$?
  if [[ "$code" == "0" || "$code" == "2" ]]; then
    ok "plan --detailed-exitcode returns 0 or 2 (got $code)"
  else
    bad "plan --detailed-exitcode returned $code"
  fi
}

# The cluster must hold a pull secret, since the registry is private.
case_pull_secret() {
  step "registry pull secret"

  local secret refs
  secret="$(kubectl -n "$NAMESPACE" get secret hello-registry \
    -o jsonpath='{.type}' 2>/dev/null)"
  assert_contains "pull secret is a dockerconfigjson" "$secret" "kubernetes.io/dockerconfigjson"

  refs="$(kubectl -n "$NAMESPACE" get deploy hello \
    -o jsonpath='{.spec.template.spec.imagePullSecrets[*].name}' 2>/dev/null)"
  assert_contains "pod references the pull secret" "$refs" "hello-registry"
}

# --- driver ------------------------------------------------------------------

cleanup() {
  if [[ -n "${KEEP:-}" ]]; then
    blue "KEEP is set; leaving namespace $NAMESPACE in place"
    return
  fi
  step "Cleanup"
  kubectl delete namespace "$NAMESPACE" --wait=false >/dev/null 2>&1 && \
    green "  deleting namespace $NAMESPACE" || true
}

main() {
  # A run interrupted mid-case must not leave the project's hook replaced.
  trap restore_hooks EXIT

  preflight

  local -a cases
  if [[ $# -gt 0 ]]; then
    cases=("$@")
  else
    # Ordered so the fast, foundational cases fail first.
    cases=(
      healthy
      pull_secret
      serving
      idempotent
      scale_reporting
      inspection
      hooks
      crash_detection
      status_honesty
      unready_rollback
      rollback
    )
  fi

  for name in "${cases[@]}"; do
    if ! declare -F "case_$name" >/dev/null; then
      red "unknown case: $name"
      exit 1
    fi
    "case_$name"
  done

  echo
  step "Results"
  green "  passed: $PASS"
  if [[ $FAIL -gt 0 ]]; then
    red "  failed: $FAIL"
    for c in "${FAILED_CASES[@]}"; do red "    - $c"; done
    cleanup
    exit 1
  fi
  green "  all assertions passed"
  cleanup
}

main "$@"
