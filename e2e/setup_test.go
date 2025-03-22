package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ctrox/zeropod/activator"
	v1 "github.com/ctrox/zeropod/api/runtime/v1"
	shimv1 "github.com/ctrox/zeropod/api/shim/v1"
	"github.com/ctrox/zeropod/manager"
	"github.com/ctrox/zeropod/zeropod"
	"github.com/go-logr/logr"
	"github.com/phayes/freeport"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/yaml"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	"sigs.k8s.io/kind/pkg/cluster"
	"sigs.k8s.io/kind/pkg/cluster/nodes"
	"sigs.k8s.io/kind/pkg/cluster/nodeutils"
	"sigs.k8s.io/kind/pkg/fs"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

const (
	installerImage      = "ghcr.io/ctrox/zeropod-installer:dev"
	managerImage        = "ghcr.io/ctrox/zeropod-manager:dev"
	installerDockerfile = "../cmd/installer/Dockerfile"
	managerDockerfile   = "../cmd/manager/Dockerfile"
	kustomizeDir        = "../config/kind"
	nodePort            = 30000
	defaultTargetPort   = 80
)

type image struct {
	tag        string
	dockerfile string
}

type e2eConfig struct {
	cfg            *rest.Config
	client         client.Client
	port           int
	clusterName    string
	kubeconfigName string
}

func (e2e *e2eConfig) cleanup() error {
	defer os.RemoveAll(e2e.kubeconfigName)
	if err := stopKind(e2e.clusterName, e2e.kubeconfigName); err != nil {
		return err
	}
	return os.RemoveAll(e2e.kubeconfigName)
}

var images = []image{
	{
		tag:        installerImage,
		dockerfile: installerDockerfile,
	},
	{
		tag:        managerImage,
		dockerfile: managerDockerfile,
	},
}

var (
	once sync.Once
	e2e  *e2eConfig
	wg   sync.WaitGroup

	matchZeropodNodeLabels = client.MatchingLabels{"app.kubernetes.io/name": "zeropod-node"}
)

func setupOnce(t testing.TB) *e2eConfig {
	once.Do(func() {
		wg.Add(1)
		defer wg.Done()
		e2e = setup(t)
	})
	wg.Wait()
	if e2e == nil {
		t.Fatal("e2e setup failed")
	}
	return e2e
}

func setup(t testing.TB) *e2eConfig {
	t.Log("building node and shim")
	require.NoError(t, build())

	t.Log("starting kind cluster")

	port, err := freeport.GetFreePort()
	require.NoError(t, err)

	name := "zeropod-e2e"
	f, err := os.CreateTemp("", name+"-*")
	require.NoError(t, err)
	cfg, err := startKind(t, "zeropod-e2e", f.Name(), port)
	require.NoError(t, err)

	// discard controller-runtime logs, we just use the client
	log.SetLogger(logr.Discard())
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, nodev1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, v1.AddToScheme(scheme))
	client, err := client.New(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)

	t.Log("deploying zeropod-node")
	if err := deployNode(t, context.Background(), client); err != nil {
		t.Fatal(err)
	}

	return &e2eConfig{cfg, client, port, name, f.Name()}
}

