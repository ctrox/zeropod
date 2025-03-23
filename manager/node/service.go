// Package node provides the node RPC service to facilitate live migrations.
package node

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/checkpoint-restore/go-criu/v7/stats"
	"github.com/containerd/containerd/runtime/v2/shim"
	"github.com/containerd/ttrpc"
	nodev1 "github.com/ctrox/zeropod/api/node/v1"
	v1 "github.com/ctrox/zeropod/api/runtime/v1"
	"github.com/mholt/archives"
	"google.golang.org/protobuf/types/known/emptypb"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	ImagePathKey       = "image_path"
	ImageIDKey         = "image_id"
	ImageServerHostKey = "image_server_host"
	ImageServerPortKey = "image_server_port"
	PageServerHostKey  = "page_server_host"
	PageServerPortKey  = "page_server_port"
	OptPath            = "/opt/zeropod"

	pagesTransferTimeout = time.Minute * 5
	caCertFile           = "/tls/ca.crt"
	caKeyFile            = "/tls/ca.key"
	// tlsKeyFile  = "/run/tls.crt"
	// tlsCertFile = "/run/tls.key"
)

func nodeSocketAddress() string {
	return fmt.Sprintf("unix://%s", nodev1.SocketPath)
}

type Server struct {
	unixListener *net.UnixListener
	listener     net.Listener
	ttrpc        *ttrpc.Server
	kube         client.Client
	log          *slog.Logger
}

// NewServer starts a node server with two listeners:
// * Unix socket for the shims to connect to it locally
// * TCP+TLS socket to allow the other node instances to connect
func NewServer(addr string, kube client.Client, log *slog.Logger) (*Server, error) {
	host, ok := os.LookupEnv(nodev1.PodIPEnvKey)
	if !ok {
		return nil, fmt.Errorf("could not find host, env POD_IP is not set")
	}

	tlsConfig, err := initTLS(host)
	if err != nil {
		return nil, fmt.Errorf("initializing TLS certificates: %w", err)
	}

	listener, err := tls.Listen("tcp", addr, tlsConfig)
	if err != nil {
		return nil, fmt.Errorf("creating net listener: %w", err)
	}

	unixListener, err := setupUnixListener()
	if err != nil {
		return nil, fmt.Errorf("setting up unix listener: %w", err)
	}

	nodeName, ok := os.LookupEnv(nodev1.NodeNameEnvKey)
	if !ok {
		return nil, fmt.Errorf("could not find node name, env NODE_NAME is not set")
	}

	s, err := ttrpc.NewServer()
	if err != nil {
		return nil, fmt.Errorf("failed to create ttrpc server: %w", err)
	}

	nodev1.RegisterNodeService(s, &nodeService{
		kube:          kube,
		log:           log,
		host:          host,
		nodeName:      nodeName,
		port:          listener.Addr().(*net.TCPAddr).Port,
		tlsConfig:     tlsConfig,
		pageServerTLS: true,
	})

	return &Server{
		ttrpc:        s,
		unixListener: unixListener,
		listener:     listener,
		kube:         kube,
		log:          log,
	}, nil
}

func setupUnixListener() (*net.UnixListener, error) {
	socket := nodeSocketAddress()
	unixListener, err := shim.NewSocket(socket)
	if err != nil {
		if !shim.SocketEaddrinuse(err) {
			return nil, fmt.Errorf("listening to socket: %w", err)
		}

		if shim.CanConnect(socket) {
			return nil, fmt.Errorf("shim socket already exists, skipping server start")
		}

		if err := shim.RemoveSocket(socket); err != nil {
			return nil, fmt.Errorf("removing pre-existing socket: %w", err)
		}

		unixListener, err = shim.NewSocket(socket)
		if err != nil {
			return nil, fmt.Errorf("failed to create shim listener: %w", err)
		}
	}

	// write socket address to filesystem
	if err := shim.WriteAddress("shim_address", socket); err != nil {
		return nil, fmt.Errorf("failed to write shim address: %w", err)
	}

	return unixListener, nil
}

func (s *Server) Start(ctx context.Context) {
	defer func() {
		_ = s.ttrpc.Close()
		_ = s.unixListener.Close()
		_ = s.listener.Close()
		_ = os.Remove(nodeSocketAddress())
	}()
	go s.ttrpc.Serve(ctx, s.unixListener)
	go s.ttrpc.Serve(ctx, s.listener)

	s.log.Info("starting node server", "unix", s.unixListener.Addr(), "socket", s.listener.Addr())

	<-ctx.Done()
	s.log.Info("stopping node server")
}

