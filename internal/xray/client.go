package xray

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	handlerService "github.com/xtls/xray-core/app/proxyman/command"
	statsService "github.com/xtls/xray-core/app/stats/command"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/proxy/vless"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type Client struct {
	conn    *grpc.ClientConn
	handler handlerService.HandlerServiceClient
	stats   statsService.StatsServiceClient
	tag     string // inbound tag
}

func NewClient(addr string, inboundTag string) (*Client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx, addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		return nil, fmt.Errorf("grpc dial %s: %w", addr, err)
	}

	return &Client{
		conn:    conn,
		handler: handlerService.NewHandlerServiceClient(conn),
		stats:   statsService.NewStatsServiceClient(conn),
		tag:     inboundTag,
	}, nil
}

func (c *Client) Close() {
	if c.conn != nil {
		c.conn.Close()
	}
}

// AddUser добавляет пользователя в Xray inbound (VLESS)
func (c *Client) AddUser(ctx context.Context, uuid string, email string) error {
	account := serial.ToTypedMessage(&vless.Account{
		Id:   uuid,
		Flow: "xtls-rprx-vision",
	})

	resp, err := c.handler.AlterInbound(ctx, &handlerService.AlterInboundRequest{
		Tag: c.tag,
		Operation: serial.ToTypedMessage(&handlerService.AddUserOperation{
			User: &protocol.User{
				Level:   0,
				Email:   email,
				Account: account,
			},
		}),
	})
	if err != nil {
		return fmt.Errorf("add user %s: %w", email, err)
	}

	log.Printf("Xray: user added: %s (resp: %v)", email, resp)
	return nil
}

// RemoveUser удаляет пользователя из Xray inbound
func (c *Client) RemoveUser(ctx context.Context, email string) error {
	_, err := c.handler.AlterInbound(ctx, &handlerService.AlterInboundRequest{
		Tag: c.tag,
		Operation: serial.ToTypedMessage(&handlerService.RemoveUserOperation{
			Email: email,
		}),
	})
	if err != nil {
		return fmt.Errorf("remove user %s: %w", email, err)
	}

	log.Printf("Xray: user removed: %s", email)
	return nil
}

// GetUserTraffic получает статистику трафика пользователя
// Xray хранит стату по email: user>>>email>>>traffic>>>uplink / downlink
func (c *Client) GetUserTraffic(ctx context.Context, email string, reset bool) (up, down int64, err error) {
	upResp, err := c.stats.GetStats(ctx, &statsService.GetStatsRequest{
		Name:   fmt.Sprintf("user>>>%s>>>traffic>>>uplink", email),
		Reset_: reset,
	})
	if err != nil {
		// Если стата ещё не появилась — не ошибка
		up = 0
	} else {
		up = upResp.GetStat().GetValue()
	}

	downResp, err := c.stats.GetStats(ctx, &statsService.GetStatsRequest{
		Name:   fmt.Sprintf("user>>>%s>>>traffic>>>downlink", email),
		Reset_: reset,
	})
	if err != nil {
		down = 0
	} else {
		down = downResp.GetStat().GetValue()
	}

	return up, down, nil
}

// GetOnlineUsers возвращает множество email-ов онлайн-пользователей.
// Сначала пробует GetAllOnlineUsers API (Xray online tracking).
// Если API недоступен или возвращает пустой результат — определяет по наличию
// ненулевого трафика (можно передать уже полученный traffic, чтобы не дёргать API дважды).
func (c *Client) GetOnlineUsers(ctx context.Context, liveTraffic map[string][2]int64) (map[string]bool, error) {
	// 1. Пробуем native online tracking API
	resp, err := c.stats.GetAllOnlineUsers(ctx, &statsService.GetAllOnlineUsersRequest{})
	if err == nil && len(resp.GetUsers()) > 0 {
		online := make(map[string]bool, len(resp.GetUsers()))
		for _, raw := range resp.GetUsers() {
			// API возвращает "user>>>email>>>online" — извлекаем email
			email := raw
			if parts := strings.SplitN(raw, ">>>", 3); len(parts) >= 2 {
				email = parts[1]
			}
			online[email] = true
		}
		return online, nil
	}

	// 2. Fallback: определяем по ненулевому трафику в текущем интервале
	if liveTraffic == nil {
		liveTraffic, err = c.QueryAllUserTraffic(ctx, false)
		if err != nil {
			return nil, err
		}
	}
	online := make(map[string]bool, len(liveTraffic))
	for email, stats := range liveTraffic {
		if stats[0] > 0 || stats[1] > 0 {
			online[email] = true
		}
	}
	return online, nil
}

// GetOnlineIPCounts возвращает количество уникальных IP (устройств) на каждого онлайн-пользователя.
// Если Xray поддерживает online tracking — использует GetStatsOnline для точного подсчёта.
// Иначе ставит 1 для каждого онлайн-пользователя.
func (c *Client) GetOnlineIPCounts(ctx context.Context, onlineUsers map[string]bool) (map[string]int, error) {

	counts := make(map[string]int, len(onlineUsers))
	for email := range onlineUsers {
		resp, err := c.stats.GetStatsOnline(ctx, &statsService.GetStatsRequest{
			Name: fmt.Sprintf("user>>>%s>>>online", email),
		})
		if err != nil {
			counts[email] = 1
			continue
		}
		count := int(resp.GetStat().GetValue())
		if count < 1 {
			count = 1
		}
		counts[email] = count
	}
	return counts, nil
}

// QueryAllUserTraffic получает стату по всем юзерам разом
func (c *Client) QueryAllUserTraffic(ctx context.Context, reset bool) (map[string][2]int64, error) {
	resp, err := c.stats.QueryStats(ctx, &statsService.QueryStatsRequest{
		Pattern: "user>>>",
		Reset_:  reset,
	})
	if err != nil {
		return nil, fmt.Errorf("query stats: %w", err)
	}

	// Парсим: user>>>email>>>traffic>>>uplink|downlink
	result := make(map[string][2]int64)
	for _, stat := range resp.GetStat() {
		// stat.Name = "user>>>uuid>>>traffic>>>uplink" или "...>>>downlink"
		parts := strings.Split(stat.GetName(), ">>>")
		if len(parts) != 4 {
			continue
		}
		email := parts[1]
		direction := parts[3]

		entry := result[email]
		switch direction {
		case "uplink":
			entry[0] = stat.GetValue()
		case "downlink":
			entry[1] = stat.GetValue()
		}
		result[email] = entry
	}

	return result, nil
}