func startKind(t testing.TB, name, kubeconfig string, port int) (c *rest.Config, err error) {
	// For now only docker is supported as podman does not seem to work
	// well with criu (rootless).
	provider := cluster.NewProvider(cluster.ProviderWithDocker())

	// setup mounts for ebpf tcp tracking (it needs to map host pids to
	// container pids)
	extraMounts := []v1alpha4.Mount{
		{
			HostPath:      "/proc",
			ContainerPath: "/host/proc",
		},
		{
			HostPath:      activator.BPFFSPath,
			ContainerPath: activator.BPFFSPath,
		},
	}
	if err := provider.Create(name,
		cluster.CreateWithV1Alpha4Config(&v1alpha4.Cluster{
			Name: name,
			FeatureGates: map[string]bool{
				"InPlacePodVerticalScaling":                true,
				"InPlacePodVerticalScalingAllocatedStatus": true,
			},
			Nodes: []v1alpha4.Node{
				{
					Role:        v1alpha4.ControlPlaneRole,
					ExtraMounts: extraMounts,
					// setup port map for our node port
					ExtraPortMappings: []v1alpha4.PortMapping{{
						ContainerPort: nodePort,
						HostPort:      int32(port),
						ListenAddress: "0.0.0.0",
						Protocol:      v1alpha4.PortMappingProtocolTCP,
					}},
				},
				{
					Role:        v1alpha4.WorkerRole,
					Labels:      map[string]string{zeropod.NodeLabel: "true"},
					ExtraMounts: extraMounts,
				},
				{
					Role:        v1alpha4.WorkerRole,
					Labels:      map[string]string{zeropod.NodeLabel: "true"},
					ExtraMounts: extraMounts,
				},
			},
		}),
		cluster.CreateWithNodeImage("kindest/node:v1.32.0@sha256:c48c62eac5da28cdadcf560d1d8616cfa6783b58f0d94cf63ad1bf49600cb027"),
		cluster.CreateWithRetain(false),
		cluster.CreateWithKubeconfigPath(kubeconfig),
		cluster.CreateWithWaitForReady(time.Minute*2),
	); err != nil {
		return nil, fmt.Errorf("unable to create kind cluster: %w", err)
	}

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}

	imagesTarPath, err := getImages(t)
	if err != nil {
		return nil, err
	}

	nodes, err := provider.ListNodes(name)
	if err != nil {
		return nil, err
	}

	for _, node := range nodes {
		if err := loadImages(node, imagesTarPath); err != nil {
			return nil, fmt.Errorf("unable to load installer image to node: %w", err)
		}
	}

	return config, nil
}

func stopKind(name, kubeconfig string) error {
	if err := cluster.NewProvider(cluster.ProviderWithDocker()).Delete(name, kubeconfig); err != nil {
		return fmt.Errorf("unable to delete kind cluster: %w", err)
	}

	return nil
}

func loadImages(node nodes.Node, imageFile string) error {
	f, err := os.Open(imageFile)
	if err != nil {
		return errors.Wrap(err, "failed to open image")
	}
	defer f.Close()
	return nodeutils.LoadImageArchive(node, f)
}

func getImages(t testing.TB) (string, error) {
	dir, err := fs.TempDir("", "images-tar")
	if err != nil {
		return "", fmt.Errorf("failed to create tempdir: %w", err)
	}
	t.Cleanup(func() {
		os.RemoveAll(dir)
	})

	imagesTarPath := filepath.Join(dir, "images.tar")

	if err := save(images, imagesTarPath); err != nil {
		return "", err
	}

	return imagesTarPath, nil
}

// save saves images to dest, as in `docker save`
func save(images []image, dest string) error {
	imageTags := []string{}
	for _, img := range images {
		imageTags = append(imageTags, img.tag)
	}
	commandArgs := append([]string{"save", "-o", dest}, imageTags...)
	return exec.Command("docker", commandArgs...).Run()
}

func build() error {
	for _, image := range images {
		commandArgs := []string{"build", "--load", "-t", image.tag, "-f", image.dockerfile, "../"}
		out, err := exec.Command("docker", commandArgs...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("error during docker build: %s: %s", err, out)
		}
	}

	return nil
}

