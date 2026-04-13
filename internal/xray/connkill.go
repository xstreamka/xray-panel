package xray

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"time"

	statsService "github.com/xtls/xray-core/app/stats/command"
)

// KillConnections принудительно обрывает TCP-соединения пользователя.
// Использует iptables REJECT с tcp-reset — ядро отправляет RST на каждый пакет.
// Правило автоматически удаляется через 5 секунд.
// Требует iptables в контейнере и network_mode: host.
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
		if blockAndRelease(ip) {
			killed++
			log.Printf("KillConn: blocked %s for user %s", ip, uuid)
		}
	}

	return killed
}

// blockAndRelease добавляет iptables REJECT правило для IP,
// которое отправляет TCP RST на все пакеты от/к этому IP на порт 443.
// Правило удаляется через 5 секунд в фоне.
func blockAndRelease(ip string) bool {
	// INPUT: пакеты ОТ клиента → сервер:443
	inArgs := []string{"-I", "INPUT", "-s", ip, "-p", "tcp", "--dport", "443",
		"-j", "REJECT", "--reject-with", "tcp-reset"}

	// OUTPUT: пакеты сервер:443 → клиенту (ответы)
	outArgs := []string{"-I", "OUTPUT", "-d", ip, "-p", "tcp", "--sport", "443",
		"-j", "REJECT", "--reject-with", "tcp-reset"}

	if err := exec.Command("iptables", inArgs...).Run(); err != nil {
		log.Printf("KillConn: iptables INPUT failed for %s: %v", ip, err)
		return false
	}

	if err := exec.Command("iptables", outArgs...).Run(); err != nil {
		log.Printf("KillConn: iptables OUTPUT failed for %s: %v", ip, err)
		// Откатываем INPUT правило
		delArgs := make([]string, len(inArgs))
		copy(delArgs, inArgs)
		delArgs[0] = "-D"
		exec.Command("iptables", delArgs...).Run()
		return false
	}

	// Удаляем правила через 5 секунд (юзер уже удалён из Xray, реконнект невозможен)
	go func() {
		time.Sleep(5 * time.Second)

		delIn := make([]string, len(inArgs))
		copy(delIn, inArgs)
		delIn[0] = "-D"
		exec.Command("iptables", delIn...).Run()

		delOut := make([]string, len(outArgs))
		copy(delOut, outArgs)
		delOut[0] = "-D"
		exec.Command("iptables", delOut...).Run()

		log.Printf("KillConn: unblocked %s", ip)
	}()

	return true
}
