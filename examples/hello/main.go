// Command hello is a deliberately misbehavable web app for exercising buidl.
//
// A working app only tests the happy path. The interesting parts of a deploy tool
// are its failure paths — rollout timeouts, crash loops, unready probes, drain
// behavior — and those are hard to trigger with an app that works. So this one can
// be told to fail in specific, controlled ways via environment variables, which
// means the failure reporting can be tested with a config change rather than by
// deliberately breaking something.
//
// Switches:
//
//	FAIL_READINESS=1     /readyz returns 503 forever (tests rollout gating + auto-rollback)
//	CRASH_ON_BOOT=1      exit(1) immediately (tests CrashLoopBackOff detection)
//	CRASH_AFTER=10s      exit(1) after a delay (tests liveness restart)
//	BOOT_DELAY=20s       stay unready for a while (tests the startup probe)
//	DRAIN_DELAY=5s       delay shutdown after SIGTERM (tests graceful drain)
//	FAIL_RATIO=0.5       fraction of / requests that return 500
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// ready gates the readiness probe. It flips true after BOOT_DELAY elapses.
var ready atomic.Bool

// requests counts handled requests, so a rollout can be observed shifting load.
var requests atomic.Int64

func main() {
	log("starting")

	// Crash before doing anything, to produce a CrashLoopBackOff.
	if envBool("CRASH_ON_BOOT") {
		log("CRASH_ON_BOOT is set; exiting non-zero without serving")
		os.Exit(1)
	}

	port := envString("PORT", "8080")

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleRoot)
	mux.HandleFunc("/readyz", handleReady)
	mux.HandleFunc("/livez", handleLive)
	mux.HandleFunc("/startupz", handleStartup)
	mux.HandleFunc("/crash", handleCrash)

	server := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
		// Bounded timeouts so a hung client cannot occupy a connection forever.
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Become ready only after the configured delay, so /startupz has
	// something real to wait on.
	bootDelay := envDuration("BOOT_DELAY", 0)
	if bootDelay > 0 {
		log("delaying readiness by %s", bootDelay)
		go func() {
			time.Sleep(bootDelay)
			ready.Store(true)
			log("now ready")
		}()
	} else {
		ready.Store(true)
	}

	if after := envDuration("CRASH_AFTER", 0); after > 0 {
		log("will exit non-zero after %s", after)
		go func() {
			time.Sleep(after)
			log("CRASH_AFTER elapsed; exiting non-zero")
			os.Exit(1)
		}()
	}

	// Handle SIGTERM so a rolling update can drain cleanly. Without this the
	// process would die instantly and in-flight requests would be dropped, which
	// is exactly what buidl's preStop delay and drain timeout exist to prevent.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		// Fail readiness first so load balancers stop sending new work, then wait
		// before actually shutting down.
		ready.Store(false)
		drain := envDuration("DRAIN_DELAY", 2*time.Second)
		log("received shutdown signal; draining for %s", drain)
		time.Sleep(drain)

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log("shutdown error: %v", err)
		}
	}()

	log("listening on :%s (release %s)", port, envString("BUIDL_RELEASE", "unknown"))

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log("server error: %v", err)
		os.Exit(1)
	}
	log("stopped cleanly")
}

