package main

import (
	"context"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"xray-panel/internal/config"
	"xray-panel/internal/database"
	"xray-panel/internal/email"
	"xray-panel/internal/handlers"
	"xray-panel/internal/middleware"
	"xray-panel/internal/models"
	"xray-panel/internal/xray"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("Config error: %v", err)
	}

	db, err := database.Connect(cfg.DSN())
	if err != nil {
		log.Fatalf("Database error: %v", err)
	}
	defer db.Close()

	if err := db.Migrate(); err != nil {
		log.Fatalf("Migration error: %v", err)
	}

	userStore := models.NewUserStore(db.Pool)
	profileStore := models.NewVPNProfileStore(db.Pool)

	// Генерируем Xray config.json из БД
	activeUUIDs, err := profileStore.GetAllActiveUUIDs(context.Background())
	if err != nil {
		log.Fatalf("Failed to get active UUIDs: %v", err)
	}

	if err := xray.GenerateConfig(cfg, activeUUIDs, cfg.XrayConfigPath); err != nil {
		log.Printf("Warning: xray config generation failed: %v", err)
	} else {
		log.Printf("Xray config generated with %d active users", len(activeUUIDs))
	}

	xrayHolder := xray.NewHolder()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go connectXray(ctx, cfg, xrayHolder, profileStore)

	// Email sender (nil если SMTP не настроен)
	var mailer *email.Sender
	if cfg.SMTPConfigured() {
		mailer = email.NewSender(cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPUser, cfg.SMTPPassword, cfg.SMTPFrom)
		log.Println("SMTP configured")
	} else {
		log.Println("SMTP not configured — verification links will be logged to console")
	}

	// Шаблоны
	funcMap := template.FuncMap{
		"formatBytes": formatBytes,
		"divGB": func(b int64) string {
			gb := float64(b) / (1024 * 1024 * 1024)
			if gb == 0 {
				return "0"
			}
			return fmt.Sprintf("%.1f", gb)
		},
	}
	renderer, err := handlers.NewRenderer(
		"internal/templates/layouts",
		"internal/templates",
		funcMap,
	)
	if err != nil {
		log.Fatalf("Template error: %v", err)
	}

	authMW := middleware.NewAuthMiddleware(userStore, cfg.SecretKey)
	authHandler := handlers.NewAuthHandler(userStore, authMW, renderer, mailer, cfg.BaseURL)
	dashHandler := handlers.NewDashboardHandler(profileStore, userStore, xrayHolder, cfg, renderer)
	adminHandler := handlers.NewAdminHandler(userStore, profileStore, xrayHolder, renderer)

	// Rate limiter: 5 попыток в минуту на IP
	loginLimiter := middleware.NewRateLimiter(5, time.Minute)

	r := chi.NewRouter()
	r.Use(chimw.RealIP)
	r.Use(chimw.Logger)
	r.Use(chimw.Recoverer)
	r.Use(chimw.Compress(5))

	fs := http.FileServer(http.Dir("static"))
	r.Handle("/static/*", http.StripPrefix("/static/", fs))

	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
	})

	// Публичные роуты
	r.Get("/login", authHandler.LoginPage)
	r.With(loginLimiter.Middleware).Post("/login", authHandler.Login)
	r.Get("/register", authHandler.RegisterPage)
	r.With(loginLimiter.Middleware).Post("/register", authHandler.Register)
	r.Get("/verify", authHandler.VerifyEmail) // GET /verify?token=xxx
	r.Get("/logout", authHandler.Logout)

	// Роуты для залогиненных, но НЕ обязательно верифицированных
	r.Group(func(r chi.Router) {
		r.Use(authMW.RequireAuth)
		r.Get("/verify-pending", authHandler.VerifyPendingPage)
		r.With(loginLimiter.Middleware).Post("/resend-verification", authHandler.ResendVerification)
	})

	// Роуты для залогиненных И верифицированных
	r.Group(func(r chi.Router) {
		r.Use(authMW.RequireAuth)
		r.Use(authMW.RequireVerified)

		r.Get("/dashboard", dashHandler.Index)
		r.Get("/dashboard/stats", dashHandler.StatsJSON)
		r.Post("/dashboard/profiles", dashHandler.CreateProfile)
		r.Post("/dashboard/profiles/{id}/delete", dashHandler.DeleteProfile)

		// Админка
		r.Group(func(r chi.Router) {
			r.Use(authMW.RequireAdmin)
			r.Get("/admin", adminHandler.Users)
			r.Get("/admin/stats", adminHandler.StatsJSON)
			r.Post("/admin/profiles/{id}/toggle", adminHandler.ToggleProfile)
			r.Post("/admin/profiles/{id}/limit", adminHandler.SetLimit)
			r.Post("/admin/profiles/{id}/reset", adminHandler.ResetTraffic)
			r.Post("/admin/users/{id}/balance", adminHandler.AddBalance)
		})
	})

	srv := &http.Server{Addr: cfg.ListenAddr, Handler: r}

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("Shutting down...")
		cancel()
		if c := xrayHolder.Get(); c != nil {
			c.Close()
		}
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		srv.Shutdown(shutdownCtx)
	}()

	log.Printf("Starting server on %s", cfg.ListenAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}

func connectXray(ctx context.Context, cfg *config.Config, holder *xray.Holder, profiles *models.VPNProfileStore) {
	firewall := xray.NewFirewall()
	firewall.Init()
	holder.SetFirewall(firewall)

	for i := 0; i < 30; i++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}

		client, err := xray.NewClient(cfg.XrayAPIAddr, cfg.XrayInboundTag)
		if err != nil {
			log.Printf("Xray gRPC attempt %d: %v", i+1, err)
			continue
		}

		holder.Set(client)
		log.Println("Xray gRPC connected")

		syncUsersToXray(ctx, client, profiles)

		collector := xray.NewStatsCollector(client, profiles, firewall)
		collector.InitCumulative(ctx)
		holder.SetCollector(collector)
		collector.Run(ctx)
		return
	}
	log.Println("Warning: could not connect to Xray gRPC after 30 attempts")
}

func syncUsersToXray(ctx context.Context, client *xray.Client, profiles *models.VPNProfileStore) {
	inactiveUUIDs, err := profiles.GetAllInactiveUUIDs(ctx)
	if err != nil {
		log.Printf("Sync: failed to get inactive UUIDs: %v", err)
	} else {
		removed := 0
		for _, uuid := range inactiveUUIDs {
			if err := client.RemoveUser(ctx, uuid); err == nil {
				removed++
			}
		}
		if removed > 0 {
			log.Printf("Sync: removed %d inactive users from Xray", removed)
		}
	}

	uuids, err := profiles.GetAllActiveUUIDs(ctx)
	if err != nil {
		log.Printf("Sync: failed to get active UUIDs: %v", err)
		return
	}

	added := 0
	for _, uuid := range uuids {
		if err := client.AddUser(ctx, uuid, uuid); err != nil {
			log.Printf("Sync: failed to add %s: %v", uuid, err)
		} else {
			added++
		}
	}
	log.Printf("Sync: %d/%d active users synced to Xray", added, len(uuids))
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %s", float64(b)/float64(div), []string{"KB", "MB", "GB", "TB"}[exp])
}
