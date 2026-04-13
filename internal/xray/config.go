package xray

import (
	"encoding/json"
	"fmt"
	"os"

	"xray-panel/internal/config"
)

// GenerateConfig создаёт config.json для Xray с включённым API, stats и policy
func GenerateConfig(cfg *config.Config, activeUUIDs []string, outputPath string) error {
	// Собираем clients из активных UUID
	clients := make([]map[string]any, 0, len(activeUUIDs))
	for _, uuid := range activeUUIDs {
		clients = append(clients, map[string]any{
			"id":    uuid,
			"email": uuid, // используем UUID как email для статистики
			"flow":  "xtls-rprx-vision",
		})
	}

	xrayConfig := map[string]any{
		// API для gRPC управления
		"api": map[string]any{
			"tag": "api",
			"services": []string{
				"HandlerService",
				"StatsService",
			},
		},

		// Статистика
		"stats": map[string]any{},

		// Policy — включаем статистику для пользователей
		"policy": map[string]any{
			"levels": map[string]any{
				"0": map[string]any{
					"statsUserUplink":   true,
					"statsUserDownlink": true,
					"statsUserOnline":   true,
					"bufferSize":        0, // отключаем внутреннюю буферизацию для мгновенного обновления счётчиков
				},
			},
			"system": map[string]any{
				"statsInboundUplink":    true,
				"statsInboundDownlink":  true,
				"statsOutboundUplink":   true,
				"statsOutboundDownlink": true,
			},
		},

		// Inbounds
		"inbounds": []map[string]any{
			// API inbound (только локальный)
			{
				"tag":      "api-in",
				"listen":   "127.0.0.1",
				"port":     10085,
				"protocol": "dokodemo-door",
				"settings": map[string]any{
					"address": "127.0.0.1",
				},
			},
			// VLESS Reality inbound (для пользователей)
			{
				"tag":      cfg.XrayInboundTag,
				"listen":   "0.0.0.0",
				"port":     443,
				"protocol": "vless",
				"settings": map[string]any{
					"clients":    clients,
					"decryption": "none",
				},
				"streamSettings": map[string]any{
					"network":  "tcp",
					"security": "reality",
					"realitySettings": map[string]any{
						"show":        false,
						"dest":        cfg.RealityDest,
						"xver":        0,
						"serverNames": []string{cfg.RealityServerName},
						"privateKey":  cfg.RealityPrivateKey,
						"shortIds":    []string{cfg.RealityShortID},
					},
				},
				"sniffing": map[string]any{
					"enabled":      true,
					"destOverride": []string{"http", "tls"},
				},
			},
		},

		// Outbounds
		"outbounds": []map[string]any{
			// Основной outbound — на Амстердам через Xray
			{
				"tag":      "proxy-out",
				"protocol": "vless",
				"settings": map[string]any{
					"vnext": []map[string]any{
						{
							"address": cfg.AmsterdamAddr,
							"port":    cfg.AmsterdamPort,
							"users": []map[string]any{
								{
									"id":         cfg.AmsterdamUUID,
									"encryption": "none",
									"flow":       "xtls-rprx-vision",
								},
							},
						},
					},
				},
				"streamSettings": map[string]any{
					"network":  "tcp",
					"security": "reality",
					"realitySettings": map[string]any{
						"serverName":  cfg.AmsterdamSNI,
						"fingerprint": "chrome",
						"publicKey":   cfg.AmsterdamPublicKey,
						"shortId":     cfg.AmsterdamShortID,
					},
				},
			},
			// Freedom для API
			{
				"tag":      "direct",
				"protocol": "freedom",
			},
		},

		// Routing
		"routing": map[string]any{
			"rules": []map[string]any{
				// API трафик → на API outbound
				{
					"type":        "field",
					"inboundTag":  []string{"api-in"},
					"outboundTag": "api",
				},
				// Весь пользовательский трафик → Амстердам
				{
					"type":        "field",
					"inboundTag":  []string{cfg.XrayInboundTag},
					"outboundTag": "proxy-out",
				},
			},
		},
	}

	data, err := json.MarshalIndent(xrayConfig, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	if err := os.WriteFile(outputPath, data, 0644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	return nil
}
