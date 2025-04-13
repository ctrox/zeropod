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
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/cmd/containerd/server/config"
	"github.com/coreos/go-systemd/v22/dbus"
	v1 "github.com/ctrox/zeropod/api/runtime/v1"
	"github.com/ctrox/zeropod/manager/node"
	"github.com/ctrox/zeropod/shim"
	"github.com/pelletier/go-toml/v2"
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
	hostOptPath    = flag.String("host-opt-path", defaultOptPath, "path where zeropod binaries are stored on the host")
	uninstall      = flag.Bool("uninstall", false, "uninstalls zeropod by cleaning up all the files the installer created")
	installTimeout = flag.Duration("timeout", time.Minute, "duration the installer waits for the installation to complete")
)

type containerRuntime string

const (
	runtimeContainerd containerRuntime = "containerd"
	runtimeRKE2       containerRuntime = "rke2"
	runtimeK3S        containerRuntime = "k3s"

	hostRoot                    = "/host"
	binPath                     = "bin/"
	criuConfigFile              = "/etc/criu/default.conf"
	shimBinaryName              = "containerd-shim-zeropod-v2"
	runtimePath                 = "/build/" + shimBinaryName
	defaultContainerdConfigPath = "/etc/containerd/config.toml"
	containerdSock              = "/run/containerd/containerd.sock"
	configBackupSuffix          = ".original"
	templateSuffix              = ".tmpl"
	caSecretName                = "ca-cert"
	defaultCriuBin              = "criu"
	criuIPTablesBin             = "criu-iptables"
	criuConfig                  = `tcp-close
skip-in-flight
network-lock skip
`
	defaultOptPath    = "/opt/zeropod"
	containerdOptKey  = "io.containerd.internal.v1.opt"
	criPluginKey      = "io.containerd.grpc.v1.cri"
	zeropodRuntimeKey = "containerd.runtimes.zeropod"
	optPlugin         = `
[plugins."io.containerd.internal.v1.opt"]
  path = "%s"
`
	zeropodTomlName = "runtime_zeropod.toml"
	runtimeConfigV3 = `version = 3

[plugins."io.containerd.cri.v1.runtime".containerd.runtimes.zeropod]
  runtime_type = "io.containerd.runc.v2"
  runtime_path = "%s/bin/containerd-shim-zeropod-v2"
  pod_annotations = [
    "zeropod.ctrox.dev/ports-map",
    "zeropod.ctrox.dev/container-names",
    "zeropod.ctrox.dev/scaledown-duration",
    "zeropod.ctrox.dev/disable-checkpointing",
    "zeropod.ctrox.dev/pre-dump",
    "zeropod.ctrox.dev/migrate",
    "zeropod.ctrox.dev/live-migrate",
    "io.containerd.runc.v2.group"
  ]

  [plugins."io.containerd.cri.v1.runtime".containerd.runtimes.zeropod.options]
    # use systemd cgroup by default
    SystemdCgroup = true
`
	configVersion2 = "version = 2"
	runtimeConfig  = `
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
    "zeropod.ctrox.dev/live-migrate",
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
		if err := runUninstall(ctx, client, containerRuntime(*runtime)); err != nil {
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
	client, err := containerd.New(containerdSock, containerd.WithDefaultNamespace("k8s"))
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
		containerd.WithInstallPath(optPath(ctx, containerRuntime(*runtime))),
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
	shimDest := filepath.Join(optPath(ctx, runtime), binPath, shimBinaryName)
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

	if runtime == runtimeK3S {
		// for some reason, k3s containerd only has access to the busybox tar by
		// default. This breaks criu checkpoint since it needs the full gnu tar.
		// To work around this, we symlink tar in our opt path to /bin/tar.
		if err := linkTar(*hostOptPath); err != nil {
			return fmt.Errorf("unable to link tar: %w", err)
		}
	}

	restartRequired, err := configureContainerd(ctx, runtime)
	if err != nil {
		if restoreErr := restoreContainerdConfig(runtime, defaultContainerdConfigPath); restoreErr != nil {
			return fmt.Errorf("unable to configure and restore containerd config: %w: %w", restoreErr, err)
		}
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

func configureContainerd(ctx context.Context, runtime containerRuntime) (restartRequired bool, err error) {
	client, err := containerd.New(containerdSock, containerd.WithDefaultNamespace("k8s"))
	if err != nil {
		return false, fmt.Errorf("creating containerd client: %w", err)
	}

	v, err := client.Version(ctx)
	if err != nil {
		return false, fmt.Errorf("getting containerd version: %w", err)
	}
	log.Printf("configuring containerd %s", v.Version)
	if strings.HasPrefix(v.Version, "1") || strings.HasPrefix(v.Version, "v1") {
		return configureContainerdv1(ctx, runtime, defaultContainerdConfigPath)
	}
	return configureContainerdv2(ctx, runtime, defaultContainerdConfigPath)
}

func configureContainerdv2(ctx context.Context, runtime containerRuntime, containerdConfig string) (bool, error) {
	if err := migrateToImports(runtime, containerdConfig); err != nil {
		return false, fmt.Errorf("migrating to imports: %w", err)
	}

	conf := &config.Config{}
	if err := config.LoadConfig(ctx, containerdConfig, conf); err != nil {
		return false, fmt.Errorf("loading containerd config: %w", err)
	}

	if zeropodImportConfigured(conf.Imports) {
		log.Println("runtime already configured, no need to restart containerd")
		return false, nil
	}

	existingOpt, containerdOptPath, err := optConfigured(ctx, containerdConfig)
	if err != nil {
		return false, fmt.Errorf("could not check opt configuration: %w", err)
	}

	if err := backupContainerdConfig(containerdConfig); err != nil {
		return false, fmt.Errorf("backing up containerd config: %w", err)
	}

	if runtime == runtimeRKE2 || runtime == runtimeK3S {
		// for rke2/k3s the containerd config has to be customized via the
		// config.toml.tmpl file. So we make a copy of the original config and
		// insert our shim config into the template.
		if err := copyConfig(containerdConfig, containerdConfig+templateSuffix); err != nil {
			return false, fmt.Errorf("unable to copy config template: %w", err)
		}
		containerdConfig = containerdConfig + templateSuffix
	}

	if err := addZeropodConfigImport(containerdConfig, conf); err != nil {
		return false, err
	}

	optPath := *hostOptPath
	if existingOpt {
		optPath = containerdOptPath
	}

	if err := writeZeropodRuntimeConfig(containerdConfig, optPath, existingOpt, conf.Version); err != nil {
		return false, err
	}

	// sanity check config by loading it again
	if err := config.LoadConfig(ctx, containerdConfig, &config.Config{}); err != nil {
		return false, fmt.Errorf("loading modified containerd config: %w", err)
	}

	return true, nil
}

func configureContainerdv1(ctx context.Context, runtime containerRuntime, containerdConfig string) (bool, error) {
	confContents, err := os.ReadFile(containerdConfig)
	if err != nil {
		return false, err
	}
	if strings.Contains(string(confContents), zeropodRuntimeKey) {
		log.Println("runtime already configured, no need to restart containerd")
		return false, nil
	}

	// backup the original config
	if err := copyConfig(containerdConfig, containerdConfig+configBackupSuffix); err != nil {
		return false, err
	}

	if runtime == runtimeRKE2 || runtime == runtimeK3S {
		// for rke2/k3s the containerd config has to be customized via the
		// config.toml.tmpl file. So we make a copy of the original config and
		// insert our shim config into the template.
		if err := copyConfig(containerdConfig, containerdConfig+templateSuffix); err != nil {
			return false, fmt.Errorf("unable to copy config template: %w", err)
		}
		containerdConfig = containerdConfig + templateSuffix
	}

	cfg, err := os.OpenFile(containerdConfig, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return false, err
	}

	configured, containerdOptPath, err := optConfigured(ctx, containerdConfig)
	if err != nil {
		return false, err
	}

	optPath := *hostOptPath
	if configured {
		optPath = containerdOptPath
	}

	if _, err := fmt.Fprintf(cfg, runtimeConfig, strings.TrimSuffix(optPath, "/")); err != nil {
		return false, err
	}

	if !configured {
		if _, err := fmt.Fprintf(cfg, optPlugin, *hostOptPath); err != nil {
			return false, err
		}
	}

	return true, nil
}

func addZeropodConfigImport(containerdConfigPath string, conf *config.Config) error {
	importsConf := struct {
		Imports []string `toml:"imports"`
	}{}
	importsConf.Imports = conf.Imports
	importsConf.Imports = append(importsConf.Imports, zeropodTomlName)
	imports, err := toml.Marshal(importsConf)
	if err != nil {
		return err
	}

	cfgData, err := os.ReadFile(containerdConfigPath)
	if err != nil {
		return fmt.Errorf("opening containerd config: %w", err)
	}
	lines := strings.Split(string(cfgData), "\n")

	start, end, found := findImportsLines(lines)
	if found {
		if start == end {
			end++
		}
		lines = slices.Delete(lines, start, end)
	}

	vLine, ok := versionLine(lines)
	if !ok {
		return fmt.Errorf("version not found in containerd config")
	}
	lines = slices.Insert(lines, vLine+1, strings.TrimSpace(string(imports)))

	if err := os.WriteFile(containerdConfigPath, []byte(strings.Join(lines, "\n")), 0644); err != nil {
		return fmt.Errorf("writing containerd config: %w", err)
	}
	return nil
}

func versionLine(lines []string) (pos int, found bool) {
	for i, line := range lines {
		if strings.Contains(strings.TrimSpace(line), "version") {
			return i, true
		}
	}
	return 0, false
}

func findImportsLines(lines []string) (start int, end int, found bool) {
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "imports") {
			// handle multiline array
			if !strings.HasSuffix(strings.TrimSpace(line), "]") {
				for j, line := range lines[i:] {
					if strings.HasSuffix(strings.TrimSpace(line), "]") {
						return i, i + j + 1, true
					}
				}
			}
			return i, i, true
		}
	}
	return 0, 0, false
}

func zeropodImportConfigured(imports []string) bool {
	for _, imp := range imports {
		if filepath.Base(imp) == zeropodTomlName {
			return true
		}
	}
	return false
}

func zeropodRuntimeConfigPath(containerdConfig string) string {
	return filepath.Join(filepath.Dir(containerdConfig), zeropodTomlName)
}

func backupContainerdConfig(containerdConfig string) error {
	return copyConfig(containerdConfig, containerdConfig+configBackupSuffix)
}

func writeZeropodRuntimeConfig(containerdConfig, optPath string, existingOpt bool, version int) error {
	zeropodRuntimeConfig := fmt.Sprintf("%s\n%s", configVersion2, runtimeConfig)
	if version == 3 {
		zeropodRuntimeConfig = runtimeConfigV3
	}
	zeropodRuntimeConfig = fmt.Sprintf(zeropodRuntimeConfig, strings.TrimSuffix(optPath, "/"))
	if !existingOpt {
		zeropodRuntimeConfig = zeropodRuntimeConfig + fmt.Sprintf(optPlugin, optPath)
	}
	if err := os.WriteFile(zeropodRuntimeConfigPath(containerdConfig), []byte(zeropodRuntimeConfig), 0644); err != nil {
		return fmt.Errorf("writing zeropod runtime config: %w", err)
	}
	return nil
}

func restoreContainerdConfig(runtime containerRuntime, containerdConfigPath string) error {
	if _, err := os.Stat(containerdConfigPath + configBackupSuffix); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			log.Println("could not find config backup, either it has already been restored or it never existed")
			return nil
		}
	}

	if err := copyConfig(containerdConfigPath+configBackupSuffix, containerdConfigFile(runtime, containerdConfigPath)); err != nil {
		return err
	}

	if err := os.Remove(containerdConfigPath + configBackupSuffix); err != nil {
		return err
	}

	return nil
}

func migrateToImports(runtime containerRuntime, containerdConfigPath string) error {
	cfg, err := os.ReadFile(containerdConfigFile(runtime, containerdConfigPath))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("unable to read containerd config: %w", err)
	}
	if strings.Contains(string(cfg), "containerd.runtimes.zeropod") {
		if err := restoreContainerdConfig(runtime, containerdConfigPath); err != nil {
			return fmt.Errorf("unable to restore original config: %w", err)
		}
	}
	return nil
}

func containerdConfigFile(runtime containerRuntime, containerdConfigPath string) string {
	if strings.HasSuffix(containerdConfigPath, templateSuffix) {
		return containerdConfigPath
	}

	if runtime == runtimeRKE2 || runtime == runtimeK3S {
		containerdConfigPath = containerdConfigPath + templateSuffix
	}

	return containerdConfigPath
}

func copyConfig(from, to string) error {
	info, err := os.Stat(from)
	if err != nil {
		return fmt.Errorf("could not stat containerd config: %w", err)
	}
	originalConfig, err := os.ReadFile(from)
	if err != nil {
		return fmt.Errorf("could not read containerd config: %w", err)
	}
	if err := os.WriteFile(to, originalConfig, info.Mode()); err != nil {
		return fmt.Errorf("could not write config backup: %w", err)
	}

	return nil
}

func optConfigured(ctx context.Context, containerdConfig string) (bool, string, error) {
	conf := &config.Config{}
	if err := config.LoadConfig(ctx, containerdConfig, conf); err != nil {
		return false, "", err
	}
	if _, ok := conf.Plugins[containerdOptKey]; ok {
		optConfig := struct {
			Path string `toml:"path"`
		}{}

		if _, err := conf.Decode(ctx, containerdOptKey, &optConfig); err != nil {
			return false, "", err
		}

		if optConfig.Path != "" {
			return true, optConfig.Path, nil
		}
	}

	return false, "", nil
}

func optPath(ctx context.Context, runtime containerRuntime) string {
	ok, path, err := optConfigured(ctx, containerdConfigFile(runtime, defaultContainerdConfigPath))
	if err != nil {
		return defaultOptPath
	}
	if ok {
		return filepath.Join(hostRoot, path)
	}
	return defaultOptPath
}

func installRuntimeClass(ctx context.Context, client kubernetes.Interface) error {
	runtimeClass := &knodev1.RuntimeClass{
		ObjectMeta: metav1.ObjectMeta{Name: v1.RuntimeClassName},
		Handler:    v1.RuntimeClassName,
		Scheduling: &knodev1.Scheduling{NodeSelector: map[string]string{shim.NodeLabel: "true"}},
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
func runUninstall(ctx context.Context, client kubernetes.Interface, runtime containerRuntime) error {
	if err := removeRuntimeClass(ctx, client); err != nil {
		return err
	}

	if err := os.RemoveAll(optPath(ctx, runtime)); err != nil {
		return fmt.Errorf("removing opt path: %w", err)
	}

	if err := restoreContainerdConfig(runtime, defaultContainerdConfigPath); err != nil {
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

func linkTar(opt string) error {
	if err := os.Symlink("/bin/tar", filepath.Join(opt, "bin", "tar")); !os.IsExist(err) {
		return err
	}
	return nil
}
