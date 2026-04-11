package xray

import "sync"

// Holder — потокобезопасная обёртка для Client.
// Нужна потому что Xray может стартовать позже панели.
type Holder struct {
	mu     sync.RWMutex
	client *Client
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
