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
	"syscall"

	v1 "github.com/ctrox/zeropod/api/runtime/v1"
	"github.com/ctrox/zeropod/manager"
	"github.com/ctrox/zeropod/manager/node"
	"github.com/ctrox/zeropod/socket"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	ctrlmanager "sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var (
	metricsAddr    = flag.String("metrics-addr", ":8080", "address of the metrics server")
	nodeServerAddr = flag.String("node-server-addr", ":8090", "address of the node server")
	debug          = flag.Bool("debug", false, "enable debug logs")
	inPlaceScaling = flag.Bool("in-place-scaling", false,
		"enable in-place resource scaling, requires InPlacePodVerticalScaling feature flag")
	statusLabels = flag.Bool("status-labels", false, "update pod labels to reflect container status")
)

func main() {
	flag.Parse()

	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if *debug {
		opts.Level = slog.LevelDebug
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, opts))
	log.Info("starting manager", "metrics-addr", *metricsAddr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := manager.AttachRedirectors(ctx, log); err != nil {
		log.Error("attaching redirectors", "err", err)
		os.Exit(1)
	}

	cleanSocketTracker, err := socket.LoadEBPFTracker()
	if err != nil {
		log.Error("loading socket tracker", "err", err)
		os.Exit(1)
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

	if err := manager.StartSubscribers(ctx, log, mgr.GetClient(), podHandlers...); err != nil {
		log.Error("starting subscribers", "err", err)
		os.Exit(1)
	}

	server := &http.Server{Addr: *metricsAddr}
	http.HandleFunc("/metrics", manager.Handler)

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
	mgr, err := ctrlmanager.New(cfg, ctrlmanager.Options{
		Scheme: scheme, Metrics: server.Options{BindAddress: "0"},
	})
	if err != nil {
		return nil, err
	}
	return mgr, nil
}
