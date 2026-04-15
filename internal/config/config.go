package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

type Config struct {
	// Сервер
	ListenAddr string
	SecretKey  string
	BaseURL    string // https://vpn.example.com — для ссылок в письмах

	// PostgreSQL
	DBHost string
	DBPort string
	DBUser string
	DBPass string
	DBName string

	// SMTP
	SMTPHost     string
	SMTPPort     string
	SMTPUser     string
	SMTPPassword string
	SMTPFrom     string

	// Xray
	XrayAPIAddr    string // gRPC API адрес (127.0.0.1:10085)
	XrayInboundTag string // тег inbound'а для пользователей
	ServerAddr     string // внешний IP/домен сервера (для генерации VLESS URI)
	ServerPort     string // внешний порт (443)

	// Reality (ЯО inbound)
	RealityDest        string
	RealityServerNames []string
	RealityPrivateKey  string
	RealityPublicKey   string
	RealityShortID     string

	// Амстердам (outbound)
	AmsterdamAddr      string
	AmsterdamPort      int
	AmsterdamUUID      string
	AmsterdamSNI       string
	AmsterdamPublicKey string
	AmsterdamShortID   string

	// Путь к xray config.json (в shared volume)
	XrayConfigPath string
}

func Load() (*Config, error) {
	_ = godotenv.Load()

	cfg := &Config{
		ListenAddr: getEnv("LISTEN_ADDR", ":8080"),
		SecretKey:  getEnv("SECRET_KEY", ""),
		BaseURL:    getEnv("BASE_URL", "http://localhost:8080"),

		DBHost: getEnv("DB_HOST", "127.0.0.1"),
		DBPort: getEnv("DB_PORT", "5432"),
		DBUser: getEnv("DB_USER", "vpnpanel"),
		DBPass: getEnv("DB_PASS", ""),
		DBName: getEnv("DB_NAME", "vpnpanel"),

		SMTPHost:     getEnv("SMTP_HOST", ""),
		SMTPPort:     getEnv("SMTP_PORT", "587"),
		SMTPUser:     getEnv("SMTP_USER", ""),
		SMTPPassword: getEnv("SMTP_PASSWORD", ""),
		SMTPFrom:     getEnv("SMTP_FROM", ""),

		XrayAPIAddr:    getEnv("XRAY_API_ADDR", "127.0.0.1:10085"),
		XrayInboundTag: getEnv("XRAY_INBOUND_TAG", "vless-in"),
		ServerAddr:     getEnv("SERVER_ADDR", ""),
		ServerPort:     getEnv("SERVER_PORT", "443"),

		RealityDest:        getEnv("REALITY_DEST", "www.google.com:443"),
		RealityServerNames: splitCSV(getEnv("REALITY_SERVER_NAME", "xstreamka.dev")),
		RealityPrivateKey:  getEnv("REALITY_PRIVATE_KEY", ""),
		RealityPublicKey:   getEnv("REALITY_PUBLIC_KEY", ""),
		RealityShortID:     getEnv("REALITY_SHORT_ID", ""),

		AmsterdamAddr:      getEnv("AMSTERDAM_ADDR", ""),
		AmsterdamPort:      getEnvInt("AMSTERDAM_PORT", 443),
		AmsterdamUUID:      getEnv("AMSTERDAM_UUID", ""),
		AmsterdamSNI:       getEnv("AMSTERDAM_SNI", ""),
		AmsterdamPublicKey: getEnv("AMSTERDAM_PUBLIC_KEY", ""),
		AmsterdamShortID:   getEnv("AMSTERDAM_SHORT_ID", ""),

		XrayConfigPath: getEnv("XRAY_CONFIG_PATH", "/etc/xray/config.json"),
	}

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

func (c *Config) DSN() string {
	return fmt.Sprintf(
		"postgres://%s:%s@%s:%s/%s?sslmode=disable",
		c.DBUser, c.DBPass, c.DBHost, c.DBPort, c.DBName,
	)
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

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}
