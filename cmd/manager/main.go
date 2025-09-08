package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	goruntime "runtime"
	"runtime/debug"
	"syscall"

	nodev1 "github.com/ctrox/zeropod/api/node/v1"
	v1 "github.com/ctrox/zeropod/api/runtime/v1"
	"github.com/ctrox/zeropod/manager"
	"github.com/ctrox/zeropod/manager/node"
	"github.com/ctrox/zeropod/socket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	ctrlmanager "sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var (
	metricsAddr    = flag.String("metrics-addr", ":8080", "address of the metrics server")
	nodeServerAddr = flag.String("node-server-addr", ":8090", "address of the node server")
	debugFlag      = flag.Bool("debug", false, "enable debug logs")
	inPlaceScaling = flag.Bool("in-place-scaling", false,
		"enable in-place resource scaling, requires InPlacePodVerticalScaling feature flag")
	statusLabels    = flag.Bool("status-labels", false, "update pod labels to reflect container status")
	probeBinaryName = flag.String("probe-binary-name", "kubelet", "set the probe binary name for probe detection")
	statusEvents    = flag.Bool("status-events", false, "create status events to reflect container status")
	versionFlag     = flag.Bool("version", false, "output version and exit")

	version   = ""
	revision  = ""
	goVersion = goruntime.Version()
)

func init() {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}

	if version == "" {
		version = info.Main.Version
	}

	for _, kv := range info.Settings {
		switch kv.Key {
		case "vcs.revision":
			revision = kv.Value
		}
	}
}

func main() {
	flag.Parse()

	if *versionFlag {
		printVersion()
		os.Exit(0)
	}

	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if *debugFlag {
		opts.Level = slog.LevelDebug
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, opts))
	log.Info("starting manager",
		"metrics-addr", *metricsAddr,
		"node-server-addr", *nodeServerAddr,
		"version", version,
		"revision", revision,
		"go", goVersion,
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tracker, cleanSocketTracker, err := socket.LoadEBPFTracker(*probeBinaryName)
	if err != nil {
		log.Warn("loading socket tracker failed, scaling down with static duration", "err", err)
		cleanSocketTracker = func() error { return nil }
	}

	if err := manager.AttachRedirectors(ctx, log, tracker); err != nil {
		log.Warn("attaching redirectors failed: restoring containers on traffic is disabled", "err", err)
	}

	mgr, err := newControllerManager()
	if err != nil {
		log.Error("creating controller manager", "err", err)
		os.Exit(1)
	}

	podHandlers := []manager.PodHandler{}
	if *statusLabels {
		podHandlers = append(podHandlers, manager.NewPodLabeller(log))
	}
	if *inPlaceScaling {
		podHandlers = append(podHandlers, manager.NewPodScaler(log))
	}
	if *statusEvents {
		podHandlers = append(podHandlers, manager.NewEventCreator(log))
	}

	col := manager.NewCollector()
	sc := manager.SubscriberConfig{Log: log, Kube: mgr.GetClient(), Collector: col}
	if err := manager.StartSubscribers(ctx, sc, podHandlers...); err != nil {
		log.Error("starting subscribers", "err", err)
		os.Exit(1)
	}

	registry := prometheus.NewRegistry()
	if err := registry.Register(col); err != nil {
		slog.Error("registering metrics", "err", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(
		registry,
		promhttp.HandlerOpts{
			EnableOpenMetrics: true,
		}),
	)
	server := &http.Server{Addr: *metricsAddr, Handler: mux}

	go func() {
		if err := server.ListenAndServe(); err != nil {
			if !errors.Is(err, http.ErrServerClosed) {
				log.Error("serving metrics", "err", err)
				os.Exit(1)
			}
		}
	}()

	nodeServer, err := node.NewServer(*nodeServerAddr, mgr.GetClient(), log)
	if err != nil {
		log.Error("creating node server", "err", err)
		os.Exit(1)
	}
	go nodeServer.Start(ctx)

	if err := manager.NewPodController(ctx, mgr, log); err != nil {
		log.Error("running pod controller", "error", err)
	}

	go func() {
		if err := mgr.Start(ctx); err != nil {
			log.Error("starting controller manager", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	log.Info("stopping manager")
	cleanSocketTracker()
	if err := server.Shutdown(ctx); err != nil {
		log.Error("shutting down server", "err", err)
	}
}

func newControllerManager() (ctrlmanager.Manager, error) {
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("getting client config: %w", err)
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := v1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	nodeName, ok := os.LookupEnv(nodev1.NodeNameEnvKey)
	if !ok {
		return nil, fmt.Errorf("could not find node name, env %s is not set", nodev1.NodeNameEnvKey)
	}
	mgr, err := ctrlmanager.New(cfg, ctrlmanager.Options{
		Scheme: scheme, Metrics: server.Options{BindAddress: "0"},
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				// for pods we're only interested in objects that are running on
				// the same node as the manager. This will reduce memory usage
				// as we only keep a subset of all pods in the cache.
				&corev1.Pod{}: cache.ByObject{
					Field: fields.SelectorFromSet(fields.Set{
						"spec.nodeName": nodeName,
					}),
				},
			},
		},
	})
	if err != nil {
		return nil, err
	}
	return mgr, nil
}

func printVersion() {
	fmt.Printf("%s:\n", filepath.Base(os.Args[0]))
	fmt.Println("  Version: ", version)
	fmt.Println("  Revision:", revision)
	fmt.Println("  Go version:", goVersion)
	fmt.Println("")
}
