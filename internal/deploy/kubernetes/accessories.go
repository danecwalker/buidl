package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/danewalker/buidl/internal/config"
	"github.com/danewalker/buidl/internal/deploy"
	"github.com/danewalker/buidl/internal/release"
)

// Accessories are supporting stateful services — a database, a cache, a queue —
// that live alongside the app.
//
// They are rendered here and nowhere else, and they are applied only by
// ApplyAccessories. Render, and therefore Plan, Deploy and Preflight, never see
// them. That separation is the whole feature: an app deploy runs many times a
// day and must be free to replace every pod it owns, and the one thing that must
// never happen as a side effect of shipping a web app is a restarted database.
// Folding accessories into Render would make "reconcile the database" the
// default rather than a decision, and the failure mode — a Postgres pod cycled
// because someone changed a log level — is unrecoverable in a way an app rollout
// never is.
//
// The cost of that split is real and worth stating: an accessory can drift from
// its configuration until someone asks buidl to reconcile it. Drift you can see
// in a plan is a better trade than an implicit restart you cannot undo.
const (
	// accessoryComponent marks accessory objects so they are recognizably
	// buidl-managed but never selected by the app's Service, PDB or spread
	// constraints.
	accessoryComponent = "accessory"

	// accessoryVolumeName is the volumeClaimTemplate name. It becomes part of the
	// PVC name (<volume>-<pod>), so it is short and stable: changing it would
	// orphan every existing volume.
	accessoryVolumeName = "data"

	// accessoryPortName must fit an IANA_SVC_NAME (15 characters), which the
	// accessory's own name is not guaranteed to.
	accessoryPortName = "tcp"

	// accessoryReplicas is fixed at one. A second replica of the same config is a
	// second Postgres primary pointed at its own volume, not a cluster — real
	// replication needs an operator, not a replica count.
	accessoryReplicas = int32(1)
)

// RenderAccessories builds the desired state for every configured accessory.
//
// Unlike Render this does not require a digest-pinned release: an accessory
// image is written by hand in buidl.yaml and is not part of the app's release
// identity, so there is nothing for buidl to pin it to.
func (t *Target) RenderAccessories(req deploy.Request) ([]Object, error) {
	cfg := req.Config

	// Map iteration order would otherwise shuffle the apply sequence between
	// runs, making two identical runs report different orderings.
	names := make([]string, 0, len(cfg.Accessories))
	for name := range cfg.Accessories {
		names = append(names, name)
	}
	sort.Strings(names)

	var objs []Object
	for _, name := range names {
		acc := cfg.Accessories[name]

		secret, checksum, err := t.accessorySecret(cfg, req.Release, name, acc, req.Secrets)
		if err != nil {
			return nil, err
		}
		if secret != nil {
			objs = append(objs, *secret)
		}

		objs = append(objs, t.accessoryService(cfg, req.Release, name, acc))

		set, err := t.accessoryStatefulSet(cfg, req.Release, name, acc, checksum)
		if err != nil {
			return nil, err
		}
		objs = append(objs, *set)
	}

	sort.SliceStable(objs, func(i, j int) bool { return objs[i].Order < objs[j].Order })
	return objs, nil
}

// PlanAccessories dry-runs the accessories against the API server.
//
// Applying anything to a database without being able to see the change first
// would be the least defensible operation this tool performs, so the explicit
// path gets the same server-side dry run the app plan does.
func (t *Target) PlanAccessories(ctx context.Context, req deploy.Request) (*deploy.Plan, error) {
	objs, err := t.RenderAccessories(req)
	if err != nil {
		return nil, err
	}

	plan := &deploy.Plan{Environment: req.Config.Environment, Release: req.Release}
	for _, obj := range objs {
		change, err := t.planObject(ctx, obj, accessoryReplicas)
		if err != nil {
			return nil, err
		}
		plan.Changes = append(plan.Changes, change)
	}
	return plan, nil
}

// ApplyAccessories reconciles the configured accessories.
//
// Nothing else in this package calls it, and that is deliberate — see the
// comment at the top of this file. It exists so reconciling an accessory is
// always something a user asked for by name.
func (t *Target) ApplyAccessories(ctx context.Context, req deploy.Request) ([]deploy.Change, error) {
	objs, err := t.RenderAccessories(req)
	if err != nil {
		return nil, err
	}
	if len(objs) == 0 {
		return nil, nil
	}

	t.log.Step("Applying accessories")
	return t.applyAll(ctx, objs, accessoryReplicas)
}

// accessoryName is the object name shared by an accessory's StatefulSet and its
// headless Service.
//
// Every accessory name is prefixed with the app's, so `web` and its `postgres`
// accessory are `web` and `web-postgres`. Collision with the app's own objects
// is impossible because config validation requires a non-empty accessory name,
// and Kubernetes names only collide within one kind.
func accessoryName(cfg *config.Config, name string) string {
	return release.ObjectName(cfg.App, name)
}

