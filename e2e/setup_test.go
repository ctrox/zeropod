package e2e

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ctrox/zeropod/socket"
	"github.com/ctrox/zeropod/zeropod"
	"github.com/phayes/freeport"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/serializer/yaml"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	"sigs.k8s.io/kind/pkg/cluster"
	"sigs.k8s.io/kind/pkg/cluster/nodes"
	"sigs.k8s.io/kind/pkg/cluster/nodeutils"
	"sigs.k8s.io/kind/pkg/fs"
)

const (
	installerImage      = "docker.io/ctrox/zeropod-installer:dev"
	managerImage        = "docker.io/ctrox/zeropod-manager:dev"
	installerDockerfile = "../cmd/installer/Dockerfile"
	managerDockerfile   = "../cmd/manager/Dockerfile"
	nodeYaml            = "../config/node.yaml"
)

var images = []string{installerImage, managerImage}

func setup(t testing.TB) (*rest.Config, client.Client, int) {
	t.Log("building node and shim")
	if err := build(); err != nil {
		t.Fatal(err)
	}

	t.Log("starting kind cluster")

	port, err := freeport.GetFreePort()
	if err != nil {
		log.Fatal(err)
	}

	cfg, err := startKind(t, "zeropod-e2e", port)
	require.NoError(t, err)

	client, err := client.New(cfg, client.Options{})
	require.NoError(t, err)

	t.Log("deploying zeropod-node")
	if err := deployNode(t, context.Background(), client); err != nil {
		t.Fatal(err)
	}

	return cfg, client, port
}

func startKind(t testing.TB, name string, port int) (c *rest.Config, err error) {
	f, err := os.CreateTemp("", name+"-*")
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() {
		os.RemoveAll(f.Name())
	})

	// For now only docker is supported as podman does not seem to work
	// well with criu (rootless).
	provider := cluster.NewProvider(cluster.ProviderWithDocker())

	t.Cleanup(func() {
		if err := stopKind(name, f.Name()); err != nil {
			t.Fatal(err)
		}
	})

	if err := provider.Create(name,
		cluster.CreateWithV1Alpha4Config(&v1alpha4.Cluster{
			Name: name,
			Nodes: []v1alpha4.Node{{
				// setup port map for our node port
				ExtraPortMappings: []v1alpha4.PortMapping{{
					ContainerPort: 30000,
					HostPort:      int32(port),
					ListenAddress: "0.0.0.0",
					Protocol:      v1alpha4.PortMappingProtocolTCP,
				}},
				// setup mounts for ebpf tcp tracking (it needs to map host pids to container pids)
				ExtraMounts: []v1alpha4.Mount{
					{
						HostPath:      "/proc",
						ContainerPath: "/host/proc",
					},
					{
						HostPath:      socket.BPFFSPath,
						ContainerPath: socket.BPFFSPath,
					},
				},
			}},
		}),
		cluster.CreateWithNodeImage("kindest/node:v1.26.3"),
		cluster.CreateWithRetain(false),
		cluster.CreateWithKubeconfigPath(f.Name()),
		cluster.CreateWithWaitForReady(time.Minute*2),
	); err != nil {
		return nil, fmt.Errorf("unable to create kind cluster: %w", err)
	}

	config, err := clientcmd.BuildConfigFromFlags("", f.Name())
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
func save(images []string, dest string) error {
	commandArgs := append([]string{"save", "-o", dest}, images...)
	return exec.Command("docker", commandArgs...).Run()
}

func build() error {
	commandArgs := []string{"build", "-t", installerImage, "-f", installerDockerfile, "../"}
	out, err := exec.Command("docker", commandArgs...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("error during docker build: %s: %s", err, out)
	}

	out, err = exec.Command("docker", "build", "-t", managerImage, "-f", managerDockerfile, "../").CombinedOutput()
	if err != nil {
		return fmt.Errorf("error during docker build: %s: %s", err, out)
	}

	return nil
}

func deployNode(t testing.TB, ctx context.Context, c client.Client) error {
	yamlFile, err := ioutil.ReadFile(nodeYaml)
	if err != nil {
		log.Fatal(err)
	}

	multidocReader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(yamlFile)))
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
				Name: runtimeClassName,
			},
		}
		return c.Get(ctx, objectName(runtimeClass), runtimeClass) == nil
	}, time.Second*30, time.Millisecond*500); !ok {
		return fmt.Errorf("runtimeClass not found")
	}

	return nil
}

func testPod(preDump bool, scaleDownDuration time.Duration) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "zeropod-e2e-",
			Namespace:    "default",
			Annotations: map[string]string{
				zeropod.PortAnnotationKey:              "80",
				zeropod.ContainerNameAnnotationKey:     "nginx",
				zeropod.ScaleDownDurationAnnotationKey: scaleDownDuration.String(),
				zeropod.PreDumpAnnotationKey:           strconv.FormatBool(preDump),
			},
			Labels: map[string]string{"app": "zeropod-e2e"},
		},
		Spec: corev1.PodSpec{
			RuntimeClassName: pointer.StringPtr(runtimeClassName),
			Containers: []corev1.Container{
				{
					StartupProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path: "/",
								Port: intstr.FromInt(80),
							},
						},
						TimeoutSeconds: 2,
					},
					Name:  "nginx",
					Image: "nginx",
					Ports: []corev1.ContainerPort{{ContainerPort: 80}},
				},
			},
		},
	}
}

