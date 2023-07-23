package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"syscall"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/services/server/config"
	"github.com/coreos/go-systemd/v22/dbus"
	nodev1 "k8s.io/api/node/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var (
	criuImage    = flag.String("criu-image", "ghcr.io/ctrox/zeropod-criu:a2c4dd2", "criu image to use.")
	criuNFTables = flag.Bool("criu-nftables", true, "use criu with nftables")
	runtime      = flag.String("runtime", "containerd", "specifies which runtime to configure. containerd/k3s/rke2")
	hostOptPath  = flag.String("host-opt-path", "/opt/zeropod", "path where zeropod binaries are stored on the host")
)

type containerRuntime string

const (
	runtimeContainerd containerRuntime = "containerd"
	runtimeRKE2       containerRuntime = "rke2"
	runtimeK3S        containerRuntime = "k3s"

	optPath          = "/opt/zeropod"
	binPath          = "bin/"
	criuConfigFile   = "/etc/criu/default.conf"
	shimBinaryName   = "containerd-shim-zeropod-v2"
	runtimePath      = "/build/" + shimBinaryName
	containerdConfig = "/etc/containerd/config.toml"
	templateSuffix   = ".tmpl"
	runtimeClassName = "zeropod"
	runtimeHandler   = "zeropod"
	defaultCriuBin   = "criu"
	criuIPTablesBin  = "criu-iptables"
	criuConfig       = `tcp-close
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
  runtime_type = "io.containerd.zeropod.v2"
  pod_annotations = [
    "zeropod.ctrox.dev/ports-map",
    "zeropod.ctrox.dev/container-names",
    "zeropod.ctrox.dev/scaledown-duration",
    "zeropod.ctrox.dev/disable-checkpointing",
    "zeropod.ctrox.dev/pre-dump"
  ]
`
)

func main() {
	flag.Parse()

	if err := installCriu(); err != nil {
		log.Fatalf("error installing criu: %s", err)
	}

	log.Println("installed criu binaries")

	if err := installRuntime(containerRuntime(*runtime)); err != nil {
		log.Fatalf("error installing runtime: %s", err)
	}

	log.Println("installed runtime")

	if err := installRuntimeClass(); err != nil {
		log.Fatalf("error installing zeropod runtimeClass: %s", err)
	}

	log.Println("installed runtimeClass")

	log.Println("installer completed")

	go func() {
		for {
			time.Sleep(time.Second)
		}
	}()

	quitChannel := make(chan os.Signal, 1)
	signal.Notify(quitChannel, syscall.SIGINT, syscall.SIGTERM)
	<-quitChannel
}

func installCriu() error {
	client, err := containerd.New("/run/containerd/containerd.sock", containerd.WithDefaultNamespace("k8s"))
	if err != nil {
		return err
	}

	ctx := context.Background()

	image, err := client.Pull(ctx, *criuImage)
	if err != nil {
		return err
	}

	if err := client.Install(
		ctx, image, containerd.WithInstallLibs,
		containerd.WithInstallReplace,
		containerd.WithInstallPath(optPath),
	); err != nil {
		return err
	}

	if !*criuNFTables {
		log.Println("nftables disabled, installing criu with iptables")
		// if we don't have nftables support, we need to use the criu binary
		// without nftables support compiled in as the config alone does not seem
		// to do the trick :/
		if err := os.Rename(filepath.Join(optPath, "bin", criuIPTablesBin), filepath.Join(optPath, "bin", defaultCriuBin)); err != nil {
			return err
		}
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

func installRuntime(runtime containerRuntime) error {
	log.Printf("installing runtime for %s", runtime)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()
	conn, err := dbus.NewSystemdConnectionContext(context.Background())
	if err != nil {
		return fmt.Errorf("unable to connect to dbus: %w", err)
	}

	// note that if the shim binary already exists, we simply switch it out with
	// the new one but existing zeropods will have to be deleted to use the
	// updated shim.
	shimDest := filepath.Join(optPath, binPath, shimBinaryName)
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

	restartRequired, err := configureContainerd(runtime)
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

func configureContainerd(runtime containerRuntime) (restartRequired bool, err error) {
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

	containerdCfg := containerdConfig

	if runtime == runtimeRKE2 || runtime == runtimeK3S {
		// for rke2/k3s the containerd config has to be customized via the
		// config.toml.tmpl file. So we make a copy of the original config and
		// insert our shim config into the template.
		if out, err := exec.Command("cp", containerdCfg, containerdCfg+templateSuffix).CombinedOutput(); err != nil {
			return false, fmt.Errorf("unable to copy config.toml to template: %s: %w", out, err)
		}
		containerdCfg = containerdConfig + templateSuffix
	}

	cfg, err := os.OpenFile(containerdCfg, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return false, err
	}

	if _, err := cfg.WriteString(runtimeConfig); err != nil {
		return false, err
	}

	configured, err := optConfigured()
	if err != nil {
		return false, err
	}

	if !configured {
		if _, err := cfg.WriteString(fmt.Sprintf(optPlugin, *hostOptPath)); err != nil {
			return false, err
		}
	}

	return true, nil
}

func optConfigured() (bool, error) {
	conf := &config.Config{}
	if err := config.LoadConfig(containerdConfig, conf); err != nil {
		return false, err
	}
	if opt, ok := conf.Plugins[containerdOptKey]; ok {
		if opt.Has("path") {
			return true, nil
		}
	}

	return false, nil
}

func installRuntimeClass() error {
	config, err := rest.InClusterConfig()
	if err != nil {
		return err
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}

	runtimeClass := &nodev1.RuntimeClass{
		ObjectMeta: v1.ObjectMeta{Name: runtimeClassName},
		Handler:    runtimeHandler,
	}

	if _, err := clientset.NodeV1().RuntimeClasses().Create(
		context.Background(), runtimeClass, v1.CreateOptions{},
	); err != nil {
		if !kerrors.IsAlreadyExists(err) {
			return err
		}
	}

	return nil
}