// accessoryLabels identify an accessory as buidl-managed while keeping it out of
// every selector the app owns.
//
// The name and instance labels must differ from the app's: the app's Service,
// PodDisruptionBudget and topology spread constraints all select on them, and an
// accessory that matched would put a database behind the app's Service and
// route HTTP traffic to Postgres.
func accessoryLabels(cfg *config.Config, rel release.Release, name string) map[string]string {
	l := rel.Labels(cfg.App)
	l[release.LabelName] = accessoryName(cfg, name)
	l[release.LabelInstance] = accessoryName(cfg, name) + "-" + rel.Environment
	l[release.LabelComponent] = accessoryComponent
	// The version label carries the app's git version, which says nothing about
	// the accessory image and changes on every commit — on a pod template that
	// would restart the database for a change to unrelated source code.
	delete(l, release.LabelVersion)
	return l
}

// accessorySelector is the immutable subset used for pod selection. A
// StatefulSet's selector cannot be changed after creation, so nothing that
// varies per release may appear here.
func accessorySelector(cfg *config.Config, rel release.Release, name string) map[string]string {
	return map[string]string{
		release.LabelName:      accessoryName(cfg, name),
		release.LabelInstance:  accessoryName(cfg, name) + "-" + rel.Environment,
		release.LabelComponent: accessoryComponent,
	}
}

// accessoryMeta builds object metadata for an accessory.
func (t *Target) accessoryMeta(cfg *config.Config, rel release.Release, name, objectName string) metav1.ObjectMeta {
	ann := rel.Annotations()
	// The release ID and image digest describe the app. Recording them on an
	// accessory would claim it was shipped as part of a release that will never
	// roll it, which is exactly the confusion this feature has to avoid.
	delete(ann, release.AnnotationRelease)
	delete(ann, release.AnnotationDigest)

	return metav1.ObjectMeta{
		Name:        objectName,
		Namespace:   t.Namespace,
		Labels:      accessoryLabels(cfg, rel, name),
		Annotations: ann,
	}
}

// accessorySecret renders an accessory's environment Secret and a checksum of
// its contents.
//
// Values live in a Secret for the same reason the app's do, and the checksum is
// annotated onto the pod template so a changed credential actually reaches the
// running process instead of sitting unread in the Secret.
func (t *Target) accessorySecret(cfg *config.Config, rel release.Release, name string, acc config.Accessory, values map[string]string) (*Object, string, error) {
	if len(acc.Env.Secret) == 0 {
		return nil, "", nil
	}

	// Sorted for a stable checksum across runs.
	keys := append([]string(nil), acc.Env.Secret...)
	sort.Strings(keys)

	data := map[string][]byte{}
	h := sha256.New()
	var missing []string
	for _, key := range keys {
		v, ok := values[key]
		if !ok {
			missing = append(missing, key)
			continue
		}
		data[key] = []byte(v)
		fmt.Fprintf(h, "%s=%s\n", key, v)
	}

	// Preflight only checks the app's declared secrets, so without this an
	// accessory would be created with no POSTGRES_PASSWORD and fail to boot for
	// reasons visible only in the container's logs.
	if len(missing) > 0 {
		return nil, "", fmt.Errorf("accessory %q requires secret(s) not set in the environment: %s\n\nhint: export them, or remove them from accessories.%s.env.secret in buidl.yaml",
			name, strings.Join(missing, ", "), name)
	}

	objectName := release.ObjectName(cfg.App, name, "env")
	sec := &corev1.Secret{
		ObjectMeta: t.accessoryMeta(cfg, rel, name, objectName),
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
	return &Object{
		GVK:        corev1.SchemeGroupVersion.WithKind("Secret"),
		Name:       objectName,
		Kind:       "Secret",
		Object:     sec,
		Namespaced: true,
		Order:      orderSecret,
	}, hex.EncodeToString(h.Sum(nil))[:16], nil
}

// accessoryService renders the headless Service.
//
// Headless (clusterIP: None) is what pairs with the StatefulSet: it gives the
// pod a stable DNS name — postgres-0.web-postgres.<namespace>.svc — that
// survives rescheduling. A normal ClusterIP would load-balance across replicas
// and hand out an address that says nothing about which instance answered,
// which is useless for a primary you must address directly.
func (t *Target) accessoryService(cfg *config.Config, rel release.Release, name string, acc config.Accessory) Object {
	objectName := accessoryName(cfg, name)

	svc := &corev1.Service{
		ObjectMeta: t.accessoryMeta(cfg, rel, name, objectName),
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Selector:  accessorySelector(cfg, rel, name),
		},
	}
	// A headless Service is legal with no ports, and an accessory with no
	// declared port still needs one for stable pod DNS.
	if acc.Port != 0 {
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:       accessoryPortName,
			Port:       acc.Port,
			TargetPort: intstr.FromString(accessoryPortName),
			Protocol:   corev1.ProtocolTCP,
		}}
	}

	return Object{
		GVK:        corev1.SchemeGroupVersion.WithKind("Service"),
		Name:       objectName,
		Kind:       "Service",
		Object:     svc,
		Namespaced: true,
		Order:      orderService,
	}
}

