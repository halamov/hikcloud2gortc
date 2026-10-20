// hikcloud2gortc — стриминг Hik-Connect NVR на Go.
//
// Воркеры держат VTM-сессии открытыми постоянно; /stream.ts отдаёт TS,
// /frame — JPEG с I-кадра, /snapshot//preview — облачные снапшоты.
package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"hikcloud2gortc/internal/api"
	"hikcloud2gortc/internal/cloudcam"
	"hikcloud2gortc/internal/config"
	"hikcloud2gortc/internal/worker"
)

func main() {
	cfg := config.C
	if cfg.User == "" || cfg.Password == "" {
		fmt.Fprintln(os.Stderr, "HIK_USER/HIK_PASSWORD не заданы")
		os.Exit(2)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "DATA_DIR:", err)
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	// облачный клиент: токен -> refresh -> login
	client := cloudcam.NewClient(cfg.User, cfg.Password, cfg.Platform, cfg.TokenFile)
	if client.LoadToken() && !client.IsRefreshNeeded(24 * time.Hour) {
		log.Info("сессия возобновлена из токена", "age_s", int(client.SessionAge().Seconds()))
	} else {
		if err := client.Login(); err != nil {
			log.Error("логин в облаке не удался", "err", err)
			if !cfg.AllowLoginFail {
				os.Exit(2)
			}
		} else {
			if err := client.SaveToken(); err != nil {
				log.Warn("не сохранил токен", "err", err)
			}
			log.Info("логин в облаке ok", "platform", cfg.Platform)
		}
	}

	cache := cloudcam.NewVtmCache(cfg.TokenFile)
	snap := cloudcam.NewSnapClient(cfg.User, cfg.Password, log)
	mgr := worker.NewManager(client, cache, cfg.DataDir, cfg.WorkerMode, int(cfg.WorkerIdleTimeout), cfg.PaceFrames, log)

	srv := api.NewServer(api.Config{
		Port:            cfg.Port,
		DefaultChannel:  cfg.Channel,
		DefaultStream:   cfg.Stream,
		OfflineResponse: cfg.OfflineResponse,
	}, client, snap, mgr, log)

	httpSrv := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Port),
		Handler: srv.Handler(),
	}

	// graceful stop: SIGTERM/SIGINT → все воркеры
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		log.Info("остановка...")
		mgr.StopAll()
		os.Exit(0)
	}()

	log.Info("hikcloud2gortc", "port", cfg.Port, "worker_mode", cfg.WorkerMode,
		"offline_response", cfg.OfflineResponse, "pace", cfg.PaceFrames)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("http", "err", err)
		os.Exit(1)
	}
}
