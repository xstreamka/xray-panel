package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

type Config struct {
	// Сервер
	ListenAddr string
	SecretKey  string
	BaseURL    string // https://vpn.example.com — для ссылок в письмах

	// TrustedProxies — список CIDR/IP (через запятую), от которых разрешено
	// принимать X-Forwarded-For / X-Real-IP. Иначе любой клиент подделывает
	// заголовок и обходит rate-limit. Если панель за nginx на том же хосте —
	// "127.0.0.1/32,::1/128"; если nginx на отдельной машине — её IP.
	TrustedProxies string

	// PostgreSQL
	DBHost    string
	DBPort    string
	DBUser    string
	DBPass    string
	DBName    string
	DBSSLMode string // disable | require | verify-ca | verify-full | prefer | allow

	// SMTP
	SMTPHost     string
	SMTPPort     string
	SMTPUser     string
	SMTPPassword string
	SMTPFrom     string

	// FeedbackEmail — получатель писем с формы обратной связи.
	// Отправка на SMTPFrom (= self) ненадёжна: Gmail/провайдеры часто
	// отбивают self-loop либо кладут в спам, поэтому выделяем отдельный адрес.
	FeedbackEmail string

	// Xray
	XrayAPIAddr    string // gRPC API адрес (127.0.0.1:10085)
	XrayInboundTag string // тег inbound'а для пользователей
	ServerAddr     string // внешний IP/домен сервера (для генерации VLESS URI)
	ServerPort     string // внешний порт (443)

	// Reality (ЯО inbound)
	RealityDest       string
	RealityServerName string
	RealityPrivateKey string
	RealityPublicKey  string
	RealityShortID    string

	// Амстердам (outbound)
	AmsterdamAddr      string
	AmsterdamPort      int
	AmsterdamUUID      string
	AmsterdamSNI       string
	AmsterdamPublicKey string
	AmsterdamShortID   string

	// Путь к xray config.json (в shared volume)
	XrayConfigPath string

	// MTProto / Telegram proxy (alexbers/mtprotoproxy)
	MTProtoEnabled      bool
	MTProtoServerPort   string
	MTProtoListenAddr   string
	MTProtoListenAddr6  string
	MTProtoConfigPath   string
	MTProtoMetricsAddr  string
	MTProtoMetricsPort  int
	MTProtoContainer    string
	MTProtoDockerSocket string
	MTProtoTLSDomain    string
	MTProtoMaskHost     string
	MTProtoSocks5Host   string
	MTProtoSocks5Port   int
	MTProtoSocks5User   string
	MTProtoSocks5Pass   string

	// Pay Service — интеграция с xstreamka.dev/pay-service
	PayServiceURL string // например https://xstreamka.dev
	WebhookSecret string // общий секрет с pay-service
}