// accessoryStatefulSet renders the workload.
//
// A StatefulSet rather than a Deployment because both properties it provides are
// load-bearing here: a stable network identity to address the instance by, and a
// volume that is re-attached to the replacement pod rather than recreated empty.
func (t *Target) accessoryStatefulSet(cfg *config.Config, rel release.Release, name string, acc config.Accessory, secretChecksum string) (*Object, error) {
	objectName := accessoryName(cfg, name)

	container, err := accessoryContainer(cfg, name, acc)
	if err != nil {
		return nil, err
	}

	// Only the secret checksum goes on the pod template. Everything else the
	// release carries — the ID, the deploy timestamp — changes on every run, and
	// a changed pod template is a restarted database.
	var podAnnotations map[string]string
	if secretChecksum != "" {
		podAnnotations = map[string]string{release.AnnotationConfigSum: secretChecksum}
	}

	spec := appsv1.StatefulSetSpec{
		ServiceName: objectName,
		Replicas:    ptr(accessoryReplicas),
		Selector:    &metav1.LabelSelector{MatchLabels: accessorySelector(cfg, rel, name)},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels:      accessoryLabels(cfg, rel, name),
				Annotations: podAnnotations,
			},
			Spec: corev1.PodSpec{
				Containers:   []corev1.Container{*container},
				NodeSelector: cfg.Deploy.Kubernetes.NodeSelector,
				// The app's pull secrets cover the accessory too: a private mirror
				// of postgres:16 needs the same credential the app image does.
				ImagePullSecrets: pullSecretRefs(cfg),
			},
		},
	}

	if acc.Storage != "" {
		size, err := resource.ParseQuantity(acc.Storage)
		if err != nil {
			return nil, fmt.Errorf("accessories.%s.storage %q: %w", name, acc.Storage, err)
		}
		claim := corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:   accessoryVolumeName,
				Labels: accessoryLabels(cfg, rel, name),
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				// ReadWriteOnce: one writer is the correct and only safe mode for a
				// database's data directory.
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: size},
				},
			},
		}
		if acc.StorageClass != "" {
			claim.Spec.StorageClassName = ptr(acc.StorageClass)
		}
		spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{claim}
	}

	set := &appsv1.StatefulSet{
		ObjectMeta: t.accessoryMeta(cfg, rel, name, objectName),
		Spec:       spec,
	}

	return &Object{
		GVK:        appsv1.SchemeGroupVersion.WithKind("StatefulSet"),
		Name:       objectName,
		Kind:       "StatefulSet",
		Object:     set,
		Namespaced: true,
		Order:      orderWorkload,
	}, nil
}

// accessoryContainer renders the accessory's container.
func accessoryContainer(cfg *config.Config, name string, acc config.Accessory) (*corev1.Container, error) {
	res, err := resources(acc.Resources)
	if err != nil {
		return nil, fmt.Errorf("accessories.%s.resources: %w", name, err)
	}

	// The app's pod is forced non-root with all capabilities dropped. An accessory
	// deliberately is not: the canonical postgres, mysql and mongo images start as
	// root to initialize and chown their data directory, then drop to their own
	// user. Imposing runAsNonRoot or dropping CAP_CHOWN there produces a crash
	// loop on first boot, and the honest place to harden an accessory is the
	// image, not a policy buidl invents for it.
	c := &corev1.Container{
		Name: name,
		// Deployed exactly as written. buidl does not resolve an accessory image
		// to a digest: unlike the app image it was not built by this tool, and
		// re-resolving a tag on every apply would turn an unrelated upstream push
		// into a database restart.
		Image:     acc.Image,
		Command:   acc.Cmd,
		Env:       accessoryEnv(acc),
		Resources: res,
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr(false),
		},
	}

	if acc.Port != 0 {
		c.Ports = []corev1.ContainerPort{{
			Name:          accessoryPortName,
			ContainerPort: acc.Port,
			Protocol:      corev1.ProtocolTCP,
		}}
	}

	for _, ref := range acc.Env.SecretRefs {
		c.EnvFrom = append(c.EnvFrom, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: ref},
			},
		})
	}
	if len(acc.Env.Secret) > 0 {
		c.EnvFrom = append(c.EnvFrom, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: release.ObjectName(cfg.App, name, "env"),
				},
			},
		})
	}

	if acc.Storage != "" {
		c.VolumeMounts = []corev1.VolumeMount{{
			Name:      accessoryVolumeName,
			MountPath: acc.MountPath,
		}}
	}

	return c, nil
}

// accessoryEnv renders env.clear.
//
// None of the variables the app receives are injected here. BUIDL_RELEASE and
// its siblings change with every deploy, and a database whose pod template
// changes every deploy is a database that restarts every deploy.
func accessoryEnv(acc config.Accessory) []corev1.EnvVar {
	if len(acc.Env.Clear) == 0 {
		return nil
	}

	names := make([]string, 0, len(acc.Env.Clear))
	for k := range acc.Env.Clear {
		names = append(names, k)
	}
	// Deterministic order keeps the pod template stable, so an unchanged config
	// produces no restart.
	sort.Strings(names)

	out := make([]corev1.EnvVar, 0, len(names))
	for _, k := range names {
		out = append(out, corev1.EnvVar{Name: k, Value: acc.Env.Clear[k]})
	}
	return out
}
