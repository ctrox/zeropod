package e2e

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ctrox/zeropod/zeropod"
	"github.com/phayes/freeport"
	"github.com/pkg/errors"
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
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
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
	installerDockerfile = "../installer/Dockerfile"
	installerYaml       = "../config/installer.yaml"
)

var images = []string{installerImage}

func setup(t testing.TB) (*rest.Config, client.Client, int) {
	t.Log("building installer and shim")
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

	t.Log("deploying installer")
	if err := deployInstaller(t, context.Background(), client); err != nil {
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
				ExtraPortMappings: []v1alpha4.PortMapping{{
					ContainerPort: 30000,
					HostPort:      int32(port),
					ListenAddress: "0.0.0.0",
					Protocol:      v1alpha4.PortMappingProtocolTCP,
				}},
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
	return nil
}

func deployInstaller(t testing.TB, ctx context.Context, c client.Client) error {
	yamlFile, err := ioutil.ReadFile(installerYaml)
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
	}, time.Second*10, time.Millisecond*500); !ok {
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
		time.Sleep(time.Hour)
	}

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

func createPodAndPortForward(t testing.TB, ctx context.Context, cfg *rest.Config, client client.Client, pod *corev1.Pod) int {
	createPodAndWait(t, ctx, client, pod)

	pf := PortForward{
		Config:          cfg,
		Clientset:       kubernetes.NewForConfigOrDie(cfg),
		Name:            pod.Name,
		Namespace:       pod.Namespace,
		DestinationPort: 80,
	}

	assert.NoError(t, pf.Start())
	t.Cleanup(func() {
		pf.Stop()
	})

	return pf.ListenPort
}