func Load() (*Config, error) {
	_ = godotenv.Load()

	cfg := &Config{
		ListenAddr: getEnv("LISTEN_ADDR", ":8080"),
		SecretKey:  getEnv("SECRET_KEY", ""),
		BaseURL:    getEnv("BASE_URL", "http://localhost:8080"),

		TrustedProxies: getEnv("TRUSTED_PROXIES", ""),

		DBHost:    getEnv("DB_HOST", "127.0.0.1"),
		DBPort:    getEnv("DB_PORT", "5432"),
		DBUser:    getEnv("DB_USER", "vpnpanel"),
		DBPass:    getEnv("DB_PASS", ""),
		DBName:    getEnv("DB_NAME", "vpnpanel"),
		DBSSLMode: getEnv("DB_SSLMODE", ""),

		SMTPHost:     getEnv("SMTP_HOST", ""),
		SMTPPort:     getEnv("SMTP_PORT", "587"),
		SMTPUser:     getEnv("SMTP_USER", ""),
		SMTPPassword: getEnv("SMTP_PASSWORD", ""),
		SMTPFrom:     getEnv("SMTP_FROM", ""),

		FeedbackEmail: getEnv("FEEDBACK_EMAIL", "xstreamka@gmail.com"),

		XrayAPIAddr:    getEnv("XRAY_API_ADDR", "127.0.0.1:10085"),
		XrayInboundTag: getEnv("XRAY_INBOUND_TAG", "vless-in"),
		ServerAddr:     getEnv("SERVER_ADDR", ""),
		ServerPort:     getEnv("SERVER_PORT", "443"),

		RealityDest:       getEnv("REALITY_DEST", "www.google.com:443"),
		RealityServerName: getEnv("REALITY_SERVER_NAME", ""),
		RealityPrivateKey: getEnv("REALITY_PRIVATE_KEY", ""),
		RealityPublicKey:  getEnv("REALITY_PUBLIC_KEY", ""),
		RealityShortID:    getEnv("REALITY_SHORT_ID", ""),

		AmsterdamAddr:      getEnv("AMSTERDAM_ADDR", ""),
		AmsterdamPort:      getEnvInt("AMSTERDAM_PORT", 443),
		AmsterdamUUID:      getEnv("AMSTERDAM_UUID", ""),
		AmsterdamSNI:       getEnv("AMSTERDAM_SNI", ""),
		AmsterdamPublicKey: getEnv("AMSTERDAM_PUBLIC_KEY", ""),
		AmsterdamShortID:   getEnv("AMSTERDAM_SHORT_ID", ""),

		XrayConfigPath: getEnv("XRAY_CONFIG_PATH", "/etc/xray/config.json"),

		PayServiceURL: getEnv("PAY_SERVICE_URL", ""),
		WebhookSecret: getEnv("WEBHOOK_SECRET", ""),
	}

	cfg.MTProtoEnabled = getEnvBool("MTPROTO_ENABLED", false)
	cfg.MTProtoServerPort = getEnv("MTPROTO_SERVER_PORT", "8443")
	cfg.MTProtoListenAddr = getEnv("MTPROTO_LISTEN_ADDR", "0.0.0.0")
	cfg.MTProtoListenAddr6 = getEnv("MTPROTO_LISTEN_ADDR_V6", "::")
	cfg.MTProtoConfigPath = getEnv("MTPROTO_CONFIG_PATH", "/etc/mtprotoproxy/config.py")
	cfg.MTProtoMetricsAddr = getEnv("MTPROTO_METRICS_ADDR", "127.0.0.1:9091")
	cfg.MTProtoMetricsPort = getEnvInt("MTPROTO_METRICS_PORT", 9091)
	cfg.MTProtoContainer = getEnv("MTPROTO_CONTAINER", "xray-panel-mtproto")
	cfg.MTProtoDockerSocket = getEnv("MTPROTO_DOCKER_SOCKET", "/var/run/docker.sock")
	cfg.MTProtoTLSDomain = getEnv("MTPROTO_TLS_DOMAIN", "vk.com")
	cfg.MTProtoMaskHost = getEnv("MTPROTO_MASK_HOST", cfg.MTProtoTLSDomain)
	cfg.MTProtoSocks5Host = getEnv("MTPROTO_SOCKS5_HOST", "")
	cfg.MTProtoSocks5Port = getEnvInt("MTPROTO_SOCKS5_PORT", 0)
	cfg.MTProtoSocks5User = getEnv("MTPROTO_SOCKS5_USER", "")
	cfg.MTProtoSocks5Pass = getEnv("MTPROTO_SOCKS5_PASS", "")

	if cfg.SecretKey == "" {
		return nil, fmt.Errorf("SECRET_KEY is required")
	}
	if cfg.DBPass == "" {
		return nil, fmt.Errorf("DB_PASS is required")
	}
	if cfg.ServerAddr == "" {
		return nil, fmt.Errorf("SERVER_ADDR is required")
	}
	if cfg.AmsterdamAddr == "" {
		return nil, fmt.Errorf("AMSTERDAM_ADDR is required")
	}

	return cfg, nil
}

// DSN собирает URL подключения к Postgres. Юзер и пароль прогоняем через
// url.UserPassword, иначе любой символ, который в URL имеет специальное значение
// (%, @, :, /, пробел и т.п.), ломает парсер pgx с "invalid URL escape".
//
// SSL режим: если задан явно через DB_SSLMODE — используем его. Иначе авто:
// для локальных адресов (127.0.0.1, ::1, localhost) — disable, для удалённых
// БД — require, чтобы пароль не передавался plaintext через сеть.
func (c *Config) DSN() string {
	sslmode := c.DBSSLMode
	if sslmode == "" {
		switch c.DBHost {
		case "127.0.0.1", "::1", "localhost":
			sslmode = "disable"
		default:
			sslmode = "require"
		}
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(c.DBUser, c.DBPass),
		Host:     fmt.Sprintf("%s:%s", c.DBHost, c.DBPort),
		Path:     c.DBName,
		RawQuery: "sslmode=" + sslmode,
	}
	return u.String()
}

// SMTPConfigured возвращает true если SMTP настроен
func (c *Config) SMTPConfigured() bool {
	return c.SMTPHost != "" && c.SMTPUser != "" && c.SMTPPassword != ""
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func getEnvBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}