func deployNode(t testing.TB, ctx context.Context, c client.Client) error {
	yml, err := loadManifests()
	if err != nil {
		return err
	}

	multidocReader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(yml)))
	for {
		buf, err := multidocReader.Read()
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}

		dec := yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme)

		obj := &unstructured.Unstructured{}
		_, gvk, err := dec.Decode([]byte(buf), nil, obj)
		if err != nil {
			return err
		}

		obj.SetGroupVersionKind(*gvk)

		if err := c.Create(ctx, obj); err != nil {
			return err
		}
	}

	// wait until runtimeclass is available, that means the install was successful
	if ok := assert.Eventually(t, func() bool {
		runtimeClass := &nodev1.RuntimeClass{
			ObjectMeta: metav1.ObjectMeta{
				Name: v1.RuntimeClassName,
			},
		}
		return c.Get(ctx, objectName(runtimeClass), runtimeClass) == nil
	}, time.Second*30, time.Millisecond*500); !ok {
		return fmt.Errorf("runtimeClass not found")
	}

	// wait for node pod to be running
	nodePods := &corev1.PodList{}
	require.NoError(t, c.List(ctx, nodePods, matchZeropodNodeLabels))
	require.Equal(t, 2, len(nodePods.Items))

	for _, pod := range nodePods.Items {
		require.Eventually(t, func() bool {
			if err := c.Get(ctx, objectName(&pod), &pod); err != nil {
				return false
			}

			return pod.Status.Phase == corev1.PodRunning
		}, time.Minute, time.Second, "waiting for node pod to be running")
	}

	return nil
}

func loadManifests() ([]byte, error) {
	fSys := filesys.MakeFsOnDisk()
	k := krusty.MakeKustomizer(krusty.MakeDefaultOptions())
	m, err := k.Run(fSys, kustomizeDir)
	if err != nil {
		return nil, err
	}

	yml, err := m.AsYaml()
	if err != nil {
		return nil, err
	}

	return yml, nil
}

type pod struct {
	meta *metav1.ObjectMeta
	spec *corev1.PodSpec
}

type podOption func(p *pod)

func annotations(annotations map[string]string) podOption {
	return func(p *pod) {
		if p.meta.Annotations == nil {
			p.meta.SetAnnotations(annotations)
		}
		for k, v := range annotations {
			p.meta.Annotations[k] = v
		}
	}
}

func preDump(preDump bool) podOption {
	return annotations(map[string]string{
		zeropod.PreDumpAnnotationKey: strconv.FormatBool(preDump),
	})
}

func scaleDownAfter(dur time.Duration) podOption {
	return annotations(map[string]string{
		zeropod.ScaleDownDurationAnnotationKey: dur.String(),
	})
}

func containerNamesAnnotation(names ...string) podOption {
	return annotations(map[string]string{
		zeropod.ContainerNamesAnnotationKey: strings.Join(names, ","),
	})
}

func portsAnnotation(portsMap string) podOption {
	return annotations(map[string]string{
		zeropod.PortsAnnotationKey: portsMap,
	})
}

func migrateAnnotation(container string) podOption {
	return annotations(map[string]string{
		zeropod.MigrateAnnotationKey: container,
	})
}

func resources(res corev1.ResourceRequirements) podOption {
	return func(p *pod) {
		for i := range p.spec.Containers {
			p.spec.Containers[i].Resources = res
		}
	}
}

const agnHostImage = "registry.k8s.io/e2e-test-images/agnhost:2.39"

func agnContainer(name string, port int) podOption {
	return addContainer(
		name, agnHostImage,
		[]string{"netexec", fmt.Sprintf("--http-port=%d", port), "--udp-port=-1"},
		port,
	)
}

func addContainer(name, image string, args []string, ports ...int) podOption {
	return func(p *pod) {
		container := corev1.Container{
			Name:  name,
			Image: image,
			Args:  args,
		}
		for _, port := range ports {
			container.Ports = append(container.Ports, corev1.ContainerPort{ContainerPort: int32(port)})
		}

		p.spec.Containers = append(p.spec.Containers, container)
	}
}

func testPod(opts ...podOption) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "zeropod-e2e-",
			Namespace:    "default",
			Labels:       map[string]string{"app": "zeropod-e2e"},
		},
		Spec: corev1.PodSpec{
			RuntimeClassName: ptr.To(v1.RuntimeClassName),
		},
	}

	for _, opt := range opts {
		opt(&pod{meta: &p.ObjectMeta, spec: &p.Spec})
	}

	if len(p.Spec.Containers) == 0 {
		addContainer("nginx", "nginx", nil, 80)(&pod{meta: &p.ObjectMeta, spec: &p.Spec})
	}

	return p
}