// nodeService is a central RPC service running once per zeropod-node. It
// facilitates pod migration requests.
type nodeService struct {
	kube          client.Client
	log           *slog.Logger
	host          string
	nodeName      string
	port          int
	tlsConfig     *tls.Config
	pageServerTLS bool
}

var findMigrationBackoff = wait.Backoff{
	Steps:    10,
	Duration: 50 * time.Millisecond,
	Factor:   1.0,
	Jitter:   0.1,
}

func (ns *nodeService) Restore(ctx context.Context, req *nodev1.RestoreRequest) (*nodev1.RestoreResponse, error) {
	ns.log.Info("got restore request",
		"pod_name", req.PodInfo.Name,
		"pod_namespace", req.PodInfo.Namespace,
		"container_name", req.PodInfo.ContainerName)

	pod := &corev1.Pod{}
	nsName := types.NamespacedName{
		Name:      req.PodInfo.Name,
		Namespace: req.PodInfo.Namespace,
	}
	if err := ns.kube.Get(ctx, nsName, pod); err != nil {
		return nil, err
	}

	migration, err := ns.findMatchingMigration(ctx, pod)
	if err != nil {
		ns.log.Error("timeout waiting for matching migration", "error", err)
		return nil, fmt.Errorf("timeout waiting for matching migration: %w", err)
	}
	ns.log.Info("claimed migration", "name", migration.Name, "namespace", migration.Namespace)

	// wait for the page server to be set by the evac
	container := v1.MigrationContainer{}
	pCtx, cancel := context.WithTimeout(ctx, time.Second*10)
	defer cancel()
	if err := wait.PollUntilContextCancel(pCtx, time.Millisecond*10, true, func(ctx context.Context) (done bool, perr error) {
		if err := ns.kube.Get(ctx, client.ObjectKeyFromObject(migration), migration); err != nil {
			perr = err
			return
		}
		for _, ctr := range migration.Spec.Containers {
			if ctr.Name == req.PodInfo.ContainerName {
				if (migration.Spec.LiveMigration && ctr.PageServer != nil && ctr.ImageServer != nil) ||
					ctr.ImageServer != nil {
					container = ctr
					done = true
				}
			}
		}
		return
	}); err != nil {
		ns.log.Error("migration servers unset", "container_name", req.PodInfo.ContainerName)
		if err := ns.updateMigrationStatus(ctx, client.ObjectKeyFromObject(migration), func(m *v1.Migration) error {
			setOrUpdateContainerStatus(m, req.PodInfo.ContainerName, func(status *v1.MigrationContainerStatus) {
				status.Condition.Phase = v1.MigrationPhaseFailed
			})
			return nil
		}); err != nil {
			ns.log.Error("failed to update migration status", "container_name", req.PodInfo.ContainerName)
		}
		return nil, fmt.Errorf("migration servers are not set on migration: %s", migration.Name)
	}

	ns.log.Info("done waiting for migration servers", "container_name", req.PodInfo.ContainerName)

	if !ns.local(container.ImageServer.Host) {
		conn, err := tls.Dial("tcp", container.ImageServer.Address(), ns.tlsConfig)
		if err != nil {
			return nil, fmt.Errorf("dialing node service: %w", err)
		}
		nodeClient := nodev1.NewNodeClient(ttrpc.NewClient(conn))
		ns.log.Info("pulling image as it's not local",
			"remote_host", container.ImageServer.Host, "remote_port", container.ImageServer.Port)
		if err := ns.pullImage(ctx, nodeClient, container.ID); err != nil {
			ns.log.Error("pulling image", "error", err)
			return nil, err
		}
	}

	if err := os.Rename(nodev1.ImagePath(container.ID), nodev1.ImagePath(req.MigrationInfo.ImageId)); err != nil {
		ns.log.Error("renaming image path", "error", err)
		return nil, err
	}
	if migration.Spec.LiveMigration {
		ns.log.Info("starting page server for migration", "name", migration.Name, "namespace", migration.Namespace)

		if _, err := ns.NewCriuLazyPages(ctx, &nodev1.CriuLazyPagesRequest{
			Address:        container.PageServer.Host,
			Port:           int32(container.PageServer.Port),
			CheckpointPath: nodev1.SnapshotPath(req.MigrationInfo.ImageId),
			Tls:            ns.pageServerTLS,
		}); err != nil {
			return nil, fmt.Errorf("unable to start lazy pages daemon: %w", err)
		}
	}

	return &nodev1.RestoreResponse{
		MigrationInfo: &nodev1.MigrationInfo{
			ImageId:       req.MigrationInfo.ImageId,
			LiveMigration: migration.Spec.LiveMigration,
			Ports:         container.Ports,
		},
	}, nil
}

