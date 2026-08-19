package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/danecwalker/buidl/internal/config"
	"github.com/danecwalker/buidl/internal/deploy"
	"github.com/danecwalker/buidl/internal/release"
)

// Object is one rendered manifest with the metadata needed to apply it.
type Object struct {
	GVK    schema.GroupVersionKind
	Name   string
	Kind   string
	Object runtime.Object
	// Namespaced is false for cluster-scoped objects like Namespace.
	Namespaced bool
	// Order controls apply sequence: lower values apply first.
	Order int
}

// Apply ordering. Dependencies must exist before the objects that reference
// them, or the first deploy into a fresh namespace fails on a missing Secret or
// ServiceAccount.
const (
	orderNamespace = 0
	orderRBAC      = 10
	orderSecret    = 20
	orderService   = 30
	orderWorkload  = 40
	orderScaling   = 50
	orderIngress   = 60
)

// Render builds the full desired state for a release.
func (t *Target) Render(req deploy.Request) ([]Object, error) {
	t.resolveScale(context.Background())

	cfg := req.Config
	rel := req.Release

	if !rel.Pinned() {
		// Deploying a tag would let the running image drift from the release
		// record on any pod restart. Refuse rather than deploy something we
		// cannot later identify.
		return nil, fmt.Errorf("release %s has no image digest; cannot render an immutable deploy", rel.ID)
	}

	var objs []Object

	if cfg.Deploy.Kubernetes.CreatesNamespace() {
		objs = append(objs, t.namespace(cfg, rel))
	}
	if cfg.Deploy.Kubernetes.ServiceAccount == "" {
		// Own a ServiceAccount per app so image pull secrets and workload
		// identity annotations have a stable place to live.
		objs = append(objs, t.serviceAccount(cfg, rel))
	}

	secret, checksum := t.secret(cfg, rel, req.Secrets)
	if secret != nil {
		objs = append(objs, *secret)
	}

	// The cluster needs its own credential to pull from a private registry; the
	// developer's or CI runner's login does not reach the kubelet.
	managedPull := t.managedPullSecret(cfg)
	if managedPull {
		pull, err := t.pullSecret(cfg, rel)
		if err != nil {
			return nil, err
		}
		if pull != nil {
			objs = append(objs, *pull)
		} else {
			managedPull = false
		}
	}

	dep, err := t.deployment(cfg, rel, checksum, managedPull)
	if err != nil {
		return nil, err
	}
	objs = append(objs, *dep)

	// Always rendered so `plan` can show the selector flip. Deploy must not
	// apply a live Service until the new release is healthy: the selector is
	// the cutover, and applying it here would empty Endpoints until the new
	// pods exist.
	objs = append(objs, t.service(cfg, rel))

	if cfg.Deploy.Autoscale != nil {
		objs = append(objs, t.hpa(cfg, rel))
	}

	// A PDB only makes sense with more than one replica; with one it would block
	// node drains entirely.
	if replicas(cfg) > 1 {
		objs = append(objs, t.pdb(cfg, rel))
	}

	if cfg.Proxy.Enabled != nil && *cfg.Proxy.Enabled {
		ing, err := t.ingress(cfg, rel)
		if err != nil {
			return nil, err
		}
		objs = append(objs, *ing)
	}

	sort.SliceStable(objs, func(i, j int) bool { return objs[i].Order < objs[j].Order })
	return objs, nil
}

func replicas(cfg *config.Config) int32 {
	if cfg.Deploy.Autoscale != nil && cfg.Deploy.Autoscale.Min > 0 {
		// PDB and topology spread should track the HPA floor, not the static
		// replica field (which is omitted so we do not fight the HPA).
		return cfg.Deploy.Autoscale.Min
	}
	if cfg.Deploy.Replicas == nil {
		return 1
	}
	return *cfg.Deploy.Replicas
}

// selectorLabels are the immutable subset used for pod selection.
//
// These must never include the release ID for a rolling deploy: a Deployment's
// selector is immutable after creation, so putting a per-release value in it
// would make the second deploy fail. Blue-green adds the release label only to
// the Service selector, which is mutable.
func selectorLabels(cfg *config.Config, rel release.Release) map[string]string {
	return map[string]string{
		release.LabelName:     cfg.App,
		release.LabelInstance: cfg.App + "-" + rel.Environment,
	}
}

