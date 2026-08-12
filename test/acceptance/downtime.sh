#!/usr/bin/env bash
#
# Measures whether a rolling update actually drops requests.
#
# "Zero downtime" is the central claim of a deploy tool and the easiest one to
# believe without evidence. A rollout can look perfect in kubectl while still
# dropping connections for a second or two — the window between an old pod's
# endpoint being removed and the new one being routed to.
#
# This polls the live URL continuously across a deploy and reports the longest
# gap between consecutive successful responses. That gap is the downtime.
#
# Usage:
#   test/acceptance/downtime.sh https://hello.danecwalker.com &
#   buidl deploy -e vultr -f buidl.vultr.yaml -y
#   # the poller prints its report when you stop it, or after --duration
#
#   URL=https://... DURATION=180 test/acceptance/downtime.sh
set -uo pipefail

readonly URL="${1:-${URL:-}}"
readonly DURATION="${DURATION:-0}"        # 0 = until interrupted
readonly INTERVAL="${INTERVAL:-0.1}"      # seconds between probes
readonly TIMEOUT="${TIMEOUT:-5}"          # per-request timeout

if [[ -z "$URL" ]]; then
  echo "usage: $0 <url>   (or URL=... $0)" >&2
  exit 1
fi

readonly LOG="$(mktemp -t buidl-downtime)"

total=0
ok=0
failed=0
first_release=""
last_release=""
release_changed_at=""
start_epoch=$(date +%s)

# report prints the analysis. Registered on EXIT so Ctrl-C still produces it —
# the run is usually stopped by hand once the deploy finishes.
report() {
  local end_epoch elapsed
  end_epoch=$(date +%s)
  elapsed=$((end_epoch - start_epoch))

  echo
  echo "════════════════════════════════════════════════════════"
  echo " Downtime report: $URL"
  echo "════════════════════════════════════════════════════════"
  printf "  duration        %ds\n" "$elapsed"
  printf "  requests        %d\n" "$total"
  printf "  succeeded       %d\n" "$ok"
  printf "  failed          %d\n" "$failed"

  if [[ $total -gt 0 ]]; then
    printf "  success rate    %.2f%%\n" "$(echo "scale=4; $ok * 100 / $total" | bc)"
  fi

  # The longest gap between successful responses is the actual downtime. Compare
  # against the polling interval: a gap barely above it means no requests were
  # lost at all.
  local max_gap
  max_gap=$(awk -F' ' '
    $2 == "ok" {
      if (prev != "") {
        gap = $1 - prev
        if (gap > max) { max = gap; at = prev }
      }
      prev = $1
    }
    END { printf "%.3f", max }
  ' "$LOG")

  # The baseline is the *observed* median gap, not the configured interval. The
  # real spacing is the sleep plus a full TLS handshake and round trip, which
  # against a remote host is several times the interval — comparing to the
  # interval alone reports normal latency as an outage.
  local median_gap
  median_gap=$(awk '
    $2 == "ok" { if (prev != "") gaps[n++] = $1 - prev; prev = $1 }
    END {
      for (i = 0; i < n; i++)
        for (j = i+1; j < n; j++)
          if (gaps[j] < gaps[i]) { t = gaps[i]; gaps[i] = gaps[j]; gaps[j] = t }
      printf "%.3f", n ? gaps[int(n/2)] : 0
    }
  ' "$LOG")

  echo
  printf "  typical gap between responses:  %ss  (network round trip)\n" "$median_gap"
  printf "  longest gap between responses:  %ss\n" "$max_gap"

  echo
  # Zero failed requests is the definitive signal. A gap between *successes* only
  # means responses were spaced further apart; if the endpoint had actually gone
  # away, requests in that window would have been refused or timed out. Reporting
  # a slow response as dropped traffic would be plainly wrong.
  if [[ $failed -eq 0 ]]; then
    echo "  => NO TRAFFIC WAS DROPPED — every request succeeded"
    # A long gap with no failures is still worth surfacing as latency.
    if (( $(echo "$max_gap > $median_gap * 3" | bc -l) )); then
      printf "     (one response took %ss, %.1fx the typical round trip — latency, not an outage)\n" \
        "$max_gap" "$(echo "scale=2; $max_gap / $median_gap" | bc)"
    fi
  else
    printf "  => TRAFFIC WAS DROPPED: %d request(s) failed\n" "$failed"
    printf "     worst gap without a successful response: %ss\n" "$max_gap"
  fi

  if [[ -n "$first_release" ]]; then
    echo
    printf "  release at start  %s\n" "$first_release"
    printf "  release at end    %s\n" "$last_release"
    if [[ -n "$release_changed_at" ]]; then
      printf "  cutover           %ss into the run\n" "$release_changed_at"
    else
      echo "  cutover           (release never changed — did a deploy run?)"
    fi
  fi

  # Failures grouped by kind: a 502 during a rollout means something different
  # from a TLS error or a refused connection.
  if [[ $failed -gt 0 ]]; then
    echo
    echo "  failures by kind:"
    awk '$2 != "ok" { print "    " $2 " " $3 }' "$LOG" | sort | uniq -c | sort -rn | head -10
  fi

  echo "════════════════════════════════════════════════════════"
  echo "  raw samples: $LOG"
}
# An EXIT trap alone does not fire when the shell is terminated by a signal, and
# this script is normally stopped by hand once the deploy finishes — so the
# report would never print. Trapping the signals explicitly fixes that.
trap report EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

echo "polling $URL every ${INTERVAL}s (Ctrl-C to stop and report)"

while true; do
  now=$(date +%s.%N)

  # -w gives the code; the body carries the release id the app reports, which is
  # what pinpoints the moment the new pod started serving.
  body=$(curl -s --max-time "$TIMEOUT" -w '\n%{http_code}' "$URL" 2>/dev/null)
  curl_status=$?
  code=$(printf '%s' "$body" | tail -1)
  release=$(printf '%s' "$body" | grep -E '^release' | awk '{print $2}')

  total=$((total + 1))

  if [[ $curl_status -eq 0 && "$code" == "200" ]]; then
    ok=$((ok + 1))
    echo "$now ok 200" >> "$LOG"

    if [[ -n "$release" ]]; then
      [[ -z "$first_release" ]] && first_release="$release"
      if [[ -n "$last_release" && "$release" != "$last_release" && -z "$release_changed_at" ]]; then
        release_changed_at=$(echo "$now - $start_epoch" | bc)
        echo "  ✓ new release serving: $release" >&2
      fi
      last_release="$release"
    fi
  else
    failed=$((failed + 1))
    # Distinguish a transport failure from an HTTP error: 28 is a timeout, 7 a
    # refused connection, 35 a TLS failure. These fail for different reasons and
    # a rollout should produce none of them.
    if [[ $curl_status -ne 0 ]]; then
      echo "$now curl-error $curl_status" >> "$LOG"
      echo "  ✗ $(date +%T) transport failure (curl $curl_status)" >&2
    else
      echo "$now http $code" >> "$LOG"
      echo "  ✗ $(date +%T) HTTP $code" >&2
    fi
  fi

  if [[ "$DURATION" != "0" ]]; then
    elapsed=$(($(date +%s) - start_epoch))
    [[ $elapsed -ge $DURATION ]] && break
  fi

  sleep "$INTERVAL"
done
