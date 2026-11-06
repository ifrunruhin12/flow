package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ifrunruhin12/flow/backend/internal/api"
	"github.com/ifrunruhin12/flow/backend/internal/auth"
	"github.com/ifrunruhin12/flow/backend/internal/bootstrap"
	"github.com/ifrunruhin12/flow/backend/internal/config"
	"github.com/ifrunruhin12/flow/backend/internal/credentials"
	"github.com/ifrunruhin12/flow/backend/internal/database"
	"github.com/ifrunruhin12/flow/backend/internal/httpserver"
	"github.com/ifrunruhin12/flow/backend/internal/library"
	"github.com/ifrunruhin12/flow/backend/internal/membership"
	"github.com/ifrunruhin12/flow/backend/internal/navidrome"
	"github.com/ifrunruhin12/flow/backend/internal/playback"
	"github.com/ifrunruhin12/flow/backend/internal/playlist"
	"github.com/ifrunruhin12/flow/backend/internal/streaming"
	"github.com/ifrunruhin12/flow/backend/migrations"
	"github.com/jackc/pgx/v5"
)

func main() {
	if err := run(); err != nil {
		slog.Error("flow stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)
	keyring, err := credentials.Load(cfg.CredentialKeyringFile)
	if err != nil {
		return err
	}

	rootContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	databaseContext, cancelDatabase := context.WithTimeout(rootContext, 10*time.Second)
	pool, err := database.Open(databaseContext, cfg.DatabaseURL, cfg.DatabaseMaxConns)
	cancelDatabase()
	if err != nil {
		return err
	}
	defer pool.Close()

	migrationContext, cancelMigrations := context.WithTimeout(rootContext, cfg.MigrationTimeout)
	err = database.MigrateUp(migrationContext, pool, migrations.Files)
	cancelMigrations()
	if err != nil {
		return err
	}
	bootstrapService := bootstrap.New(pool, keyring)
	if err := bootstrapService.PrepareKeyVersions(rootContext); err != nil {
		return fmt.Errorf("prepare credential key versions: %w", err)
	}
	if err := bootstrapService.ReencryptCredentials(rootContext); err != nil {
		return fmt.Errorf("re-encrypt stored credentials: %w", err)
	}

	navidromeClient, err := navidrome.NewWithTimeouts(cfg.NavidromeURL, cfg.NavidromeTimeout, cfg.NavidromeStreamHeaderTimeout)
	if err != nil {
		return err
	}
	integration := &httpserver.IntegrationStatus{}
	remoteReady := false
	if cfg.NavidromeCredentialsConfigured() {
		verifyContext, cancelVerify := context.WithTimeout(rootContext, cfg.NavidromeTimeout*4)
		verification, verifyErr := navidromeClient.VerifyAdmin(verifyContext, cfg.NavidromeAdminUsername, cfg.NavidromeAdminPassword)
		cancelVerify()
		if verifyErr != nil {
			integration.Set("degraded", "administrator verification failed")
			logger.Warn("navidrome integration degraded", "error", verifyErr)
		} else {
			bootstrapContext, cancelBootstrap := context.WithTimeout(rootContext, cfg.ProvisionTimeout)
			_, bootstrapErr := bootstrapService.EnsureAdmin(bootstrapContext, cfg.FlowAdminUsername, cfg.FlowAdminPassword, cfg.NavidromeAdminPassword, verification, bootstrap.CorrelationID())
			cancelBootstrap()
			if bootstrapErr != nil {
				return fmt.Errorf("bootstrap Flow administrator: %w", bootstrapErr)
			}
			integration.Set("verified", "administrator and security gates verified")
			remoteReady = true
			logger.Info("navidrome integration verified")
		}
	} else {
		integration.Set("degraded", "administrator credentials not configured")
		logger.Warn("navidrome integration degraded", "reason", "administrator credentials not configured")
	}
	authService, err := auth.NewService(pool, cfg.SessionTTL)
	if err != nil {
		return fmt.Errorf("initialize authentication: %w", err)
	}
	var adminCredential navidrome.AdminCredential
	adminUsername, adminPassword, credentialErr := bootstrapService.DecryptAdminCredential(rootContext)
	if credentialErr == nil {
		adminCredential = navidrome.AdminCredential{Username: adminUsername, Password: adminPassword}
	} else if !errors.Is(credentialErr, pgx.ErrNoRows) {
		return fmt.Errorf("load stored Navidrome administrator credential: %w", credentialErr)
	}
	membershipService := membership.New(pool, keyring, navidromeClient, adminCredential, remoteReady, cfg.ProvisionTimeout)
	credentialCache := navidrome.NewCredentialCache(membershipService, navidromeClient)
	navidromeAdapter := navidrome.NewAdapter(navidromeClient, credentialCache)
	streamRegistry := streaming.NewRegistry(100)
	lifecycle := &streaming.Lifecycle{Registry: streamRegistry, Credentials: credentialCache}
	authService.SetStreamRevoker(streamRegistry)
	membershipService.SetLifecycle(lifecycle)
	libraryService := library.New(navidromeAdapter)
	playlistService := playlist.New(navidromeAdapter)
	streamingGateway := streaming.NewGateway(authService, streamRegistry, navidromeAdapter)
	playbackService := playback.New(playback.NewPostgresStore(pool), libraryService)
	apiServer := api.New(logger, authService, membershipService, cfg.PublicOrigin, cfg.CookieSecure, cfg.SessionTTL, api.Services{
		Library: libraryService, Playlists: playlistService, Streaming: streamingGateway,
		Playback: playbackService,
	})

	application := httpserver.New(logger, pool, navidromeClient, integration, apiServer.Handler())
	httpServer := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           application.Handler(),
		ReadHeaderTimeout: cfg.HTTPReadHeaderTimeout,
		IdleTimeout:       cfg.HTTPIdleTimeout,
	}
	listener, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.HTTPAddr, err)
	}
	defer listener.Close()

	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("flow backend listening", "address", listener.Addr().String())
		serverErrors <- httpServer.Serve(listener)
	}()
	application.SetReady(true)

	select {
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-rootContext.Done():
		application.SetReady(false)
		streamRegistry.AbortAll()
		shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancelShutdown()
		if err := httpServer.Shutdown(shutdownContext); err != nil {
			_ = httpServer.Close()
			return err
		}
		logger.Info("flow backend stopped cleanly")
		return nil
	}
}