// objectMeta builds metadata common to every managed object.
func (t *Target) objectMeta(cfg *config.Config, rel release.Release, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:        name,
		Namespace:   t.Namespace,
		Labels:      rel.Labels(cfg.App),
		Annotations: rel.Annotations(),
	}
}

func (t *Target) namespace(cfg *config.Config, rel release.Release) Object {
	labels := rel.Labels(cfg.App)
	if deploy.IsEphemeral(cfg) {
		// So a stale sweep can find preview namespaces without reconstructing
		// the slug that originally created them.
		labels[release.LabelEphemeral] = "true"
	}
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   t.Namespace,
			Labels: labels,
		},
	}
	return Object{
		GVK:        corev1.SchemeGroupVersion.WithKind("Namespace"),
		Name:       t.Namespace,
		Kind:       "Namespace",
		Object:     ns,
		Namespaced: false,
		Order:      orderNamespace,
	}
}

func (t *Target) serviceAccount(cfg *config.Config, rel release.Release) Object {
	name := release.ObjectName(cfg.App)
	sa := &corev1.ServiceAccount{ObjectMeta: t.objectMeta(cfg, rel, name)}
	return Object{
		GVK:        corev1.SchemeGroupVersion.WithKind("ServiceAccount"),
		Name:       name,
		Kind:       "ServiceAccount",
		Object:     sa,
		Namespaced: true,
		Order:      orderRBAC,
	}
}

// secret renders the app's environment Secret and returns a checksum of its
// contents.
//
// The checksum is annotated onto the pod template so that changing a secret
// value triggers a rollout. Without it, Kubernetes would leave existing pods
// running with the stale value — a genuinely dangerous silent no-op.
func (t *Target) secret(cfg *config.Config, rel release.Release, values map[string]string) (*Object, string) {
	if len(cfg.Env.Secret) == 0 {
		return nil, ""
	}

	data := map[string][]byte{}
	// Sort for a stable checksum across runs.
	names := append([]string(nil), cfg.Env.Secret...)
	sort.Strings(names)

	h := sha256.New()
	for _, name := range names {
		v, ok := values[name]
		if !ok {
			// Preflight rejects missing secrets, so reaching here means the value
			// was intentionally empty.
			continue
		}
		data[name] = []byte(v)
		fmt.Fprintf(h, "%s=%s\n", name, v)
	}
	checksum := hex.EncodeToString(h.Sum(nil))[:16]

	name := release.ObjectName(cfg.App, "env")
	sec := &corev1.Secret{
		ObjectMeta: t.objectMeta(cfg, rel, name),
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
	return &Object{
		GVK:        corev1.SchemeGroupVersion.WithKind("Secret"),
		Name:       name,
		Kind:       "Secret",
		Object:     sec,
		Namespaced: true,
		Order:      orderSecret,
	}, checksum
}

// deployment renders the app workload.
func (t *Target) deployment(cfg *config.Config, rel release.Release, secretChecksum string, managedPull bool) (*Object, error) {
	name := t.workloadName(cfg, rel)

	container, err := t.container(cfg, rel)
	if err != nil {
		return nil, err
	}

	podLabels := rel.Labels(cfg.App)
	// The release label on pods is what lets blue-green flip a Service selector
	// and what lets `buidl logs --release` target one release's pods.
	podLabels[release.LabelRelease] = rel.ID

	podAnnotations := rel.Annotations()
	if secretChecksum != "" {
		podAnnotations[release.AnnotationConfigSum] = secretChecksum
	}

	spec := appsv1.DeploymentSpec{
		Selector: &metav1.LabelSelector{MatchLabels: selectorLabels(cfg, rel)},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels:      podLabels,
				Annotations: podAnnotations,
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{*container},
				// Set on the pod rather than relying on the ServiceAccount, so the
				// reference is visible in the manifest a user reviews.
				ImagePullSecrets:              pullSecretRefs(cfg, managedPull),
				ServiceAccountName:            t.serviceAccountName(cfg),
				NodeSelector:                  cfg.Deploy.Kubernetes.NodeSelector,
				TerminationGracePeriodSeconds: ptr(int64(cfg.Deploy.DrainTimeout.Or(defaultDrain).Seconds())),
				SecurityContext: &corev1.PodSecurityContext{
					// Defense in depth: even if the image's USER directive is
					// missing, the pod cannot run as root.
					RunAsNonRoot: ptr(true),
					SeccompProfile: &corev1.SeccompProfile{
						Type: corev1.SeccompProfileTypeRuntimeDefault,
					},
				},
				// Spread replicas across nodes so a single node failure cannot
				// take the whole app down.
				TopologySpreadConstraints: topologySpread(cfg, rel),
			},
		},
	}

	// When an HPA owns the replica count, omitting replicas from the applied
	// object prevents buidl and the HPA from fighting over it on every deploy.
	if cfg.Deploy.Autoscale == nil {
		spec.Replicas = ptr(replicas(cfg))
	}

	switch cfg.Deploy.Strategy.Type {
	case config.StrategyRecreate:
		spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
	default:
		// Blue-green also uses RollingUpdate internally; the cutover is achieved
		// by the Service selector flip, not by the Deployment strategy.
		surge := intstr.Parse(cfg.Deploy.Strategy.MaxSurge)
		unavailable := intstr.Parse(cfg.Deploy.Strategy.MaxUnavailable)
		spec.Strategy = appsv1.DeploymentStrategy{
			Type: appsv1.RollingUpdateDeploymentStrategyType,
			RollingUpdate: &appsv1.RollingUpdateDeployment{
				MaxSurge:       &surge,
				MaxUnavailable: &unavailable,
			},
		}
	}

	// Keep enough ReplicaSet history to satisfy the configured rollback depth.
	spec.RevisionHistoryLimit = ptr(int32(cfg.RetainReleases))
	spec.ProgressDeadlineSeconds = ptr(int32(cfg.Deploy.DeployTimeout.Or(defaultDeploy).Seconds()))
	if delay := cfg.Deploy.Strategy.ReadinessDelay.Duration; delay > 0 {
		spec.MinReadySeconds = int32(delay.Seconds())
	}

	dep := &appsv1.Deployment{
		ObjectMeta: t.objectMeta(cfg, rel, name),
		Spec:       spec,
	}

	return &Object{
		GVK:        appsv1.SchemeGroupVersion.WithKind("Deployment"),
		Name:       name,
		Kind:       "Deployment",
		Object:     dep,
		Namespaced: true,
		Order:      orderWorkload,
	}, nil
}