func (ns *nodeService) FinishRestore(ctx context.Context, req *nodev1.RestoreRequest) (*nodev1.RestoreResponse, error) {
	ns.log.Info("got finish restore request",
		"pod_name", req.PodInfo.Name,
		"pod_namespace", req.PodInfo.Namespace,
		"container_name", req.PodInfo.ContainerName)

	migrationList := &v1.MigrationList{}
	if err := ns.kube.List(ctx, migrationList, client.InNamespace(req.PodInfo.Namespace)); err != nil {
		return nil, err
	}
	var migration *v1.Migration
	for _, mig := range migrationList.Items {
		if mig.Spec.TargetPod == req.PodInfo.Name {
			migration = &mig
			break
		}
	}
	if migration == nil {
		return nil, fmt.Errorf("unable to find matching migration for pod %s", req.PodInfo.Name)
	}

	var restoreDur time.Duration
	phase := v1.MigrationPhaseCompleted
	if len(migration.Spec.Containers) > 0 && migration.Spec.LiveMigration {
		defer func() {
			if err := os.RemoveAll(nodev1.ImagePath(req.MigrationInfo.ImageId)); err != nil {
				ns.log.Error("cleaning up image path", "error", err)
			}
		}()
		restoreStats, err := getRestoreStats(req.MigrationInfo.ImageId)
		if err != nil {
			ns.log.Error("unable to read criu restore stats", "error", err)
			phase = v1.MigrationPhaseFailed
		} else {
			if restoreStats.RestoreTime != nil {
				restoreDur = time.Microsecond * time.Duration(*restoreStats.RestoreTime)
			}
			ns.log.Info("restore stats", "restore_dur", restoreDur, "pages_restored", restoreStats.PagesRestored)
		}
	}

	if err := ns.updateMigrationStatus(ctx, client.ObjectKeyFromObject(migration), func(migration *v1.Migration) error {
		setOrUpdateContainerStatus(migration, req.PodInfo.ContainerName, func(status *v1.MigrationContainerStatus) {
			// in case the migration is already set to failed we skip updating it
			if status.Condition.Phase == v1.MigrationPhaseFailed {
				return
			}
			if restoreDur != 0 {
				status.RestoredAt = metav1.NewMicroTime(req.MigrationInfo.RestoreEnd.AsTime())
				// this is an approximation as we just use the time before calling
				// checkpoint and subtract it from when start of the container
				// returns. The process will be started a few milliseconds before
				// start returns. Using the Criu reported RestoreTime also seems
				// somewhat inaccurate as that results in lower freeze durations
				// than measeured by the actual process.
				status.MigrationDuration = metav1.Duration{Duration: status.RestoredAt.Sub(status.PausedAt.Time)}
			}
			status.Condition.Phase = phase
		})
		return nil
	}); err != nil {
		return nil, err
	}

	return &nodev1.RestoreResponse{}, nil
}

func getRestoreStats(imageID string) (*stats.RestoreStatsEntry, error) {
	imgDir, err := os.Open(nodev1.SnapshotPath(imageID))
	if err != nil {
		return nil, err
	}

	return stats.CriuGetRestoreStats(imgDir)

}

