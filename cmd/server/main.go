package main

import (
	"context"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"xray-panel/internal/config"
	"xray-panel/internal/database"
	"xray-panel/internal/email"
	"xray-panel/internal/handlers"
	"xray-panel/internal/middleware"
	"xray-panel/internal/models"
	"xray-panel/internal/subscription"
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
	tariffStore := models.NewTariffStore(db.Pool)
	receiptStore := models.NewPaymentReceiptStore(db.Pool)
	inviteStore := models.NewInviteStore(db.Pool)
	trafficLogStore := models.NewTrafficLogStore(db.Pool)

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

	// Email sender (nil если SMTP не настроен)
	var mailer *email.Sender
	if cfg.SMTPConfigured() {
		mailer = email.NewSender(cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPUser, cfg.SMTPPassword, cfg.SMTPFrom)
		log.Println("SMTP configured")
	} else {
		log.Println("SMTP not configured — verification links will be logged to console")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go connectXray(ctx, cfg, xrayHolder, profileStore, userStore, trafficLogStore, mailer, cfg.BaseURL)

	subWorker := subscription.NewWorker(userStore, profileStore, mailer, xrayHolder, cfg.BaseURL)
	go subWorker.Run(ctx)

	// Шаблоны
	// assetVersion — busts browser cache для css/js при каждом рестарте панели,
	// чтобы клиенты гарантированно получали новый бандл после деплоя.
	assetVersion := strconv.FormatInt(time.Now().Unix(), 10)
	funcMap := template.FuncMap{
		"formatBytes": formatBytes,
		"divGB": func(b int64) string {
			gb := float64(b) / (1024 * 1024 * 1024)
			if gb == 0 {
				return "0"
			}
			return fmt.Sprintf("%.1f", gb)
		},
		"assetVersion": func() string { return assetVersion },
	}
	renderer, err := handlers.NewRenderer(
		"internal/templates/layouts",
		"internal/templates",
		funcMap,
	)
	if err != nil {
		log.Fatalf("Template error: %v", err)
	}

	authMW := middleware.NewAuthMiddleware(userStore, cfg.SecretKey, cfg.BaseURL)
	// Лимит на запросы восстановления пароля: 3 в час с одного IP.
	// Значение продублировано константой handlers.ResetLimitMax для UI-сноски.
	resetLimiter := middleware.NewRateLimiter(handlers.ResetLimitMax, time.Hour)
	authHandler := handlers.NewAuthHandler(userStore, inviteStore, authMW, renderer, mailer, cfg.BaseURL, resetLimiter)
	dashHandler := handlers.NewDashboardHandler(profileStore, userStore, tariffStore, trafficLogStore, xrayHolder, cfg, renderer)
	adminHandler := handlers.NewAdminHandler(userStore, profileStore, tariffStore, inviteStore, trafficLogStore, xrayHolder, renderer, cfg.BaseURL)
	settingsHandler := handlers.NewSettingsHandler(userStore, renderer)
	// Лимит обратной связи: 3 сообщения в час с одного IP — чтобы форму
	// нельзя было использовать для спам-флуда на админский ящик.
	feedbackLimiter := middleware.NewRateLimiter(3, time.Hour)
	feedbackHandler := handlers.NewFeedbackHandler(renderer, mailer, cfg.FeedbackEmail, feedbackLimiter)
	payHandler := handlers.NewPayHandler(
		renderer, tariffStore, receiptStore, userStore, mailer, xrayHolder,
		cfg.PayServiceURL, cfg.BaseURL, cfg.WebhookSecret,
	)

	// Rate limiter: 5 попыток в минуту на IP
	loginLimiter := middleware.NewRateLimiter(5, time.Minute)

	r := chi.NewRouter()
	r.Use(chimw.RealIP)
	r.Use(chimw.Logger)
	r.Use(chimw.Recoverer)
	r.Use(chimw.Compress(5))

	fs := http.FileServer(http.Dir("static"))
	r.Handle("/static/*", http.StripPrefix("/static/", fs))

	// Публичные роуты
	r.Get("/login", authHandler.LoginPage)
	r.With(loginLimiter.Middleware).Post("/login", authHandler.Login)
	r.Get("/register", authHandler.RegisterPage)
	r.With(loginLimiter.Middleware).Post("/register", authHandler.Register)
	r.Get("/verify", authHandler.VerifyEmail) // GET /verify?token=xxx
	r.Get("/logout", authHandler.Logout)
	// Восстановление пароля. Rate-limit (3/час) выполняется внутри хендлера,
	// чтобы рендерить дружелюбную страницу вместо голого 429.
	r.Get("/forgot", authHandler.ForgotPasswordPage)
	r.Post("/forgot", authHandler.ForgotPassword)
	r.Get("/reset", authHandler.ResetPasswordPage) // GET /reset?token=xxx
	r.Post("/reset", authHandler.ResetPassword)
	// Webhook от pay-service (защищён HMAC-подписью, не сессией)
	r.Post("/api/payments/webhook", payHandler.Webhook)

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

		r.Get("/", dashHandler.Welcome)
		r.Get("/dashboard", dashHandler.Index)
		r.Get("/dashboard/stats", dashHandler.StatsJSON)
		r.Get("/dashboard/traffic", dashHandler.TrafficChart)
		r.Get("/dashboard/profiles/{id}/traffic", dashHandler.ProfileTrafficChart)
		r.Post("/dashboard/profiles", dashHandler.CreateProfile)
		r.Post("/dashboard/profiles/{id}/limit", dashHandler.SetProfileLimit)
		r.Post("/dashboard/profiles/{id}/toggle", dashHandler.ToggleProfile)
		r.Post("/dashboard/profiles/{id}/reset", dashHandler.ResetProfileTraffic)
		r.Post("/dashboard/profiles/{id}/delete", dashHandler.DeleteProfile)

		// Оплата (редирект на pay-service)
		r.Get("/pay", payHandler.Index)
		r.Post("/pay/checkout", payHandler.Checkout)
		r.Get("/pay/history", payHandler.History)

		// Настройки уведомлений
		r.Get("/settings", settingsHandler.Index)
		r.Post("/settings", settingsHandler.Save)

		// Обратная связь
		r.Get("/feedback", feedbackHandler.Index)
		r.Post("/feedback", feedbackHandler.Send)

		// Админка
		r.Group(func(r chi.Router) {
			r.Use(authMW.RequireAdmin)
			r.Get("/admin", adminHandler.Users)
			r.Get("/admin/stats", adminHandler.StatsJSON)
			r.Get("/admin/users/{id}", adminHandler.UserView)
			r.Get("/admin/users/{id}/traffic", adminHandler.UserTrafficChart)
			r.Post("/admin/profiles/{id}/toggle", adminHandler.ToggleProfile)
			r.Post("/admin/profiles/{id}/limit", adminHandler.SetLimit)
			r.Post("/admin/profiles/{id}/reset", adminHandler.ResetTraffic)
			r.Post("/admin/users/{id}/extra", adminHandler.SetExtraBalance)
			r.Post("/admin/users/{id}/toggle", adminHandler.ToggleUserActive)
			r.Post("/admin/users/{id}/subscription", adminHandler.SetSubscription)
			r.Post("/admin/users/{id}/subscription/cancel", adminHandler.CancelSubscription)

			// Тарифы
			r.Get("/admin/tariffs", adminHandler.TariffsList)
			r.Post("/admin/tariffs", adminHandler.TariffCreate)
			r.Post("/admin/tariffs/{id}", adminHandler.TariffUpdate)
			r.Post("/admin/tariffs/{id}/delete", adminHandler.TariffDelete)

			// Инвайты + режим регистрации
			r.Get("/admin/invites", adminHandler.InvitesList)
			r.Post("/admin/invites", adminHandler.InviteCreate)
			r.Post("/admin/invites/{id}/toggle", adminHandler.InviteToggle)
			r.Post("/admin/invites/{id}/delete", adminHandler.InviteDelete)
			r.Get("/admin/invites/{id}/users", adminHandler.InviteUsers)
			r.Post("/admin/settings/registration-mode", adminHandler.SetRegistrationMode)
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

func connectXray(
	ctx context.Context,
	cfg *config.Config,
	holder *xray.Holder,
	profiles *models.VPNProfileStore,
	users *models.UserStore,
	trafficLogs *models.TrafficLogStore,
	mailer *email.Sender,
	baseURL string,
) {
	firewall := xray.NewFirewall(cfg.ServerPort)
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

		collector := xray.NewStatsCollector(client, profiles, users, trafficLogs, firewall, mailer, baseURL)
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