// workloadName is the Deployment name. Blue-green needs one Deployment per live
// release so both can run simultaneously; rolling reuses a single name.
func (t *Target) workloadName(cfg *config.Config, rel release.Release) string {
	if cfg.Deploy.Strategy.Type == config.StrategyBlueGreen {
		return release.ObjectName(cfg.App, rel.ID)
	}
	return release.ObjectName(cfg.App)
}

func (t *Target) serviceAccountName(cfg *config.Config) string {
	if cfg.Deploy.Kubernetes.ServiceAccount != "" {
		return cfg.Deploy.Kubernetes.ServiceAccount
	}
	return release.ObjectName(cfg.App)
}

// imagePullPolicy is Never for a sideloaded local image (nothing to pull,
// and IfNotPresent would still try a registry that does not exist).
func imagePullPolicy(cfg *config.Config) corev1.PullPolicy {
	if cfg.LocalImage() {
		return corev1.PullNever
	}
	// The digest is immutable, so there is nothing to re-pull.
	return corev1.PullIfNotPresent
}

// container renders the app container.
func (t *Target) container(cfg *config.Config, rel release.Release) (*corev1.Container, error) {
	env, err := t.envVars(cfg, rel)
	if err != nil {
		return nil, err
	}

	res, err := resources(cfg.Deploy.Resources)
	if err != nil {
		return nil, fmt.Errorf("deploy.resources: %w", err)
	}

	c := &corev1.Container{
		Name: cfg.App,
		// Always the digest reference: a restarted pod pulls identical bytes.
		Image:           rel.Ref(),
		ImagePullPolicy: imagePullPolicy(cfg),
		Ports: []corev1.ContainerPort{{
			Name:          "http",
			ContainerPort: cfg.Deploy.Port,
			Protocol:      corev1.ProtocolTCP,
		}},
		Env:       env,
		Resources: res,
		// Three probes, three jobs. Kubernetes will not run liveness or
		// readiness until startup succeeds. Readiness gates traffic and the
		// rollout. Liveness restarts a wedged process and must stay cheap.
		ReadinessProbe: t.probe(cfg, cfg.Deploy.Healthcheck.Readiness),
		LivenessProbe:  livenessFrom(t.probe(cfg, cfg.Deploy.Healthcheck.Liveness)),
		StartupProbe:   startupFrom(t.probe(cfg, cfg.Deploy.Healthcheck.Startup), cfg),
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr(false),
			ReadOnlyRootFilesystem:   ptr(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		// Give the app a window to finish in-flight requests before SIGTERM.
		Lifecycle: &corev1.Lifecycle{
			PreStop: &corev1.LifecycleHandler{
				Sleep: &corev1.SleepAction{Seconds: preStopSeconds(cfg)},
			},
		},
	}

	if len(cfg.Deploy.Command) > 0 {
		c.Command = cfg.Deploy.Command
	}

	// Mount whole pre-existing Secrets by reference.
	for _, name := range cfg.Env.SecretRefs {
		c.EnvFrom = append(c.EnvFrom, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: name},
			},
		})
	}
	if len(cfg.Env.Secret) > 0 {
		c.EnvFrom = append(c.EnvFrom, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: release.ObjectName(cfg.App, "env"),
				},
			},
		})
	}

	return c, nil
}

