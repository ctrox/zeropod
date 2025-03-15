package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/services/server/config"
	"github.com/coreos/go-systemd/v22/dbus"
	v1 "github.com/ctrox/zeropod/api/runtime/v1"
	"github.com/ctrox/zeropod/manager/node"
	"github.com/ctrox/zeropod/zeropod"
	corev1 "k8s.io/api/core/v1"
	knodev1 "k8s.io/api/node/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var (
	criuImage      = flag.String("criu-image", "ghcr.io/ctrox/zeropod-criu:8d5cef546a035c4dda3a1be28ff1202c3b1b4c72", "criu image to use.")
	runtime        = flag.String("runtime", "containerd", "specifies which runtime to configure. containerd/k3s/rke2")
	hostOptPath    = flag.String("host-opt-path", "/opt/zeropod", "path where zeropod binaries are stored on the host")
	uninstall      = flag.Bool("uninstall", false, "uninstalls zeropod by cleaning up all the files the installer created")
	installTimeout = flag.Duration("timeout", time.Minute, "duration the installer waits for the installation to complete")
)

type containerRuntime string

const (
	runtimeContainerd containerRuntime = "containerd"
	runtimeRKE2       containerRuntime = "rke2"
	runtimeK3S        containerRuntime = "k3s"

	binPath            = "bin/"
	criuConfigFile     = "/etc/criu/default.conf"
	shimBinaryName     = "containerd-shim-zeropod-v2"
	runtimePath        = "/build/" + shimBinaryName
	containerdConfig   = "/etc/containerd/config.toml"
	configBackupSuffix = ".original"
	templateSuffix     = ".tmpl"
	caSecretName       = "ca-cert"
	defaultCriuBin     = "criu"
	criuIPTablesBin    = "criu-iptables"
	criuConfig         = `tcp-close
skip-in-flight
network-lock skip
`
	containerdOptKey  = "io.containerd.internal.v1.opt"
	criPluginKey      = "io.containerd.grpc.v1.cri"
	zeropodRuntimeKey = "containerd.runtimes.zeropod"
	optPlugin         = `
[plugins."io.containerd.internal.v1.opt"]
  path = "%s"
`
	runtimeConfig = `
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.zeropod]
  runtime_type = "io.containerd.runc.v2"
  runtime_path = "%s/bin/containerd-shim-zeropod-v2"
  pod_annotations = [
    "zeropod.ctrox.dev/ports-map",
    "zeropod.ctrox.dev/container-names",
    "zeropod.ctrox.dev/scaledown-duration",
    "zeropod.ctrox.dev/disable-checkpointing",
    "zeropod.ctrox.dev/pre-dump",
    "zeropod.ctrox.dev/migrate",
    "io.containerd.runc.v2.group"
  ]
  [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.zeropod.options]
    # use systemd cgroup by default
    SystemdCgroup = true
`
)