func (ns *nodeService) findMatchingMigration(ctx context.Context, pod *corev1.Pod) (*v1.Migration, error) {
	notFoundErr := fmt.Errorf("no matching migration found")
	migration := &v1.Migration{}
	migrationList := &v1.MigrationList{}
	if err := retry.OnError(findMigrationBackoff,
		func(err error) bool { return errors.Is(err, notFoundErr) },
		func() error {
			if err := ns.kube.List(ctx, migrationList, client.InNamespace(pod.Namespace)); err != nil {
				return err
			}
			for _, mig := range migrationList.Items {
				// if the target pod is the same as ours, it's our
				// migration but it has already been claimed.
				if mig.Spec.TargetPod == pod.Name && mig.Spec.TargetNode != "" {
					migration = &mig
					return nil
				}

				if matchingMigration(pod, mig) {
					// in order to claim the migration, we need to set the target node and
					// successfully update it.
					mig.Spec.TargetNode = ns.nodeName
					mig.Spec.TargetPod = pod.Name
					if err := ns.kube.Update(ctx, &mig); err != nil {
						// if we get a conflict it means this migration has
						// already been claimed by another node. We continue to
						// try to find another one.
						if kerrors.IsConflict(err) {
							continue
						}
						return fmt.Errorf("claiming migration: %w", err)
					}
					migration = &mig
					return nil
				}
			}
			return notFoundErr
		}); err != nil {
		return nil, fmt.Errorf("timeout waiting for restore: %w", err)
	}
	return migration, nil
}

func matchingMigration(pod *corev1.Pod, migration v1.Migration) bool {
	return migration.Spec.TargetNode == "" &&
		migration.Spec.PodTemplateHash == pod.Labels[appsv1.DefaultDeploymentUniqueLabelKey]
}

func (ns *nodeService) pullImage(ctx context.Context, nodeClient nodev1.NodeClient, id string) error {
	cl, err := nodeClient.PullImage(ctx, &nodev1.PullImageRequest{ImageId: id})
	if err != nil {
		return err
	}
	img, err := cl.Recv()
	if err != nil {
		return err
	}
	ns.log.Info("received image data, starting extract", "len", len(img.ImageData))

	format := archives.CompressedArchive{
		Compression: archives.Zstd{},
		Extraction:  archives.Tar{},
	}

	baseDir := nodev1.ImagePath(id)
	if err := os.MkdirAll(baseDir, os.ModePerm); err != nil {
		return err
	}
	return format.Extract(ctx, bytes.NewReader(img.ImageData), func(ctx context.Context, f archives.FileInfo) error {
		name := filepath.Join(baseDir, filepath.Clean(f.NameInArchive))
		if f.IsDir() {
			return os.MkdirAll(name, f.Mode())
		}

		rc, err := f.Open()
		if err != nil {
			return err
		}

		file, err := os.Create(name)
		if err != nil {
			return err
		}

		if _, err := io.Copy(file, rc); err != nil {
			return err
		}

		return nil
	})
}

func (ns *nodeService) PrepareEvac(ctx context.Context, req *nodev1.EvacRequest) (*nodev1.EvacResponse, error) {
	ns.log.Info("got evac preparation request",
		"pod_name", req.PodInfo.Name,
		"pod_namespace", req.PodInfo.Namespace,
		"container_name", req.PodInfo.ContainerName)

	migration := &v1.Migration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.PodInfo.Name,
			Namespace: req.PodInfo.Namespace,
		},
	}
	// wait for the migration to be claimed
	pCtx, cancel := context.WithTimeout(ctx, time.Second*10)
	defer cancel()
	if err := wait.PollUntilContextCancel(pCtx, time.Millisecond*10, true, func(ctx context.Context) (done bool, perr error) {
		if err := ns.kube.Get(ctx, client.ObjectKeyFromObject(migration), migration); err != nil {
			if kerrors.IsNotFound(err) {
				return
			}
			perr = err
			return
		}
		if migration.Spec.TargetNode != "" {
			ns.log.Info("migration is claimed")
			done = true
		}
		return
	}); err != nil {
		ns.log.Error("prepare evac request failed",
			"name", migration.Name, "namespace", migration.Namespace, "error", err)
		if err := ns.updateMigrationStatus(ctx, client.ObjectKeyFromObject(migration), func(m *v1.Migration) error {
			setOrUpdateContainerStatus(migration, req.PodInfo.ContainerName, func(status *v1.MigrationContainerStatus) {
				status.Condition.Phase = v1.MigrationPhaseUnclaimed
			})
			return nil
		}); err != nil {
			ns.log.Error("failed to update migration status",
				"name", migration.Name, "namespace", migration.Namespace, "error", err)
		}
		return nil, err
	}

	ns.log.Info("evac prepare done")

	return &nodev1.EvacResponse{}, nil
}