// envVars renders clear environment plus the standard injected variables.
func (t *Target) envVars(cfg *config.Config, rel release.Release) ([]corev1.EnvVar, error) {
	// Injected first so a user-set value in env.clear can override them.
	injected := map[string]string{
		"PORT":          fmt.Sprintf("%d", cfg.Deploy.Port),
		"BUIDL_ENV":     rel.Environment,
		"BUIDL_RELEASE": rel.ID,
		"BUIDL_APP":     cfg.App,
	}
	if rel.Git.SHA != "" {
		injected["BUIDL_GIT_SHA"] = rel.Git.SHA
	}

	merged := map[string]string{}
	for k, v := range injected {
		merged[k] = v
	}
	for k, v := range cfg.Env.Clear {
		merged[k] = v
	}

	names := make([]string, 0, len(merged))
	for k := range merged {
		names = append(names, k)
	}
	// Deterministic order keeps the pod template stable, so an unchanged config
	// produces no spurious rollout.
	sort.Strings(names)

	out := make([]corev1.EnvVar, 0, len(names)+1)
	for _, k := range names {
		out = append(out, corev1.EnvVar{Name: k, Value: merged[k]})
	}

	// Expose the pod's own name; useful for request logs and tracing.
	out = append(out, corev1.EnvVar{
		Name: "BUIDL_INSTANCE",
		ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
		},
	})

	return out, nil
}

// probe builds one probe from the healthcheck config. path is the HTTP
// endpoint for this probe; exec healthchecks ignore it.
func (t *Target) probe(cfg *config.Config, path string) *corev1.Probe {
	hc := cfg.Deploy.Healthcheck

	p := &corev1.Probe{
		InitialDelaySeconds: hc.InitialDelaySeconds,
		PeriodSeconds:       hc.PeriodSeconds,
		TimeoutSeconds:      hc.TimeoutSeconds,
		FailureThreshold:    hc.FailureThreshold,
		SuccessThreshold:    1,
	}

	if len(hc.Command) > 0 {
		p.Exec = &corev1.ExecAction{Command: hc.Command}
		return p
	}

	p.HTTPGet = &corev1.HTTPGetAction{
		Path:   path,
		Port:   probePort(cfg),
		Scheme: corev1.URISchemeHTTP,
	}
	return p
}

// probePort prefers the container's named "http" port so a port change does
// not leave probes pointed at a stale number. A healthcheck.port that differs
// from deploy.port is an explicit override and stays numeric.
func probePort(cfg *config.Config) intstr.IntOrString {
	if cfg.Deploy.Healthcheck.Port != 0 && cfg.Deploy.Healthcheck.Port != cfg.Deploy.Port {
		return intstr.FromInt32(cfg.Deploy.Healthcheck.Port)
	}
	return intstr.FromString("http")
}

// livenessFrom derives a liveness probe from readiness.
//
// It is deliberately more forgiving than readiness: a liveness failure kills the
// container, so a transient blip should remove a pod from the load balancer
// (readiness) long before it triggers a restart.
func livenessFrom(readiness *corev1.Probe) *corev1.Probe {
	if readiness == nil {
		return nil
	}
	l := readiness.DeepCopy()
	l.FailureThreshold = readiness.FailureThreshold * 2
	if l.FailureThreshold < 6 {
		l.FailureThreshold = 6
	}
	l.PeriodSeconds = 20
	l.SuccessThreshold = 1
	return l
}