// handleRoot reports identity and configuration, so a rollout is visible and env
// propagation is verifiable from outside the cluster.
func handleRoot(w http.ResponseWriter, r *http.Request) {
	n := requests.Add(1)

	// Optional error injection, for observing a partially failing release.
	if ratio := envFloat("FAIL_RATIO", 0); ratio > 0 {
		// Deterministic by request count rather than random, so a test can predict
		// exactly which requests fail.
		if float64(n%100)/100.0 < ratio {
			http.Error(w, "injected failure", http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	fmt.Fprintf(w, "hello from buidl — v2\n\n")
	fmt.Fprintf(w, "served    %s\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(w, "release   %s\n", envString("BUIDL_RELEASE", "-"))
	fmt.Fprintf(w, "env       %s\n", envString("BUIDL_ENV", "-"))
	fmt.Fprintf(w, "app       %s\n", envString("BUIDL_APP", "-"))
	fmt.Fprintf(w, "instance  %s\n", envString("BUIDL_INSTANCE", "-"))
	fmt.Fprintf(w, "commit    %s\n", envString("BUIDL_GIT_SHA", "-"))
	fmt.Fprintf(w, "requests  %d\n", n)
	fmt.Fprintf(w, "ready     %t\n", ready.Load())

	// Report secret presence and length only. Echoing the value would make this
	// example a template for leaking credentials into a response body.
	fmt.Fprintf(w, "\nsecrets:\n")
	for _, name := range secretNames() {
		value := os.Getenv(name)
		if value == "" {
			fmt.Fprintf(w, "  %-20s (not set)\n", name)
			continue
		}
		fmt.Fprintf(w, "  %-20s set, %d chars\n", name, len(value))
	}

	fmt.Fprintf(w, "\nconfig:\n")
	for _, kv := range interestingEnv() {
		fmt.Fprintf(w, "  %-20s %s\n", kv[0], kv[1])
	}
}

// handleReady is the readiness probe buidl gates rollouts on.
func handleReady(w http.ResponseWriter, r *http.Request) {
	// An app that reports ready before it can serve is the most common cause of a
	// "successful" deploy that drops traffic, so this switch exists to prove buidl
	// actually waits.
	if envBool("FAIL_READINESS") {
		http.Error(w, "readiness deliberately failing (FAIL_READINESS)", http.StatusServiceUnavailable)
		return
	}
	if !ready.Load() {
		http.Error(w, "not ready yet", http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintln(w, "ok")
}

// handleStartup is the startup probe. Kubernetes will not run liveness or
// readiness until this succeeds, so a slow boot must fail here, not on /livez.
func handleStartup(w http.ResponseWriter, r *http.Request) {
	if !ready.Load() {
		http.Error(w, "still starting", http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintln(w, "ok")
}

// handleLive is the liveness probe. It stays healthy unless the process is dying,
// so a readiness failure alone does not trigger restarts.
func handleLive(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintln(w, "ok")
}

// handleCrash exits the process, for testing restart and crash-loop detection on
// demand rather than by redeploying.
func handleCrash(w http.ResponseWriter, r *http.Request) {
	log("crash requested via /crash")
	fmt.Fprintln(w, "crashing")
	go func() {
		time.Sleep(100 * time.Millisecond)
		os.Exit(1)
	}()
}

// secretNames lists the demo secret variables whose presence is reported.
func secretNames() []string {
	return []string{"DEMO_SECRET", "DATABASE_URL"}
}

// interestingEnv returns the non-secret configuration worth displaying.
func interestingEnv() [][2]string {
	var out [][2]string
	for _, entry := range os.Environ() {
		name, value, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		switch name {
		case "LOG_LEVEL", "GREETING", "PORT", "FAIL_READINESS", "BOOT_DELAY",
			"CRASH_AFTER", "DRAIN_DELAY", "FAIL_RATIO":
			out = append(out, [2]string{name, value})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

func envString(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func envBool(name string) bool {
	switch strings.ToLower(os.Getenv(name)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func envDuration(name string, fallback time.Duration) time.Duration {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	// Accept a bare number as seconds, matching buidl's own duration handling.
	if !strings.ContainsAny(raw, "smh") {
		if n, err := strconv.Atoi(raw); err == nil {
			return time.Duration(n) * time.Second
		}
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		log("ignoring invalid %s=%q", name, raw)
		return fallback
	}
	return d
}

func envFloat(name string, fallback float64) float64 {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return fallback
	}
	return f
}

// log writes a timestamped line to stdout, where a container runtime collects it.
func log(format string, args ...any) {
	fmt.Printf("%s  %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, args...))
}