func testService(port int) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "zeropod-e2e-",
			Namespace:    "default",
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "zeropod-e2e"},
			Type:     corev1.ServiceTypeNodePort,
			Ports: []corev1.ServicePort{{
				Port:       int32(port),
				TargetPort: intstr.FromInt(port),
				NodePort:   nodePort,
			}},
		},
	}
}

func objectName(obj client.Object) types.NamespacedName {
	return types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}
}

func createPodAndWait(t testing.TB, ctx context.Context, client client.Client, pod *corev1.Pod) (cleanup func()) {
	err := client.Create(ctx, pod)
	assert.NoError(t, err)
	t.Logf("created pod %s", pod.Name)

	require.Eventually(t, func() bool {
		if err := client.Get(ctx, objectName(pod), pod); err != nil {
			return false
		}

		return pod.Status.Phase == corev1.PodRunning
	}, time.Minute, time.Second, "waiting for pod to be running")

	return func() {
		client.Delete(ctx, pod)
		assert.NoError(t, err)
		require.Eventually(t, func() bool {
			if err := client.Get(ctx, objectName(pod), pod); err != nil {
				return true
			}

			return false
		}, time.Minute*2, time.Second, "waiting for pod to be deleted")
	}
}

func freezerDeployment(name, namespace string, memoryGiB int, opts ...podOption) *appsv1.Deployment {
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "zeropod-e2e"},
			},
			Replicas: ptr.To(int32(1)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "zeropod-e2e"},
				},
				Spec: corev1.PodSpec{
					RuntimeClassName: ptr.To(v1.RuntimeClassName),
					Containers: []corev1.Container{{
						Name:            "freezer",
						Image:           "ghcr.io/ctrox/zeropod-freezer",
						ImagePullPolicy: corev1.PullIfNotPresent,
						Command:         []string{"/freezer"},
						Args:            []string{"-memory", strconv.Itoa(memoryGiB)},
						Ports: []corev1.ContainerPort{{
							Name:          "freezer",
							ContainerPort: 8080,
						}},
					}},
				},
			},
		},
	}
	for _, opt := range opts {
		opt(&pod{meta: &deploy.Spec.Template.ObjectMeta, spec: &deploy.Spec.Template.Spec})
	}
	return deploy
}

func createDeployAndWait(t testing.TB, ctx context.Context, c client.Client, deploy *appsv1.Deployment) (cleanup func()) {
	err := c.Create(ctx, deploy)
	assert.NoError(t, err)
	t.Logf("created deployment %s", deploy.Name)

	require.Eventually(t, func() bool {
		podList := &corev1.PodList{}
		if err := c.List(ctx, podList, client.MatchingLabels(deploy.Spec.Selector.MatchLabels)); err != nil {
			return false
		}
		running, ready := false, false
		for _, pod := range podList.Items {
			running = pod.Status.Phase == corev1.PodRunning
			if len(pod.Status.ContainerStatuses) < 1 {
				return false
			}
			ready = pod.Status.ContainerStatuses[0].Ready
		}
		return running && ready
	}, time.Minute, time.Second, "waiting for pods of deployment to be running")

	return func() {
		c.Delete(ctx, deploy)
		assert.NoError(t, err)
		require.Eventually(t, func() bool {
			if err := c.Get(ctx, objectName(deploy), deploy); err != nil {
				return true
			}
			return false
		}, time.Minute*2, time.Second, "waiting for deployment to be deleted")
	}
}

func podsOfDeployment(t testing.TB, ctx context.Context, c client.Client, deploy *appsv1.Deployment) []corev1.Pod {
	podList := &corev1.PodList{}
	if err := c.List(ctx, podList, client.MatchingLabels(deploy.Spec.Selector.MatchLabels)); err != nil {
		t.Error(err)
	}

	return podList.Items
}

