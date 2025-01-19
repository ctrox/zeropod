package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/ctrox/zeropod/manager"
	"github.com/ctrox/zeropod/socket"
)

var (
	metricsAddr    = flag.String("metrics-addr", ":8080", "address of the metrics server")
	debug          = flag.Bool("debug", false, "enable debug logs")
	inPlaceScaling = flag.Bool("in-place-scaling", false,
		"enable in-place resource scaling, requires InPlacePodVerticalScaling feature flag")
	statusLabels = flag.Bool("status-labels", false, "update pod labels to reflect container status")
)

func main() {
	flag.Parse()

	opts := &slog.HandlerOptions{}
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
		slog.Error("loading socket tracker", "err", err)
		os.Exit(1)
	}

	podHandlers := []manager.PodHandler{}
	if *statusLabels {
		podHandlers = append(podHandlers, manager.NewPodLabeller(log))
	}
	if *inPlaceScaling {
		podHandlers = append(podHandlers, manager.NewPodScaler(log))
	}

	if err := manager.StartSubscribers(ctx, log, podHandlers...); err != nil {
		slog.Error("starting subscribers", "err", err)
		os.Exit(1)
	}

	server := &http.Server{Addr: *metricsAddr}
	http.HandleFunc("/metrics", manager.Handler)

	go func() {
		if err := server.ListenAndServe(); err != nil {
			if !errors.Is(err, http.ErrServerClosed) {
				slog.Error("serving metrics", "err", err)
				os.Exit(1)
			}
		}
	}()

	<-ctx.Done()
	slog.Info("stopping manager")
	cleanSocketTracker()
	if err := server.Shutdown(ctx); err != nil {
		slog.Error("shutting down server", "err", err)
	}
}
