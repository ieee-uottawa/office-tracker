// Package main is the entry point for the office tracker application. It initializes all components, starts the HTTP server and Discord bot, and handles graceful shutdown.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"office/internal/app"
	"office/internal/config"
	"office/internal/database"
	"office/internal/logging"
	"office/internal/service"
	"office/internal/transport/discord"
	httptransport "office/internal/transport/http"
	usbtransport "office/internal/transport/usb"
)

func main() {
	// Setup logging
	logging.Setup()
	var log = logging.Component("main")

	log.Info("Starting office tracker application")
	runCtx, stopSignal := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignal()

	// Load .env into the process environment if present (local development).
	// Real environment variables (e.g. set by Docker/systemd) always take
	// precedence and are left untouched when no .env file exists.
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		log.Warn("failed to load .env file", "err", err)
	}

	// Load configuration from environment variables
	cfg := config.Load()

	// Database
	db := database.Open(cfg.DBPath)
	defer db.Close()

	if err := database.Migrate(db); err != nil {
		log.Error("migration failed", "err", err)
		panic(err)
	}

	// Create middleware config from environment
	mwConfig := httptransport.MiddlewareConfig{
		APIKey:        cfg.APIKey,
		CORSOrigins:   cfg.CORSOrigins,
		CORSEnabled:   cfg.CORSEnabled,
		APIKeyEnabled: cfg.APIKeyEnabled,
	}

	// Initialize application with all dependencies
	application := app.New(db, cfg.HTTPPort, mwConfig)

	// Reports service defaults to unavailable unless Discord delivery is configured.
	reportsService := service.NewReportsService(application.Services.Stats, nil, false)
	application.Services.Reports = reportsService
	application.Services.Scheduler.SetReportsService(reportsService)

	// Discord bot
	bot, err := discord.New(cfg.DiscordToken, application.Services, cfg.DiscordExecGuildID, cfg.DiscordCommunityGuildID)
	if err != nil {
		log.Error("failed to create Discord bot", "err", err)
		panic(err)
	}

	// Configure reports if enabled
	if bot != nil && cfg.ReportsEnabled {
		reportsDelivery := discord.NewReportsDelivery(bot.Session(), cfg.DiscordReportsChannelID)
		reportsService = service.NewReportsService(application.Services.Stats, reportsDelivery, true)
		application.Services.Reports = reportsService
		application.Services.Scheduler.SetReportsService(reportsService)
		log.Info("reports service configured")
	}

	// Start scheduler service (after reports are configured)
	if err := application.Services.Scheduler.Start(); err != nil {
		log.Error("failed to start scheduler", "err", err)
		panic(err)
	}

	if bot != nil {
		// Wire attendance-change callback so scans and API-driven check-ins trigger dashboard refresh
		service.SetAttendanceChangeCallback(func() {
			if bot != nil {
				bot.TriggerRender()
			}
		})
		service.SetEnvironmentChangeCallback(func() {
			if bot != nil {
				bot.TriggerRender()
			}
		})

	}
	// Local transports must start before any calls to Discord. Internet outages
	// or an unsynchronized clock must not take down the card reader.
	application.HTTP = httptransport.New(cfg.HTTPPort, application.Services.Attendance, application.Services.User, application.Services.Session, application.Services.Stats, application.Services.Environment, reportsService, mwConfig)
	application.HTTP.Reader = &service.USBReaderService{DB: db, Attendance: application.Services.Attendance, Health: application.HTTP.DeviceHealth}
	application.HTTP.Start()
	usbDone := make(chan struct{})
	if path := os.Getenv("USB_READER_PORT"); path != "" {
		reader := &service.USBReaderService{DB: db, Attendance: application.Services.Attendance, Health: application.HTTP.DeviceHealth}
		go func() { defer close(usbDone); usbtransport.Run(runCtx, path, reader.Handle) }()
	} else {
		close(usbDone)
	}
	if bot != nil {
		for runCtx.Err() == nil {
			err := bot.InitializeDashboards(cfg.DiscordExecGuildID, cfg.DiscordCommunityGuildID, cfg.DiscordDashboardChannelName)
			if err == nil {
				err = bot.Start()
			}
			if err == nil {
				break
			}
			log.Error("Discord unavailable; local scanning remains active; retrying", "err", err)
			select {
			case <-runCtx.Done():
			case <-time.After(10 * time.Second):
			}
		}
	}

	log.Info("office-tracker running")

	// Graceful shutdown
	<-runCtx.Done()

	log.Info("shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Stop scheduler, HTTP and Discord bot in parallel
	done := make(chan struct{})
	go func() {
		application.Services.Scheduler.Stop(ctx)
		application.HTTP.Stop(ctx)
		close(done)
	}()

	if bot != nil {
		bot.Stop()
	}

	// Wait for HTTP to finish
	<-done
	select {
	case <-usbDone:
	case <-ctx.Done():
	}
}