func main() {
	flag.Parse()

	client, err := inClusterClient()
	if err != nil {
		log.Fatalf("unable to create in-cluster client: %s", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *installTimeout)
	defer cancel()

	if *uninstall {
		if err := runUninstall(ctx, client); err != nil {
			log.Fatalf("error uninstalling zeropod: %s", err)
		}

		log.Println("uninstaller completed")
		os.Exit(0)
	}

	if err := installCriu(ctx); err != nil {
		log.Fatalf("error installing criu: %s", err)
	}

	log.Printf("installed criu binaries from %s", *criuImage)

	if err := installRuntime(ctx, containerRuntime(*runtime)); err != nil {
		log.Fatalf("error installing runtime: %s", err)
	}

	log.Println("installed runtime")

	if err := installRuntimeClass(ctx, client); err != nil {
		log.Fatalf("error installing zeropod runtimeClass: %s", err)
	}

	log.Println("installed runtimeClass")

	if err := loadTLSCA(ctx, client); err != nil {
		log.Fatalf("error loading TLS CA certificate: %s", err)
	}

	log.Println("installed ca cert")

	log.Println("installer completed")
}

func installCriu(ctx context.Context) error {
	client, err := containerd.New("/run/containerd/containerd.sock", containerd.WithDefaultNamespace("k8s"))
	if err != nil {
		return err
	}

	image, err := client.Pull(ctx, *criuImage)
	if err != nil {
		return err
	}

	if err := client.Install(
		ctx, image, containerd.WithInstallLibs,
		containerd.WithInstallReplace,
		containerd.WithInstallPath(node.OptPath),
	); err != nil {
		return err
	}

	// write the criu config
	if err := os.MkdirAll(path.Dir(criuConfigFile), os.ModePerm); err != nil {
		return err
	}

	if err := os.WriteFile(criuConfigFile, []byte(criuConfig), 0644); err != nil {
		return err
	}

	return nil
}

func installRuntime(ctx context.Context, runtime containerRuntime) error {
	log.Printf("installing runtime for %s", runtime)

	conn, err := dbus.NewSystemdConnectionContext(ctx)
	if err != nil {
		return fmt.Errorf("unable to connect to dbus: %w", err)
	}

	// note that if the shim binary already exists, we simply switch it out with
	// the new one but existing zeropods will have to be deleted to use the
	// updated shim.
	shimDest := filepath.Join(node.OptPath, binPath, shimBinaryName)
	if err := os.Remove(shimDest); err != nil {
		log.Printf("unable to remove shim binary, continuing with install: %s", err)
	}

	shim, err := os.ReadFile(runtimePath)
	if err != nil {
		return fmt.Errorf("unable to read shim file: %w", err)
	}

	if err := os.WriteFile(shimDest, shim, 0755); err != nil {
		return fmt.Errorf("unable to write shim file: %w", err)
	}

	restartRequired, err := configureContainerd(runtime, containerdConfig)
	if err != nil {
		return fmt.Errorf("unable to configure containerd: %w", err)
	}

	if !restartRequired {
		return nil
	}

	switch runtime {
	case runtimeContainerd:
		return restartUnit(ctx, conn, "containerd.service")
	case runtimeRKE2:
		// for rke2/k3s we try restarting both services agent/server since we
		// don't know what our node is using. We return the error only if both
		// restarts fail.
		agentErr := restartUnit(ctx, conn, "rke2-agent.service")
		serverErr := restartUnit(ctx, conn, "rke2-server.service")

		if agentErr != nil && serverErr != nil {
			return fmt.Errorf("unable to restart rke2 agent/server: %w, %w", agentErr, serverErr)
		}

		return nil
	case runtimeK3S:
		agentErr := restartUnit(ctx, conn, "k3s-agent.service")
		serverErr := restartUnit(ctx, conn, "k3s.service")

		if agentErr != nil && serverErr != nil {
			return fmt.Errorf("unable to restart k3s agent/server: %w, %w", agentErr, serverErr)
		}

		return nil
	}

	return nil
}

func restartUnit(ctx context.Context, conn *dbus.Conn, service string) error {
	ch := make(chan string)
	if _, err := conn.TryRestartUnitContext(ctx, service, "replace", ch); err != nil {
		return fmt.Errorf("unable to restart %s", service)
	}
	<-ch

	return nil
}

func configureContainerd(runtime containerRuntime, containerdConfig string) (restartRequired bool, err error) {
	conf := &config.Config{}
	if err := config.LoadConfig(containerdConfig, conf); err != nil {
		return false, err
	}

	if criPlugins, ok := conf.Plugins[criPluginKey]; ok {
		if criPlugins.Has(zeropodRuntimeKey) {
			log.Println("runtime already configured, no need to restart containerd")
			return false, nil
		}
	}

	// backup the original config
	if err := copyConfig(containerdConfig, containerdConfig+configBackupSuffix); err != nil {
		return false, err
	}

	if runtime == runtimeRKE2 || runtime == runtimeK3S {
		// for rke2/k3s the containerd config has to be customized via the
		// config.toml.tmpl file. So we make a copy of the original config and
		// insert our shim config into the template.
		if out, err := exec.Command("cp", containerdConfig, containerdConfig+templateSuffix).CombinedOutput(); err != nil {
			return false, fmt.Errorf("unable to copy config.toml to template: %s: %w", out, err)
		}
		containerdConfig = containerdConfig + templateSuffix
	}

	cfg, err := os.OpenFile(containerdConfig, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return false, err
	}

	configured, containerdOptPath, err := optConfigured(containerdConfig)
	if err != nil {
		return false, err
	}

	optPath := *hostOptPath
	if configured {
		optPath = containerdOptPath
	}

	if _, err := cfg.WriteString(fmt.Sprintf(runtimeConfig, strings.TrimSuffix(optPath, "/"))); err != nil {
		return false, err
	}

	if !configured {
		if _, err := cfg.WriteString(fmt.Sprintf(optPlugin, *hostOptPath)); err != nil {
			return false, err
		}
	}

	return true, nil
}

func restoreContainerdConfig() error {
	if _, err := os.Stat(containerdConfig + configBackupSuffix); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			log.Println("could not find config backup, either it has already been restored or it never existed")
			return nil
		}
	}

	if err := copyConfig(containerdConfig+configBackupSuffix, containerdConfig); err != nil {
		return err
	}

	if err := os.Remove(containerdConfig + configBackupSuffix); err != nil {
		return err
	}

	return nil
}