func (ns *nodeService) Evac(ctx context.Context, req *nodev1.EvacRequest) (*nodev1.EvacResponse, error) {
	ns.log.Info("got evac request",
		"pod_name", req.PodInfo.Name,
		"pod_namespace", req.PodInfo.Namespace,
		"container_name", req.PodInfo.ContainerName,
		"image_id", req.MigrationInfo.ImageId)

	pod := &corev1.Pod{}
	nsName := types.NamespacedName{
		Name:      req.PodInfo.Name,
		Namespace: req.PodInfo.Namespace,
	}
	if err := ns.kube.Get(ctx, nsName, pod); err != nil {
		return nil, err
	}

	var pageServer *v1.MigrationServer
	if req.MigrationInfo.LiveMigration {
		tlsConfig := ns.tlsConfig
		if !ns.pageServerTLS {
			tlsConfig = nil
		}
		psp := newPageServerProxy("0.0.0.0:0", nodev1.LazyPagesSocket(req.MigrationInfo.ImageId), tlsConfig, nil, ns.log)
		pspContext, cancel := context.WithTimeout(context.Background(), pagesTransferTimeout)
		if err := psp.Start(pspContext); err != nil {
			ns.log.Error("page server src proxy", "error", err)
		}
		pageServer = &v1.MigrationServer{
			Host: ns.host, Port: psp.Port(),
		}
		ns.log.Info("started page server src proxy", "port", psp.Port(), "tls", ns.pageServerTLS)
		go func() {
			if err := psp.Wait(); err != nil {
				ns.log.Error("page server src proxy", "error", err)
			}
			cancel()
			ns.log.Info("page server src proxy closed")
		}()
	}

	if err := ns.updateMigration(ctx, nsName, func(migration *v1.Migration) error {
		if found := updateContainerSpec(migration, req.PodInfo.ContainerName, func(mc *v1.MigrationContainer) {
			mc.ImageServer = &v1.MigrationServer{
				Host: ns.host,
				Port: ns.port,
			}
			mc.PageServer = pageServer
			mc.Ports = req.PodInfo.Ports
			ns.log.Debug("found our container, setting migration servers")
		}); !found {
			return fmt.Errorf("migration does not have image for requested container %s", req.PodInfo.ContainerName)
		}
		migration.Spec.LiveMigration = req.MigrationInfo.LiveMigration
		return nil
	}); err != nil {
		return nil, err
	}
	if pageServer != nil {
		ns.log.Info("set page server in evac", "host", ns.host, "port", pageServer.Port)
	}

	if err := ns.updateMigrationStatus(ctx, nsName, func(migration *v1.Migration) error {
		setOrUpdateContainerStatus(migration, req.PodInfo.ContainerName, func(status *v1.MigrationContainerStatus) {
			status.PausedAt = metav1.NewMicroTime(req.MigrationInfo.PausedAt.AsTime())
			status.Condition.Phase = v1.MigrationPhaseRunning
		})
		return nil
	}); err != nil {
		return nil, err
	}

	return &nodev1.EvacResponse{}, nil
}

