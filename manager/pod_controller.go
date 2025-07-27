package manager

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net/netip"
	"net/url"
	"os"
	"path"

	nodev1 "github.com/ctrox/zeropod/api/node/v1"
	v1 "github.com/ctrox/zeropod/api/runtime/v1"
	shimv1 "github.com/ctrox/zeropod/api/shim/v1"
	"github.com/ctrox/zeropod/socket"
	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

func NewPodController(ctx context.Context, mgr manager.Manager, log *slog.Logger) error {
	ctrl.SetLogger(logr.FromSlogHandler(log.Handler()))

	pr, err := newPodReconciler(mgr.GetClient(), log)
	if err != nil {
		return err
	}
	c, err := controller.New("pod-controller", mgr, controller.Options{
		Reconciler: pr,
	})
	if err != nil {
		return err
	}
	return c.Watch(source.Kind(
		mgr.GetCache(), &corev1.Pod{}, &handler.TypedEnqueueRequestForObject[*corev1.Pod]{},
		predicate.NewTypedPredicateFuncs[*corev1.Pod](func(pod *corev1.Pod) bool {
			return isZeropod(pod)
		}),
	))
}

type podReconciler struct {
	kube     client.Client
	log      *slog.Logger
	nodeName string
	tracker  socket.Tracker
}

func newPodReconciler(kube client.Client, log *slog.Logger) (*podReconciler, error) {
	nodeName, ok := os.LookupEnv(nodev1.NodeNameEnvKey)
	if !ok {
		return nil, fmt.Errorf("could not find node name, env %s is not set", nodev1.NodeNameEnvKey)
	}
	tracker, err := socket.NewEBPFTracker()
	if err != nil {
		return nil, err
	}
	return &podReconciler{
		log:      log,
		kube:     kube,
		nodeName: nodeName,
		tracker:  tracker,
	}, nil
}

func (r *podReconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	log := r.log.With("req", request)

	if err := r.kube.Get(ctx, request.NamespacedName, &v1.Migration{}); err == nil {
		// migration already exists, there's nothing for us to do
		return reconcile.Result{}, nil
	}

	pod := &corev1.Pod{}
	if err := r.kube.Get(ctx, request.NamespacedName, pod); err != nil {
		if errors.IsNotFound(err) {
			// pod is already gone, it's too late to migrate it
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	// pass the pod IP to the tracker so it can ignore kubelet probes going to
	// this pod.
	netIP, err := netip.ParseAddr(pod.Status.PodIP)
	if err == nil {
		// TODO: support ipv6-only pods
		podIPv4 := netIP.As4()
		if err := r.tracker.PutPodIP(binary.NativeEndian.Uint32(podIPv4[:])); err != nil {
			// log error but continue as we might want to do other things
			log.Error("putting pod IP in tracker map", "error", err)
		}
	}

	if !r.isMigratable(pod) {
		return reconcile.Result{}, nil
	}

	migration, err := newMigration(pod)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("initializing migration object: %w", err)
	}
	if err := r.kube.Create(ctx, migration); err != nil {
		if !errors.IsAlreadyExists(err) {
			return reconcile.Result{}, fmt.Errorf("creating migration object: %w", err)
		}
	}
	log.Info("created migration for pod", "pod_name", pod.Name, "pod_namespace", pod.Namespace)

	for _, container := range migration.Spec.Containers {
		migration.Status.Containers = append(migration.Status.Containers, v1.MigrationContainerStatus{
			Name: container.Name,
			Condition: v1.MigrationCondition{
				Phase: v1.MigrationPhasePending,
			},
		})
	}
	if err := r.kube.Status().Update(ctx, migration); err != nil {
		// updating the status to pending is just cosmetic, we don't want to
		// slow down the process by retrying here.
		log.Warn("setting migration status failed, ignoring")
		return reconcile.Result{}, nil
	}

	return reconcile.Result{}, nil
}

func (r podReconciler) isMigratable(pod *corev1.Pod) bool {
	// some of these are already handled by the cache/predicate but there's no
	// harm in being sure.
	if pod.Spec.NodeName != r.nodeName {
		return false
	}

	if !isZeropod(pod) {
		return false
	}

	if pod.DeletionTimestamp == nil {
		return false
	}

	if !anyMigrationEnabled(pod) {
		return false
	}

	if !hasScaledDownContainer(pod) && !liveMigrationEnabled(pod) {
		r.log.Info("skipping pod with no scaled down containers and live migration disabled",
			"pod_name", pod.Name, "pod_namespace", pod.Namespace)
		return false
	}

	return true
}

func isZeropod(pod *corev1.Pod) bool {
	return pod.Spec.RuntimeClassName != nil && *pod.Spec.RuntimeClassName == v1.RuntimeClassName
}

func newMigration(pod *corev1.Pod) (*v1.Migration, error) {
	containers := []v1.MigrationContainer{}
	for _, container := range pod.Status.ContainerStatuses {
		u, err := url.Parse(container.ContainerID)
		if err != nil {
			return nil, fmt.Errorf("unable to parse container ID %s", container.ContainerID)
		}
		containers = append(containers, v1.MigrationContainer{
			Name: container.Name,
			ID:   u.Host,
		})
	}

	return &v1.Migration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
		Spec: v1.MigrationSpec{
			SourcePod:       pod.Name,
			SourceNode:      pod.Spec.NodeName,
			PodTemplateHash: pod.Labels[appsv1.DefaultDeploymentUniqueLabelKey],
			Containers:      containers,
		},
	}, nil
}

func hasScaledDownContainer(pod *corev1.Pod) bool {
	for _, container := range pod.Spec.Containers {
		if k, ok := pod.Labels[path.Join(StatusLabelKeyPrefix, container.Name)]; ok {
			if k == shimv1.ContainerPhase_SCALED_DOWN.String() {
				return true
			}
		}
	}
	return false
}

func anyMigrationEnabled(pod *corev1.Pod) bool {
	_, migrate := pod.Annotations[nodev1.MigrateAnnotationKey]
	_, liveMigrate := pod.Annotations[nodev1.LiveMigrateAnnotationKey]
	return migrate || liveMigrate
}

func liveMigrationEnabled(pod *corev1.Pod) bool {
	_, ok := pod.Annotations[nodev1.LiveMigrateAnnotationKey]
	return ok
}
