package xray

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"

	statsService "github.com/xtls/xray-core/app/stats/command"
)

// KillConnections принудительно сбрасывает TCP-соединения пользователя.
// Получает свежие IP из Xray, затем через ss -K отправляет TCP RST.
// Требует iproute2 в контейнере и network_mode: host.
func (c *Client) KillConnections(ctx context.Context, uuid string) int {
	resp, err := c.stats.GetStatsOnlineIpList(ctx, &statsService.GetStatsRequest{
		Name: fmt.Sprintf("user>>>%s>>>online", uuid),
	})
	if err != nil {
		log.Printf("KillConn: failed to get IPs for %s: %v", uuid, err)
		return 0
	}

	ips := resp.GetIps()
	if len(ips) == 0 {
		return 0
	}

	killed := 0
	for ip := range ips {
		cmd := exec.CommandContext(ctx, "ss", "-K", "sport", "=", "443", "dst", ip)
		output, err := cmd.CombinedOutput()
		if err != nil {
			log.Printf("KillConn: ss -K failed for %s: %v (output: %s)",
				ip, err, strings.TrimSpace(string(output)))
			continue
		}
		killed++
		log.Printf("KillConn: killed connections to %s for user %s", ip, uuid)
	}

	return killed
}
