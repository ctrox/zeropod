package manager

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	nodev1 "github.com/ctrox/zeropod/api/node/v1"
	v1 "github.com/ctrox/zeropod/api/runtime/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const checkpointCleanupFinalizer = "migration.runtime.zeropod.ctrox.dev/checkpoint-cleanup"

func NewMigrationController(ctx context.Context, mgr manager.Manager, log *slog.Logger) error {
	nodeName, ok := os.LookupEnv(nodev1.NodeNameEnvKey)
	if !ok {
		return fmt.Errorf("could not find node name, env %s is not set", nodev1.NodeNameEnvKey)
	}
	c, err := controller.New("migration-controller", mgr, controller.Options{
		Reconciler: newMigrationReconciler(mgr.GetClient(), log.With("controller", "migration"), nodeName),
	})
	if err != nil {
		return err
	}
	return c.Watch(source.Kind(
		mgr.GetCache(), &v1.Migration{}, &handler.TypedEnqueueRequestForObject[*v1.Migration]{},
		predicate.NewTypedPredicateFuncs[*v1.Migration](func(migration *v1.Migration) bool {
			return migration.Spec.SourceNode == nodeName
		}),
	))
}

type migrationReconciler struct {
	kube     client.Client
	log      *slog.Logger
	nodeName string
}

func newMigrationReconciler(kube client.Client, log *slog.Logger, nodeName string) *migrationReconciler {
	return &migrationReconciler{
		log:      log,
		kube:     kube,
		nodeName: nodeName,
	}
}

func (r *migrationReconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	migration := &v1.Migration{}
	if err := r.kube.Get(ctx, request.NamespacedName, migration); err != nil {
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("getting migration %w", err)
	}

	// should be handled by the predicate already but just to be sure
	if migration.Spec.SourceNode != r.nodeName {
		return reconcile.Result{}, nil
	}

	if !migration.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(migration, checkpointCleanupFinalizer) {
			if err := r.cleanupAllCheckpointImages(migration); err != nil {
				return reconcile.Result{}, err
			}

			controllerutil.RemoveFinalizer(migration, checkpointCleanupFinalizer)
			if err := r.kube.Update(ctx, migration); err != nil {
				return reconcile.Result{}, err
			}
		}
		return reconcile.Result{}, nil
	}

	if err := r.ensureFinalizers(ctx, migration); err != nil {
		return reconcile.Result{}, fmt.Errorf("ensuring finalizers %w", err)
	}

	return reconcile.Result{}, r.cleanupFinalCheckpointImages(migration)
}

func (r *migrationReconciler) ensureFinalizers(ctx context.Context, migration *v1.Migration) error {
	if !controllerutil.ContainsFinalizer(migration, checkpointCleanupFinalizer) {
		controllerutil.AddFinalizer(migration, checkpointCleanupFinalizer)
		if err := r.kube.Update(ctx, migration); err != nil {
			return err
		}
	}
	return nil
}

func (r *migrationReconciler) cleanupAllCheckpointImages(migration *v1.Migration) error {
	for _, container := range migration.Status.Containers {
		if err := r.cleanupCheckpointImage(migration, container.Name); err != nil {
			return err
		}
	}
	return nil
}

func (r *migrationReconciler) cleanupFinalCheckpointImages(migration *v1.Migration) error {
	for _, container := range migration.Status.Containers {
		if !container.Condition.Phase.Final() {
			continue
		}
		if err := r.cleanupCheckpointImage(migration, container.Name); err != nil {
			return err
		}
	}
	return nil
}

func (r *migrationReconciler) cleanupCheckpointImage(migration *v1.Migration, name string) error {
	for _, container := range migration.Spec.Containers {
		if name != container.Name {
			continue
		}
		if _, err := os.Stat(nodev1.ImagePath(container.ID)); err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return fmt.Errorf("cleaning up container image %s: %w", container.ID, err)
		}
		if err := os.RemoveAll(nodev1.ImagePath(container.ID)); err != nil {
			return fmt.Errorf("cleaning up container image %s: %w", container.ID, err)
		}
		r.log.Info("cleaned up container image", "name", container.Name, "id", container.ID)
	}
	return nil
}