func createServiceAndWait(t testing.TB, ctx context.Context, client client.Client, svc *corev1.Service, replicas int) (cleanup func()) {
	require.NoError(t, client.Create(ctx, svc))
	t.Logf("created service %s", svc.Name)

	waitForService(t, ctx, client, svc, replicas)

	return func() {
		assert.NoError(t, client.Delete(ctx, svc))
		require.Eventually(t, func() bool {
			if err := client.Get(ctx, objectName(svc), svc); err != nil {
				return true
			}

			return false
		}, time.Minute, time.Second, "waiting for service to be deleted")
	}
}

func waitForService(t testing.TB, ctx context.Context, client client.Client, svc *corev1.Service, replicas int) {
	if !assert.Eventually(t, func() bool {
		endpoints := &corev1.Endpoints{}
		if err := client.Get(ctx, objectName(svc), endpoints); err != nil {
			return false
		}

		if len(endpoints.Subsets) == 0 {
			return false
		}

		return len(endpoints.Subsets[0].Addresses) == replicas
	}, time.Minute, time.Second, "waiting for service endpoints to be ready") {
		endpoints := &corev1.Endpoints{}
		if err := client.Get(ctx, objectName(svc), endpoints); err == nil {
			if len(endpoints.Subsets) > 0 {
				t.Logf("service did not get ready: expected %d addresses, got %d", replicas, len(endpoints.Subsets[0].Addresses))
			}
			t.Logf("endpoints: %v", endpoints)

			commandArgs := []string{"exec", "zeropod-e2e-control-plane", "journalctl", "-u", "containerd"}
			out, err := exec.Command("docker", commandArgs...).CombinedOutput()
			if err != nil {
				t.Logf("error getting containerd logs: %s", err)
			}
			t.Logf("containerd logs: %s", out)
		}
	}
	// we give it some more time before returning just to make sure it's
	// really ready to receive requests.
	time.Sleep(time.Millisecond * 500)
}

func cordonNode(t testing.TB, ctx context.Context, client client.Client, name string) (uncordon func()) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	require.NoError(t, client.Get(ctx, objectName(node), node))
	node.Spec.Unschedulable = true
	require.NoError(t, client.Update(ctx, node))
	return func() {
		uncordonNode(t, ctx, client, node)
	}
}

func uncordonNode(t testing.TB, ctx context.Context, client client.Client, node *corev1.Node) {
	require.NoError(t, client.Get(ctx, objectName(node), node))
	node.Spec.Unschedulable = false
	require.NoError(t, client.Update(ctx, node))
}

func cordonOtherNodes(t testing.TB, ctx context.Context, client client.Client, name string) (uncordon func()) {
	nodeList := &corev1.NodeList{}
	require.NoError(t, client.List(ctx, nodeList))
	uncordonFuncs := []func(){}
	for _, node := range nodeList.Items {
		if node.Name == name {
			continue
		}
		require.NoError(t, client.Get(ctx, objectName(&node), &node))
		node.Spec.Unschedulable = true
		require.NoError(t, client.Update(ctx, &node))
		uncordonFuncs = append(uncordonFuncs, func() {
			uncordonNode(t, ctx, client, &node)
		})
	}

	return func() {
		for _, f := range uncordonFuncs {
			f()
		}
	}
}

func podExec(cfg *rest.Config, pod *corev1.Pod, command string) (string, string, error) {
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return "", "", err
	}

	buf := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	request := client.CoreV1().RESTClient().
		Post().
		Namespace(pod.Namespace).
		Resource("pods").
		Name(pod.Name).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Command: []string{"/bin/sh", "-c", command},
			Stdin:   false,
			Stdout:  true,
			Stderr:  true,
			TTY:     true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(cfg, "POST", request.URL())
	if err != nil {
		return "", "", err
	}

	if err := exec.StreamWithContext(context.Background(), remotecommand.StreamOptions{
		Stdout: buf,
		Stderr: errBuf,
	}); err != nil {
		return "", "", fmt.Errorf("failed exec command %q on %v/%v: %w", command, pod.Namespace, pod.Name, err)
	}

	return buf.String(), errBuf.String(), nil
}

