package xray

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"sync"

	statsService "github.com/xtls/xray-core/app/stats/command"
)

const iptablesChain = "VPN_BLOCK"

// Firewall управляет iptables-правилами для блокировки TCP-соединений.
// Правила НЕ удаляются автоматически — только при реактивации пользователя.
type Firewall struct {
	mu         sync.Mutex
	blockedIPs map[string][]string // uuid → []ip
	inited     bool
}

func NewFirewall() *Firewall {
	return &Firewall{
		blockedIPs: make(map[string][]string),
	}
}

// Init создаёт отдельную iptables chain и подключает к INPUT/OUTPUT.
// Вызывать один раз при старте.
func (f *Firewall) Init() {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.inited {
		return
	}

	// Создаём chain (игнорируем ошибку если уже есть)
	exec.Command("iptables", "-N", iptablesChain).Run()

	// Очищаем chain от старых правил (после перезапуска панели)
	exec.Command("iptables", "-F", iptablesChain).Run()

	// Подключаем chain к INPUT и OUTPUT (если ещё не подключена)
	// -C проверяет, -A добавляет
	if exec.Command("iptables", "-C", "INPUT", "-j", iptablesChain).Run() != nil {
		exec.Command("iptables", "-I", "INPUT", "-j", iptablesChain).Run()
	}
	if exec.Command("iptables", "-C", "OUTPUT", "-j", iptablesChain).Run() != nil {
		exec.Command("iptables", "-I", "OUTPUT", "-j", iptablesChain).Run()
	}

	f.inited = true
	log.Printf("Firewall: chain %s initialized", iptablesChain)
}

// BlockUser добавляет REJECT-правила для всех IP пользователя.
// TCP RST мгновенно обрывает соединения.
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

	f.mu.Lock()
	defer f.mu.Unlock()

	blocked := 0
	var blockedList []string

	for ip := range ips {
		// Блокируем входящие (клиент → сервер:443)
		inErr := exec.Command("iptables", "-A", iptablesChain,
			"-s", ip, "-p", "tcp", "--dport", "443",
			"-j", "REJECT", "--reject-with", "tcp-reset").Run()

		// Блокируем исходящие (сервер:443 → клиент)
		outErr := exec.Command("iptables", "-A", iptablesChain,
			"-d", ip, "-p", "tcp", "--sport", "443",
			"-j", "REJECT", "--reject-with", "tcp-reset").Run()

		if inErr == nil && outErr == nil {
			blocked++
			blockedList = append(blockedList, ip)
			log.Printf("Firewall: blocked %s for user %s", ip, uuid)
		}
	}

	if len(blockedList) > 0 {
		f.blockedIPs[uuid] = blockedList
	}

	return blocked
}

// UnblockUser удаляет все REJECT-правила для пользователя.
// Вызывать при реактивации (admin toggle, traffic reset).
func (f *Firewall) UnblockUser(uuid string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	ips, ok := f.blockedIPs[uuid]
	if !ok {
		return
	}

	for _, ip := range ips {
		exec.Command("iptables", "-D", iptablesChain,
			"-s", ip, "-p", "tcp", "--dport", "443",
			"-j", "REJECT", "--reject-with", "tcp-reset").Run()

		exec.Command("iptables", "-D", iptablesChain,
			"-d", ip, "-p", "tcp", "--sport", "443",
			"-j", "REJECT", "--reject-with", "tcp-reset").Run()

		log.Printf("Firewall: unblocked %s for user %s", ip, uuid)
	}

	delete(f.blockedIPs, uuid)
}