// startupFrom derives a startup probe that tolerates a slow boot without
// delaying steady-state failure detection.
func startupFrom(readiness *corev1.Probe, cfg *config.Config) *corev1.Probe {
	if readiness == nil {
		return nil
	}
	s := readiness.DeepCopy()
	s.PeriodSeconds = 2
	s.InitialDelaySeconds = 0
	// A timeout longer than the period overlaps probes. Startup checks
	// must stay cheap; the failure budget covers a slow boot, not a
	// hung request.
	if s.TimeoutSeconds > s.PeriodSeconds {
		s.TimeoutSeconds = s.PeriodSeconds
	}
	// Allow up to the deploy timeout for the process to come up at all.
	budget := cfg.Deploy.DeployTimeout.Or(defaultDeploy).Seconds()
	s.FailureThreshold = int32(budget / 2)
	if s.FailureThreshold < 10 {
		s.FailureThreshold = 10
	}
	s.SuccessThreshold = 1
	return s
}

// preStopSeconds is how long to wait after SIGTERM is scheduled before the
// process is signalled, giving load balancers time to stop routing to this pod.
func preStopSeconds(cfg *config.Config) int64 {
	drain := int64(cfg.Deploy.DrainTimeout.Or(defaultDrain).Seconds())
	// A few seconds covers endpoint propagation; more just slows deploys.
	if drain > 5 {
		return 5
	}
	if drain < 1 {
		return 1
	}
	return drain
}

// topologySpread spreads pods across nodes, best-effort.
func topologySpread(cfg *config.Config, rel release.Release) []corev1.TopologySpreadConstraint {
	if replicas(cfg) < 2 {
		return nil
	}
	return []corev1.TopologySpreadConstraint{{
		MaxSkew:     1,
		TopologyKey: "kubernetes.io/hostname",
		// ScheduleAnyway, not DoNotSchedule: on a single-node cluster (kind,
		// minikube, a small bare-metal box) DoNotSchedule would leave pods
		// permanently Pending.
		WhenUnsatisfiable: corev1.ScheduleAnyway,
		LabelSelector:     &metav1.LabelSelector{MatchLabels: selectorLabels(cfg, rel)},
	}}
}

// service renders the ClusterIP Service that fronts the app.
func (t *Target) service(cfg *config.Config, rel release.Release) Object {
	name := release.ObjectName(cfg.App)

	selector := selectorLabels(cfg, rel)
	if cfg.Deploy.Strategy.Type == config.StrategyBlueGreen {
		// The selector pins one release. Flipping this single field is the
		// atomic cutover.
		selector[release.LabelRelease] = rel.ID
	}

	svc := &corev1.Service{
		ObjectMeta: t.objectMeta(cfg, rel, name),
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: selector,
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       80,
				TargetPort: intstr.FromString("http"),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}

	return Object{
		GVK:        corev1.SchemeGroupVersion.WithKind("Service"),
		Name:       name,
		Kind:       "Service",
		Object:     svc,
		Namespaced: true,
		Order:      orderService,
	}
}

// ingress renders external routing.
func (t *Target) ingress(cfg *config.Config, rel release.Release) (*Object, error) {
	hosts := hostList(cfg)
	if len(hosts) == 0 {
		return nil, fmt.Errorf("proxy is enabled but no host is configured")
	}

	name := release.ObjectName(cfg.App)
	meta := t.objectMeta(cfg, rel, name)
	if meta.Annotations == nil {
		meta.Annotations = map[string]string{}
	}
	for k, v := range cfg.Proxy.Annotations {
		meta.Annotations[k] = v
	}
	if cfg.Proxy.SSL {
		// cert-manager watches this annotation to issue and renew the cert.
		meta.Annotations["cert-manager.io/cluster-issuer"] = cfg.Proxy.ClusterIssuer
	}

	pathType := networkingv1.PathTypePrefix
	rules := make([]networkingv1.IngressRule, 0, len(hosts))
	for _, host := range hosts {
		rules = append(rules, networkingv1.IngressRule{
			Host: host,
			IngressRuleValue: networkingv1.IngressRuleValue{
				HTTP: &networkingv1.HTTPIngressRuleValue{
					Paths: []networkingv1.HTTPIngressPath{{
						Path:     "/",
						PathType: &pathType,
						Backend: networkingv1.IngressBackend{
							Service: &networkingv1.IngressServiceBackend{
								Name: release.ObjectName(cfg.App),
								Port: networkingv1.ServiceBackendPort{Number: 80},
							},
						},
					}},
				},
			},
		})
	}

	spec := networkingv1.IngressSpec{Rules: rules}
	if cfg.Proxy.Class != "" {
		spec.IngressClassName = &cfg.Proxy.Class
	}
	if cfg.Proxy.SSL {
		spec.TLS = []networkingv1.IngressTLS{{
			Hosts: hosts,
			// One secret per app+env; cert-manager populates it.
			SecretName: release.ObjectName(cfg.App, "tls"),
		}}
	}

	ing := &networkingv1.Ingress{ObjectMeta: meta, Spec: spec}
	return &Object{
		GVK:        networkingv1.SchemeGroupVersion.WithKind("Ingress"),
		Name:       name,
		Kind:       "Ingress",
		Object:     ing,
		Namespaced: true,
		Order:      orderIngress,
	}, nil
}