func restoreCount(t testing.TB, ctx context.Context, client client.Client, cfg *rest.Config, pod *corev1.Pod) (int, error) {
	val, err := getNodeMetric(t, ctx, client, cfg, zeropod.MetricRestoreDuration)
	if err != nil {
		return 0, err
	}

	metric, ok := findMetricByLabelMatch(val.Metric, map[string]string{
		zeropod.LabelPodName:      pod.Name,
		zeropod.LabelPodNamespace: pod.Namespace,
	})
	if !ok {
		return 0, fmt.Errorf("could not find restore duration metric that matches pod: %s/%s: %w",
			pod.Name, pod.Namespace, err)
	}

	if metric.Histogram == nil {
		return 0, fmt.Errorf("found metric that is not a histogram")
	}

	if metric.Histogram.SampleCount == nil {
		return 0, fmt.Errorf("histogram sample count is nil")
	}

	return int(*metric.Histogram.SampleCount), nil
}

func waitUntilScaledDown(t testing.TB, ctx context.Context, c client.Client, pod *corev1.Pod) {
	for _, container := range pod.Spec.Containers {
		require.Eventually(t, func() bool {
			ok, err := isScaledDown(ctx, c, pod, container.Name)
			t.Logf("scaled down: %v: %s", ok, pod.GetLabels()[path.Join(manager.StatusLabelKeyPrefix, container.Name)])
			return err == nil && ok
		}, time.Second*15, time.Second)
	}
}

func isScaledDown(ctx context.Context, c client.Client, pod *corev1.Pod, containerName string) (bool, error) {
	if err := c.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
		return false, err
	}
	v, ok := pod.GetLabels()[path.Join(manager.StatusLabelKeyPrefix, containerName)]
	return ok && v == shimv1.ContainerPhase_SCALED_DOWN.String(), nil
}

func alwaysRunningFor(t testing.TB, ctx context.Context, c client.Client, pod *corev1.Pod, dur time.Duration) {
	for _, container := range pod.Spec.Containers {
		require.Never(t, func() bool {
			ok, err := isRunning(ctx, c, pod, container.Name)
			t.Logf("running: %v: %s", ok, pod.GetLabels()[path.Join(manager.StatusLabelKeyPrefix, container.Name)])
			return err != nil && !ok
		}, dur, time.Second)
	}
}

func isRunning(ctx context.Context, c client.Client, pod *corev1.Pod, containerName string) (bool, error) {
	if err := c.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
		return false, err
	}
	v, ok := pod.GetLabels()[path.Join(manager.StatusLabelKeyPrefix, containerName)]
	return ok && v == shimv1.ContainerPhase_RUNNING.String(), nil
}

func findMetricByLabelMatch(metrics []*dto.Metric, labels map[string]string) (*dto.Metric, bool) {
	for _, metric := range metrics {
		if metricMatchesLabels(metric, labels) {
			return metric, true
		}
	}
	return nil, false
}

func metricMatchesLabels(metric *dto.Metric, labels map[string]string) bool {
	for k, v := range labels {
		if !metricMatchesLabel(metric, k, v) {
			return false
		}
	}
	return true
}

func metricMatchesLabel(metric *dto.Metric, key, value string) bool {
	for _, label := range metric.Label {
		if label.Name == nil || label.Value == nil {
			return false
		}
		if *label.Name == key && *label.Value == value {
			return true
		}
	}
	return false
}

func getNodeMetric(t testing.TB, ctx context.Context, c client.Client, cfg *rest.Config, metricName string) (*dto.MetricFamily, error) {
	var val *dto.MetricFamily
	metric := prometheus.BuildFQName(zeropod.MetricsNamespace, "", metricName)
	if !assert.Eventually(t, func() bool {
		mfs, err := getNodeMetrics(ctx, c, cfg)
		if err != nil {
			t.Logf("error getting node metrics: %s", err)
			return false
		}
		v, ok := mfs[metric]
		val = v
		return ok
	}, time.Second*30, time.Second) {
		if err := printNodeLogs(t, ctx, c, cfg); err != nil {
			t.Logf("error printing node logs: %s", err)
		}
		return nil, fmt.Errorf("could not find expected metric: %s", metric)
	}
	return val, nil
}