func testService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "zeropod-e2e-",
			Namespace:    "default",
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "zeropod-e2e"},
			Type:     corev1.ServiceTypeNodePort,
			Ports: []corev1.ServicePort{{
				Port:       80,
				TargetPort: intstr.FromInt(80),
				NodePort:   30000,
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
		}, time.Minute, time.Second, "waiting for pod to be deleted")
	}
}

func createServiceAndWait(t testing.TB, ctx context.Context, client client.Client, svc *corev1.Service, replicas int) (cleanup func()) {
	err := client.Create(ctx, svc)
	require.NoError(t, err)
	t.Logf("created service %s", svc.Name)

	if !assert.Eventually(t, func() bool {
		endpoints := &corev1.Endpoints{}
		if err := client.Get(ctx, objectName(svc), endpoints); err != nil {
			return false
		}

		if len(endpoints.Subsets) == 0 {
			return false
		}

		return len(endpoints.Subsets[0].Addresses) == replicas
	}, time.Second*30, time.Second, "waiting for service endpoints to be ready") {
		t.Log("service did not get ready")
	}

	// we give it some more time before returning just to make sure it's
	// really ready to receive requests.
	time.Sleep(time.Millisecond * 100)

	return func() {
		client.Delete(ctx, svc)
		assert.NoError(t, err)
		require.Eventually(t, func() bool {
			if err := client.Get(ctx, objectName(svc), svc); err != nil {
				return true
			}

			return false
		}, time.Minute, time.Second, "waiting for service to be deleted")
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

	if err := exec.Stream(remotecommand.StreamOptions{
		Stdout: buf,
		Stderr: errBuf,
	}); err != nil {
		return "", "", fmt.Errorf("failed exec command %q on %v/%v: %w", command, pod.Namespace, pod.Name, err)
	}

	return buf.String(), errBuf.String(), nil
}

func restoreCount(t testing.TB, client client.Client, cfg *rest.Config, pod *corev1.Pod) int {
	mfs := getNodeMetrics(t, client, cfg)

	running := prometheus.BuildFQName(zeropod.MetricsNamespace, "", zeropod.MetricRestoreDuration)
	val, ok := mfs[running]
	if !ok {
		t.Fatalf("could not find expected metric: %s", running)
	}

	metric, ok := findMetricByLabelMatch(val.Metric, map[string]string{
		zeropod.LabelPodName:      pod.Name,
		zeropod.LabelPodNamespace: pod.Namespace,
	})
	if !ok {
		t.Fatalf("could not find running metric that matches pod: %s/%s", pod.Name, pod.Namespace)
	}

	if metric.Histogram == nil {
		t.Fatalf("found metric that is not a histogram")
	}

	if metric.Histogram.SampleCount == nil {
		t.Fatalf("histogram sample count is nil")
	}

	return int(*metric.Histogram.SampleCount)

}

func waitForZeropodScaledown(t testing.TB, client client.Client, cfg *rest.Config, pod *corev1.Pod) {
	assert.Eventually(
		t, func() bool { return !zeropodRunning(t, client, cfg, pod) },
		time.Second*30, time.Millisecond*100, "zeropod is not scaled down",
	)
}

func zeropodRunning(t testing.TB, client client.Client, cfg *rest.Config, pod *corev1.Pod) bool {
	mfs := getNodeMetrics(t, client, cfg)

	running := prometheus.BuildFQName(zeropod.MetricsNamespace, "", zeropod.MetricRunning)
	val, ok := mfs[running]
	if !ok {
		t.Fatalf("could not find expected metric: %s", running)
	}

	metric, ok := findMetricByLabelMatch(val.Metric, map[string]string{
		zeropod.LabelPodName:      pod.Name,
		zeropod.LabelPodNamespace: pod.Namespace,
	})
	if !ok {
		t.Fatalf("could not find running metric that matches pod: %s/%s", pod.Name, pod.Namespace)
	}

	if metric.Gauge == nil {
		t.Fatalf("found metric that is not a gauge")
	}

	if metric.Gauge.Value == nil {
		t.Fatalf("metric value is nil")
	}

	return *metric.Gauge.Value == 1
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

func getNodeMetrics(t testing.TB, c client.Client, cfg *rest.Config) map[string]*dto.MetricFamily {
	cs, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)

	ctx := context.Background()
	nodePods := &corev1.PodList{}
	require.NoError(t, c.List(ctx, nodePods, client.MatchingLabels{"app.kubernetes.io/name": "zeropod-node"}))
	require.Equal(t, 1, len(nodePods.Items))

	pf := PortForward{
		Config: cfg, Clientset: cs,
		Name:            nodePods.Items[0].Name,
		Namespace:       nodePods.Items[0].Namespace,
		DestinationPort: 8080,
	}
	require.NoError(t, pf.Start())
	defer pf.Stop()

	resp, err := http.Get(fmt.Sprintf("http://localhost:%v/metrics", pf.ListenPort))
	require.NoError(t, err)

	var parser expfmt.TextParser
	mfs, err := parser.TextToMetricFamilies(resp.Body)
	require.NoError(t, err)

	return mfs
}
