package xray

import (
	"context"
	"fmt"
	"log"
	"os/exec"

	statsService "github.com/xtls/xray-core/app/stats/command"
)

// Firewall убивает активные TCP-соединения заблокированных VPN-пользователей.
//
// Используем ss -K вместо iptables REJECT:
//   - ss -K мгновенно убивает существующие соединения (TCP RST)
//   - НЕ создаёт постоянных правил → не блокирует доступ к веб-панели/лендингу
//   - Новые VPN-подключения не пройдут: RemoveUser удаляет UUID из Xray
type Firewall struct{}

func NewFirewall() *Firewall {
	return &Firewall{}
}

// Init очищает legacy iptables chain VPN_BLOCK, если она осталась от старой версии.
func (f *Firewall) Init() {
	exec.Command("iptables", "-D", "INPUT", "-j", "VPN_BLOCK").Run()
	exec.Command("iptables", "-D", "OUTPUT", "-j", "VPN_BLOCK").Run()
	exec.Command("iptables", "-F", "VPN_BLOCK").Run()
	exec.Command("iptables", "-X", "VPN_BLOCK").Run()

	log.Println("Firewall: initialized (ss -K mode, iptables rules removed)")
}

// BlockUser убивает все активные TCP-соединения пользователя на порту 443.
//
// ВАЖНО: фильтр ss -K использует терминологию local/remote:
//   - dst  = remote peer address (IP юзера)
//   - sport = local port (443 — порт Xray)
//
// RemoveUser вызывается ДО BlockUser и гарантирует, что реконнект не пройдёт.
func (f *Firewall) BlockUser(ctx context.Context, client *Client, uuid string) int {
	resp, err := client.stats.GetStatsOnlineIpList(ctx, &statsService.GetStatsRequest{
		Name: fmt.Sprintf("user>>>%s>>>online", uuid),
	})
	if err != nil {
		log.Printf("Firewall: failed to get IPs for %s: %v", uuid, err)
		return 0
	}

	ips := resp.GetIps()
	if len(ips) == 0 {
		return 0
	}

	killed := 0
	for ip := range ips {
		// dst = remote address (юзер), sport = local port (Xray слушает на 443)
		err := exec.Command("ss", "-K", "dst", ip, "sport", "=", "443").Run()
		if err != nil {
			log.Printf("Firewall: ss -K failed for %s: %v", ip, err)
			continue
		}
		killed++
		log.Printf("Firewall: killed connections from %s for user %s", ip, uuid)
	}

	return killed
}

// UnblockUser — no-op: ss -K не создаёт постоянных правил, чистить нечего.
func (f *Firewall) UnblockUser(uuid string) {}
