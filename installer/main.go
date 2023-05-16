package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"syscall"
	"time"

	"github.com/containerd/containerd"
	"github.com/coreos/go-systemd/v22/dbus"
	nodev1 "k8s.io/api/node/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var (
	image = flag.String("image", "", "installer image with binaries and libs for containerd")
)

const (
	optPath          = "/opt/zeropod"
	binPath          = "/opt/zeropod/bin/"
	criuImage        = "docker.io/ctrox/criu:v3.18"
	criuConfigFile   = "/etc/criu/default.conf"
	shimBinaryName   = "containerd-shim-zeropod-v2"
	runtimePath      = "/build/" + shimBinaryName
	containerdConfig = "/etc/containerd/config.toml"
	runtimeClassName = "zeropod"
	runtimeHandler   = "zeropod"
	criuConfig       = `tcp-close
skip-in-flight
network-lock iptables
`
	runtimeConfig = `
[plugins."io.containerd.internal.v1.opt"]
  path = "/opt/zeropod"

[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.zeropod]
  runtime_type = "io.containerd.zeropod.v2"
  pod_annotations = [
    "zeropod.ctrox.dev/port",
    "zeropod.ctrox.dev/container-name",
    "zeropod.ctrox.dev/scaledownduration",
    "zeropod.ctrox.dev/stateful"
  ]
`
)

func main() {
	if err := installCriu(); err != nil {
		log.Fatalf("Error installing criu: %s", err)
	}

	log.Println("installed criu binaries")

	if out, err := installRuntime(); err != nil {
		log.Fatalf("Error installing runtime: %s: %s", out, err)
	}

	log.Println("installed runtime")

	if err := installRuntimeClass(); err != nil {
		log.Fatalf("Error installing zeropod runtimeClass: %s", err)
	}

	log.Println("installed runtimeClass")

	log.Println("installed completed")

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
	if err := os.MkdirAll(path.Dir(criuConfigFile), os.ModePerm); err != nil {
		return err
	}

	if err := ioutil.WriteFile(criuConfigFile, []byte(criuConfig), 0644); err != nil {
		return err
	}

	client, err := containerd.New("/run/containerd/containerd.sock", containerd.WithDefaultNamespace("k8s"))
	if err != nil {
		return err
	}

	ctx := context.Background()

	image, err := client.Pull(ctx, criuImage)
	if err != nil {
		return err
	}

	return client.Install(ctx, image, containerd.WithInstallLibs, containerd.WithInstallReplace, containerd.WithInstallPath(optPath))
}

func installRuntime() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()
	conn, err := dbus.NewSystemdConnectionContext(context.Background())
	if err != nil {
		return nil, fmt.Errorf("unable to connect to dbus: %w", err)
	}

	// note that if the shim binary already exists, we simply switch it out with
	// the new one but existing zeropods will have to be deleted to use the
	// updated shim.
	if err := os.Remove(path.Join(binPath, shimBinaryName)); err != nil {
		log.Printf("unable to remove shim binary, continuing with install: %s", err)
	}

	if out, err := exec.Command("cp", runtimePath, binPath).CombinedOutput(); err != nil {
		return out, err
	}

	if err := exec.Command("grep", "-q", runtimeHandler, containerdConfig).Run(); err == nil {
		// runtime already configured
		log.Println("runtime already configured, no need to restart containerd")
		return nil, nil
	}

	cfg, err := os.OpenFile(containerdConfig, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	if _, err := cfg.WriteString(runtimeConfig); err != nil {
		return nil, err
	}

	ch := make(chan string)
	if _, err := conn.RestartUnitContext(ctx, "containerd.service", "replace", ch); err != nil {
		return nil, fmt.Errorf("unable to restart containerd")
	}
	<-ch
	return nil, nil
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