func getNodeMetrics(ctx context.Context, c client.Client, cfg *rest.Config) (map[string]*dto.MetricFamily, error) {
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	nodePods, err := getNodePods(ctx, c)
	if err != nil {
		return nil, err
	}

	mfs := make(map[string]*dto.MetricFamily)
	for _, nodePod := range nodePods {
		pf := PortForward{
			Config: cfg, Clientset: cs,
			Name:            nodePod.Name,
			Namespace:       nodePod.Namespace,
			DestinationPort: 8080,
		}
		if err := pf.Start(); err != nil {
			return nil, err
		}
		defer pf.Stop()

		resp, err := http.Get(fmt.Sprintf("http://localhost:%v/metrics", pf.ListenPort))
		if err != nil {
			return nil, err
		}

		var parser expfmt.TextParser
		m, err := parser.TextToMetricFamilies(resp.Body)
		if err != nil {
			return nil, err
		}
		for k, v := range m {
			if _, ok := mfs[k]; ok {
				mfs[k].Metric = append(mfs[k].Metric, v.Metric...)
				continue
			}
			mfs[k] = v
		}
	}

	return mfs, nil
}

func getNodePods(ctx context.Context, c client.Client) ([]corev1.Pod, error) {
	nodePods := &corev1.PodList{}
	if err := c.List(ctx, nodePods, matchZeropodNodeLabels); err != nil {
		return nil, err
	}
	if len(nodePods.Items) < 1 {
		return nil, fmt.Errorf("expected to find at least 1 node pod, got %d", len(nodePods.Items))
	}

	return nodePods.Items, nil
}

func printNodeLogs(t testing.TB, ctx context.Context, c client.Client, cfg *rest.Config) error {
	nodePods, err := getNodePods(ctx, c)
	if err != nil {
		return err
	}

	for _, nodePod := range nodePods {
		logs, err := getPodLogs(ctx, cfg, nodePod)
		if err != nil {
			return err
		}
		t.Log(logs)
	}
	return nil
}

func getPodLogs(ctx context.Context, cfg *rest.Config, pod corev1.Pod) (string, error) {
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return "", fmt.Errorf("creating client: %w", err)
	}
	req := clientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{})
	podLogs, err := req.Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("opening log stream: %w", err)
	}
	defer podLogs.Close()

	buf := new(bytes.Buffer)
	if _, err := io.Copy(buf, podLogs); err != nil {
		return "", fmt.Errorf("copying pod logs: %w", err)
	}
	return buf.String(), nil
}

type freeze struct {
	LastObservation    time.Time     `json:"lastObservation"`
	LastFreezeDuration time.Duration `json:"lastFreezeDuration"`
	Data               string        `json:"data"`
}

func freezerWrite(data string, port int) error {
	f := freeze{Data: data}
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	resp, err := http.Post(fmt.Sprintf("http://localhost:%d/set", port), "", bytes.NewBuffer(b))
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func freezerRead(port int) (*freeze, error) {
	f := &freeze{}
	resp, err := http.Get(fmt.Sprintf("http://localhost:%d/get", port))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return f, json.Unmarshal(body, f)
}

func availabilityCheck(ctx context.Context, port int) time.Duration {
	var downtime time.Duration
	for {
		lowTimeoutClient := &http.Client{Timeout: time.Millisecond * 50}
		beforeReq := time.Now()
		resp, err := lowTimeoutClient.Get(fmt.Sprintf("http://localhost:%d/get", port))
		if err != nil {
			downtime += time.Since(beforeReq)
			continue
		}
		resp.Body.Close()
		select {
		case <-ctx.Done():
			return downtime
		default:
			time.Sleep(lowTimeoutClient.Timeout)
			continue
		}
	}
}