func (ns *nodeService) NewCriuLazyPages(ctx context.Context, r *nodev1.CriuLazyPagesRequest) (*emptypb.Empty, error) {
	// the criu lazy-pages daemon would be able to connect directly to the page
	// server via TLS but in testing this has been proven to work very
	// unreliably. The root cause is unclear (gnutls?) but on arm64, TLS1.2
	// worked fine while TLS1.3 simply broke sometimes. On amd64 both are just
	// failing. The TLS handshake is successful but then CRIU fails with errors
	// like: CRIU fails with: Error (criu/page-xfer.c:1635): page-xfer: BUG
	//
	// so instead, we now also spawn a proxy on the dst side, which takes care
	// of TLS in Go and provides a plain local TCP socket for the lazy-pages
	// daemon to connect to.
	psp := newPageServerProxy(
		"127.0.0.1:0",
		fmt.Sprintf("%s:%d", r.Address, r.Port),
		nil,
		ns.tlsConfig,
		ns.log,
	)
	pspContext, cancel := context.WithTimeout(context.Background(), pagesTransferTimeout)
	if err := psp.Start(pspContext); err != nil {
		cancel()
		ns.log.Error("starting page server dst proxy", "error", err)
		return nil, fmt.Errorf("starting page server dst proxy: %w", err)
	}
	args := []string{
		"-o", "/dev/stdout", "-v", "lazy-pages", "--images-dir",
		r.CheckpointPath, "--work-dir", r.CheckpointPath, "--page-server",
		"--address", "127.0.0.1", "--port", strconv.Itoa(psp.Port()),
	}
	cmd := exec.Command(filepath.Join(OptPath, "bin/criu"), args...)
	ns.log.Info("starting lazy pages daemon", "cmd", cmd.Args)
	cmd.Env = []string{"LD_LIBRARY_PATH=" + filepath.Join(OptPath, "lib")}
	execLogger := newExecLogger("criu-page-server", ns.log, slog.LevelDebug)
	cmd.Stderr = execLogger
	cmd.Stdout = execLogger
	if err := cmd.Start(); err != nil {
		cancel()
		return &emptypb.Empty{}, fmt.Errorf("error running lazy-pages daemon: %s", err)
	}
	go func() {
		cmd.Wait()
		cancel()
		if err := psp.Wait(); err != nil {
			ns.log.Error("page server dst proxy", "error", err)
		}
		ns.log.Info("page server dst proxy closed")
	}()
	return &emptypb.Empty{}, nil
}

var imageIDRegexp = regexp.MustCompile("^[A-Fa-f0-9]{64}$")

// PullImage allows the caller to pull a compressed image from the server
// TODO: transmit image in chunks, not all in one message
func (ns *nodeService) PullImage(ctx context.Context, req *nodev1.PullImageRequest, imageStream nodev1.Node_PullImageServer) error {
	ns.log.Info("got pull image request", "image_id", req.ImageId)
	if !imageIDRegexp.MatchString(req.ImageId) {
		ns.log.Error("requested image_id is invalid", "image_id", req.ImageId)
		return fmt.Errorf("invalid image_id requested: %s", req.ImageId)
	}

	arch, err := archives.FilesFromDisk(ctx, nil, map[string]string{nodev1.SnapshotPath(req.ImageId): ""})
	if err != nil {
		return fmt.Errorf("unable to archive checkpoint: %w", err)
	}

	format := archives.CompressedArchive{
		Compression: archives.Zstd{},
		Archival:    archives.Tar{},
	}

	// TODO: send in chunks
	imageData := bytes.Buffer{}
	if err := format.Archive(ctx, &imageData, arch); err != nil {
		return err
	}
	ns.log.Debug("sending archived image data", "path", nodev1.SnapshotPath(req.ImageId), "size", imageData.Len())

	return imageStream.Send(&nodev1.Image{
		ImageData: imageData.Bytes(),
	})
}

func (ns *nodeService) local(host string) bool {
	return ns.host == host
}

type updateFunc func(*v1.Migration) error

func (ns *nodeService) updateMigration(ctx context.Context, nsName types.NamespacedName, update updateFunc) error {
	migration := &v1.Migration{}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := ns.kube.Get(ctx, nsName, migration); err != nil {
			return err
		}
		if err := update(migration); err != nil {
			return err
		}
		return ns.kube.Update(ctx, migration)
	})
}

func (ns *nodeService) updateMigrationStatus(ctx context.Context, nsName types.NamespacedName, update updateFunc) error {
	migration := &v1.Migration{}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := ns.kube.Get(ctx, nsName, migration); err != nil {
			return err
		}
		if err := update(migration); err != nil {
			return err
		}
		return ns.kube.Status().Update(ctx, migration)
	})
}

func updateContainerSpec(migration *v1.Migration, containerName string, updateFunc func(*v1.MigrationContainer)) (found bool) {
	for i, container := range migration.Spec.Containers {
		if container.Name == containerName {
			updateFunc(&migration.Spec.Containers[i])
			return true
		}
	}
	return false
}

func setOrUpdateContainerStatus(migration *v1.Migration, containerName string, updateFunc func(*v1.MigrationContainerStatus)) {
	for i, container := range migration.Status.Containers {
		if container.Name == containerName {
			updateFunc(&migration.Status.Containers[i])
			return
		}
	}
	status := v1.MigrationContainerStatus{Name: containerName}
	updateFunc(&status)
	migration.Status.Containers = append(migration.Status.Containers, status)
}
