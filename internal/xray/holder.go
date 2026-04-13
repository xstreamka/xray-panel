package xray

import "sync"

// Holder — потокобезопасная обёртка для Client, StatsCollector и Firewall.
type Holder struct {
	mu        sync.RWMutex
	client    *Client
	collector *StatsCollector
	firewall  *Firewall
}

func NewHolder() *Holder {
	return &Holder{}
}

func (h *Holder) Set(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.client = c
}

func (h *Holder) Get() *Client {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.client
}

func (h *Holder) SetCollector(c *StatsCollector) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.collector = c
}

func (h *Holder) GetCollector() *StatsCollector {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.collector
}

func (h *Holder) SetFirewall(f *Firewall) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.firewall = f
}

func (h *Holder) GetFirewall() *Firewall {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.firewall
}
