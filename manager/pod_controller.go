package manager

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"

	nodev1 "github.com/ctrox/zeropod/api/node/v1"
	v1 "github.com/ctrox/zeropod/api/runtime/v1"
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
}

func newPodReconciler(kube client.Client, log *slog.Logger) (*podReconciler, error) {
	nodeName, ok := os.LookupEnv(nodev1.NodeNameEnvKey)
	if !ok {
		return nil, fmt.Errorf("could not find node name, env %s is not set", nodev1.NodeNameEnvKey)
	}
	return &podReconciler{
		log:      log,
		kube:     kube,
		nodeName: nodeName,
	}, nil
}

func (r *podReconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
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

	if r.isMigrationSource(pod) {
		if err := r.prepareMigrationSource(ctx, pod); err != nil {
			return reconcile.Result{}, fmt.Errorf("preparing migration source: %w", err)
		}
	}

	if r.isMigrationTarget(pod) {
		requeue, err := r.prepareMigrationTarget(ctx, pod)
		if err != nil {
			return reconcile.Result{}, fmt.Errorf("preparing migration target: %w", err)
		}
		return reconcile.Result{Requeue: requeue}, nil
	}

	return reconcile.Result{}, nil
}

func (r podReconciler) isMigrationSource(pod *corev1.Pod) bool {
	return r.isMigrationCandidate(pod) && pod.DeletionTimestamp != nil
}

func (r podReconciler) isMigrationTarget(pod *corev1.Pod) bool {
	return r.isMigrationCandidate(pod) &&
		pod.DeletionTimestamp == nil &&
		pod.Spec.NodeName != "" &&
		pod.Status.Phase == corev1.PodPending
}

func (r podReconciler) isMigrationCandidate(pod *corev1.Pod) bool {
	// some of these are already handled by the cache/predicate but there's no
	// harm in being sure.
	if pod.Spec.NodeName != r.nodeName {
		return false
	}

	if !isZeropod(pod) {
		return false
	}

	return anyMigrationEnabled(pod)
}

func (r podReconciler) prepareMigrationSource(ctx context.Context, pod *corev1.Pod) error {
	log := r.log.With("pod_name", pod.Name, "pod_namespace", pod.Namespace)
	migration, err := newMigration(pod)
	if err != nil {
		return fmt.Errorf("initializing migration object: %w", err)
	}
	if err := r.kube.Create(ctx, migration); err != nil {
		if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("creating migration object: %w", err)
		}
	}
	log.Info("created migration for pod")

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
		return nil
	}
	return nil
}

func (r podReconciler) prepareMigrationTarget(ctx context.Context, pod *corev1.Pod) (requeue bool, err error) {
	log := r.log.With("pod_name", pod.Name, "pod_namespace", pod.Namespace)
	migrationList := &v1.MigrationList{}
	if err := r.kube.List(ctx, migrationList, client.InNamespace(pod.Namespace)); err != nil {
		return false, err
	}
	for _, mig := range migrationList.Items {
		if mig.ClaimedAndMatchesPod(pod) {
			log.Debug("no need to claim migration in controller, already claimed")
			return false, nil
		}
		if mig.MatchesPod(pod) && !mig.Claimed() {
			mig.Claim(pod.Name, pod.Spec.NodeName)
			if err := r.kube.Update(ctx, &mig); err != nil {
				if errors.IsConflict(err) {
					log.Warn("conflict claiming migration, requeueing")
					return true, nil
				}
				return false, fmt.Errorf("failed to claim migration: %w", err)
			}
			log.Info("claimed migration in controller")
			return false, nil
		}
	}
	return false, nil
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

func anyMigrationEnabled(pod *corev1.Pod) bool {
	_, migrate := pod.Annotations[nodev1.MigrateAnnotationKey]
	_, liveMigrate := pod.Annotations[nodev1.LiveMigrateAnnotationKey]
	return migrate || liveMigrate
}

func liveMigrationEnabled(pod *corev1.Pod) bool {
	_, ok := pod.Annotations[nodev1.LiveMigrateAnnotationKey]
	return ok
}