// hostList returns the deduplicated external hostnames.
func hostList(cfg *config.Config) []string {
	seen := map[string]bool{}
	var hosts []string
	for _, h := range append([]string{cfg.Proxy.Host}, cfg.Proxy.Hosts...) {
		h = strings.TrimSpace(h)
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		hosts = append(hosts, h)
	}
	return hosts
}

// hpa renders the HorizontalPodAutoscaler.
func (t *Target) hpa(cfg *config.Config, rel release.Release) Object {
	as := cfg.Deploy.Autoscale
	name := release.ObjectName(cfg.App)

	var metrics []autoscalingv2.MetricSpec
	if as.CPUPercent > 0 {
		metrics = append(metrics, utilizationMetric(corev1.ResourceCPU, as.CPUPercent))
	}
	if as.MemoryPercent > 0 {
		metrics = append(metrics, utilizationMetric(corev1.ResourceMemory, as.MemoryPercent))
	}

	h := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: t.objectMeta(cfg, rel, name),
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       t.workloadName(cfg, rel),
			},
			MinReplicas: ptr(as.Min),
			MaxReplicas: as.Max,
			Metrics:     metrics,
		},
	}

	return Object{
		GVK:        autoscalingv2.SchemeGroupVersion.WithKind("HorizontalPodAutoscaler"),
		Name:       name,
		Kind:       "HorizontalPodAutoscaler",
		Object:     h,
		Namespaced: true,
		Order:      orderScaling,
	}
}

func utilizationMetric(name corev1.ResourceName, target int32) autoscalingv2.MetricSpec {
	return autoscalingv2.MetricSpec{
		Type: autoscalingv2.ResourceMetricSourceType,
		Resource: &autoscalingv2.ResourceMetricSource{
			Name: name,
			Target: autoscalingv2.MetricTarget{
				Type:               autoscalingv2.UtilizationMetricType,
				AverageUtilization: ptr(target),
			},
		},
	}
}

// pdb keeps at least one pod available during voluntary disruptions such as node
// drains and cluster upgrades.
func (t *Target) pdb(cfg *config.Config, rel release.Release) Object {
	name := release.ObjectName(cfg.App)
	minAvailable := intstr.FromInt32(1)

	p := &policyv1.PodDisruptionBudget{
		ObjectMeta: t.objectMeta(cfg, rel, name),
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvailable,
			Selector:     &metav1.LabelSelector{MatchLabels: selectorLabels(cfg, rel)},
		},
	}

	return Object{
		GVK:        policyv1.SchemeGroupVersion.WithKind("PodDisruptionBudget"),
		Name:       name,
		Kind:       "PodDisruptionBudget",
		Object:     p,
		Namespaced: true,
		Order:      orderScaling,
	}
}

// resources converts config quantity strings into a Kubernetes requirements
// block.
func resources(r config.Resources) (corev1.ResourceRequirements, error) {
	out := corev1.ResourceRequirements{}

	convert := func(m map[string]string) (corev1.ResourceList, error) {
		if len(m) == 0 {
			return nil, nil
		}
		list := corev1.ResourceList{}
		for k, v := range m {
			q, err := resource.ParseQuantity(v)
			if err != nil {
				return nil, fmt.Errorf("%s=%q: %w", k, v, err)
			}
			list[corev1.ResourceName(k)] = q
		}
		return list, nil
	}

	req, err := convert(r.Requests)
	if err != nil {
		return out, err
	}
	lim, err := convert(r.Limits)
	if err != nil {
		return out, err
	}
	out.Requests = req
	out.Limits = lim
	return out, nil
}

// ptr returns a pointer to v. Kubernetes API types use pointers to distinguish
// "unset" from "zero", which matters for fields like replicas.
func ptr[T any](v T) *T { return &v }