func copyConfig(from, to string) error {
	originalConfig, err := os.ReadFile(from)
	if err != nil {
		return fmt.Errorf("could not read containerd config: %w", err)
	}
	if err := os.WriteFile(to, originalConfig, os.ModePerm); err != nil {
		return fmt.Errorf("could not write config backup: %w", err)
	}

	return nil
}

func optConfigured(containerdConfig string) (bool, string, error) {
	conf := &config.Config{}
	if err := config.LoadConfig(containerdConfig, conf); err != nil {
		return false, "", err
	}
	if opt, ok := conf.Plugins[containerdOptKey]; ok {
		if opt.Has("path") {
			return true, opt.Get("path").(string), nil
		}
	}

	return false, "", nil
}

func installRuntimeClass(ctx context.Context, client kubernetes.Interface) error {
	runtimeClass := &knodev1.RuntimeClass{
		ObjectMeta: metav1.ObjectMeta{Name: v1.RuntimeClassName},
		Handler:    v1.RuntimeClassName,
		Scheduling: &knodev1.Scheduling{NodeSelector: map[string]string{zeropod.NodeLabel: "true"}},
	}

	if _, err := client.NodeV1().RuntimeClasses().Create(ctx, runtimeClass, metav1.CreateOptions{}); err != nil {
		if !kerrors.IsAlreadyExists(err) {
			return err
		}
	}

	return nil
}

func removeRuntimeClass(ctx context.Context, client kubernetes.Interface) error {
	if err := client.NodeV1().RuntimeClasses().Delete(ctx, v1.RuntimeClassName, metav1.DeleteOptions{}); err != nil {
		if !kerrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func inClusterClient() (kubernetes.Interface, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}

	return kubernetes.NewForConfig(config)
}

// runUninstall removes all components installed by zeropod and restores the
// original configuration.
func runUninstall(ctx context.Context, client kubernetes.Interface) error {
	if err := removeRuntimeClass(ctx, client); err != nil {
		return err
	}

	if err := os.RemoveAll(node.OptPath); err != nil {
		return fmt.Errorf("removing opt path: %w", err)
	}

	if err := restoreContainerdConfig(); err != nil {
		return err
	}

	return nil
}

func loadTLSCA(ctx context.Context, client kubernetes.Interface) error {
	// TODO: do not hardcode
	namespace := "zeropod-system"
	secret, err := client.CoreV1().Secrets(namespace).Get(ctx, caSecretName, metav1.GetOptions{})
	if err != nil {
		if kerrors.IsNotFound(err) {
			secret, err = generateTLSCA(ctx, client, namespace)
			if err != nil {
				return err
			}
		}
	}
	if err := os.WriteFile("/tls/ca.crt", secret.Data[corev1.TLSCertKey], 0600); err != nil {
		return err
	}
	if err := os.WriteFile("/tls/ca.key", secret.Data[corev1.TLSPrivateKeyKey], 0600); err != nil {
		return err
	}

	return nil
}

func generateTLSCA(ctx context.Context, client kubernetes.Interface, namespace string) (*corev1.Secret, error) {
	ca, err := node.GenCert(nil, nil)
	if err != nil {
		return nil, err
	}

	certOut := new(bytes.Buffer)
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: ca.Certificate[0]}); err != nil {
		return nil, fmt.Errorf("failed to write data to cert: %w", err)
	}

	privBytes, err := x509.MarshalPKCS8PrivateKey(ca.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("unable to marshal private key: %w", err)
	}

	keyOut := new(bytes.Buffer)
	if err := pem.Encode(keyOut, &pem.Block{Type: "PRIVATE KEY", Bytes: privBytes}); err != nil {
		return nil, fmt.Errorf("failed to write data to key: %w", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      caSecretName,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			corev1.TLSCertKey:       certOut.Bytes(),
			corev1.TLSPrivateKeyKey: keyOut.Bytes(),
		},
		Type: corev1.SecretTypeTLS,
	}
	secret, err = client.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
	if kerrors.IsAlreadyExists(err) {
		return client.CoreV1().Secrets(namespace).Get(ctx, caSecretName, metav1.GetOptions{})
	}

	return secret, err
}
