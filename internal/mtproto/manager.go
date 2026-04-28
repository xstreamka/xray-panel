package mtproto

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"xray-panel/internal/config"
	"xray-panel/internal/email"
	"xray-panel/internal/models"
)

type UserDisconnectFunc func(ctx context.Context, userID int, reason string)

type Manager struct {
	cfg         *config.Config
	profiles    *models.MTProtoProfileStore
	users       *models.UserStore
	trafficLogs *models.MTProtoTrafficLogStore
	mailer      *email.Sender
	baseURL     string
	httpClient  *http.Client

	mu              sync.RWMutex
	lastCounters    map[int][2]int64
	counterSeen     map[int]bool
	liveTraffic     map[int][2]int64
	online          map[int]bool
	currentConns    map[int]int64
	cumulative      map[int]int64
	limits          map[int]int64
	profileToUser   map[int]int
	disabledUsers   map[int]bool
	trafficBuffer   map[int][2]int64
	lastFullEnforce time.Time

	disconnectHook UserDisconnectFunc
}

const trafficLogRetention = 90 * 24 * time.Hour

func NewManager(
	cfg *config.Config,
	profiles *models.MTProtoProfileStore,
	users *models.UserStore,
	trafficLogs *models.MTProtoTrafficLogStore,
	mailer *email.Sender,
	baseURL string,
) *Manager {
	return &Manager{
		cfg:           cfg,
		profiles:      profiles,
		users:         users,
		trafficLogs:   trafficLogs,
		mailer:        mailer,
		baseURL:       baseURL,
		httpClient:    &http.Client{Timeout: 5 * time.Second},
		lastCounters:  make(map[int][2]int64),
		counterSeen:   make(map[int]bool),
		liveTraffic:   make(map[int][2]int64),
		online:        make(map[int]bool),
		currentConns:  make(map[int]int64),
		cumulative:    make(map[int]int64),
		limits:        make(map[int]int64),
		profileToUser: make(map[int]int),
		disabledUsers: make(map[int]bool),
		trafficBuffer: make(map[int][2]int64),
	}
}

func (m *Manager) Enabled() bool {
	return m != nil && m.cfg.MTProtoEnabled
}

func (m *Manager) SetUserDisconnectHook(fn UserDisconnectFunc) {
	m.mu.Lock()
	m.disconnectHook = fn
	m.mu.Unlock()
}

func (m *Manager) Link(secretHex string) string {
	return BuildLink(m.cfg, secretHex)
}

func BuildLink(cfg *config.Config, secretHex string) string {
	domainHex := hex.EncodeToString([]byte(cfg.MTProtoTLSDomain))
	return fmt.Sprintf("tg://proxy?server=%s&port=%s&secret=ee%s%s",
		cfg.ServerAddr, cfg.MTProtoServerPort, secretHex, domainHex)
}

func (m *Manager) Snapshot() (traffic map[int][2]int64, online map[int]bool, conns map[int]int64) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	traffic = make(map[int][2]int64, len(m.liveTraffic))
	online = make(map[int]bool, len(m.online))
	conns = make(map[int]int64, len(m.currentConns))
	for k, v := range m.liveTraffic {
		traffic[k] = v
	}
	for k, v := range m.online {
		online[k] = v
	}
	for k, v := range m.currentConns {
		conns[k] = v
	}
	return
}

func (m *Manager) InitCumulative(ctx context.Context) {
	profiles, err := m.profiles.ListAll(ctx)
	if err != nil {
		log.Printf("MTProto: failed to init cumulative: %v", err)
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range profiles {
		m.cumulative[p.ID] = p.TrafficUp + p.TrafficDown
		m.limits[p.ID] = p.TrafficLimit
		m.profileToUser[p.ID] = p.UserID
	}
	clear(m.disabledUsers)
	log.Printf("MTProto: cumulative initialized for %d profiles", len(profiles))
}

func (m *Manager) RegisterProfile(ctx context.Context, p *models.MTProtoProfile) {
	m.mu.Lock()
	m.cumulative[p.ID] = p.TrafficUp + p.TrafficDown
	m.limits[p.ID] = p.TrafficLimit
	m.profileToUser[p.ID] = p.UserID
	delete(m.disabledUsers, p.UserID)
	m.mu.Unlock()

	if err := m.Sync(ctx); err != nil {
		log.Printf("MTProto: sync after register profile=%d: %v", p.ID, err)
	}
}

func (m *Manager) UnregisterProfile(ctx context.Context, profileID int) {
	m.mu.Lock()
	delete(m.cumulative, profileID)
	delete(m.limits, profileID)
	delete(m.profileToUser, profileID)
	delete(m.lastCounters, profileID)
	delete(m.counterSeen, profileID)
	delete(m.liveTraffic, profileID)
	delete(m.online, profileID)
	delete(m.currentConns, profileID)
	delete(m.trafficBuffer, profileID)
	m.mu.Unlock()

	if err := m.SyncForceDisconnect(ctx); err != nil {
		log.Printf("MTProto: sync after unregister profile=%d: %v", profileID, err)
	}
}

func (m *Manager) UpdateLimit(profileID int, limitBytes int64) {
	m.mu.Lock()
	m.limits[profileID] = limitBytes
	m.mu.Unlock()
}

func (m *Manager) ResetCumulative(profileID int) {
	m.mu.Lock()
	m.cumulative[profileID] = 0
	m.mu.Unlock()
}

func (m *Manager) ReactivateUserAll(ctx context.Context, userID int) {
	m.mu.Lock()
	delete(m.disabledUsers, userID)
	m.mu.Unlock()

	profiles, err := m.profiles.GetByUserID(ctx, userID)
	if err != nil {
		log.Printf("MTProto reactivate: GetByUserID %d: %v", userID, err)
		return
	}

	m.mu.Lock()
	for _, p := range profiles {
		m.profileToUser[p.ID] = p.UserID
		m.limits[p.ID] = p.TrafficLimit
		m.cumulative[p.ID] = p.TrafficUp + p.TrafficDown
	}
	m.mu.Unlock()

	ids, err := m.profiles.ReactivateAllByUser(ctx, userID)
	if err != nil {
		log.Printf("MTProto reactivate: ReactivateAllByUser %d: %v", userID, err)
		return
	}
	if len(ids) == 0 {
		return
	}
	if err := m.Sync(ctx); err != nil {
		log.Printf("MTProto reactivate: sync user=%d: %v", userID, err)
	}
	log.Printf("MTProto reactivate: user=%d — %d profiles enabled", userID, len(ids))
}

func (m *Manager) DisconnectUserAll(ctx context.Context, userID int, reason string) {
	m.disconnectUserAll(ctx, userID, reason, true)
}

func (m *Manager) DisconnectUserAllLocal(ctx context.Context, userID int, reason string) {
	m.disconnectUserAll(ctx, userID, reason, false)
}

func (m *Manager) disconnectUserAll(ctx context.Context, userID int, reason string, notifyHook bool) {
	ids, err := m.profiles.DeactivateAllByUser(ctx, userID)
	if err != nil {
		log.Printf("MTProto enforce: DeactivateAllByUser %d: %v", userID, err)
		return
	}

	m.mu.Lock()
	m.disabledUsers[userID] = true
	for _, id := range ids {
		m.limits[id] = 0
	}
	hook := m.disconnectHook
	m.mu.Unlock()

	if len(ids) > 0 {
		if err := m.SyncForceDisconnect(ctx); err != nil {
			log.Printf("MTProto enforce: sync user=%d: %v", userID, err)
		}
	}
	if notifyHook && hook != nil {
		hook(ctx, userID, reason)
	}
	log.Printf("MTProto enforce: user=%d disabled (%s) — %d profiles", userID, reason, len(ids))
}

func (m *Manager) Sync(ctx context.Context) error {
	return m.syncWith(ctx, m.Reload, "config synced")
}

// SyncForceDisconnect перезапускает контейнер целиком — единственный способ
// разорвать живые TCP-сессии забаненных пользователей (см. комментарий к Restart).
// Вызывать только в путях принудительного отключения: лимит/баланс/срок.
func (m *Manager) SyncForceDisconnect(ctx context.Context) error {
	return m.syncWith(ctx, m.Restart, "config synced (force restart)")
}

func (m *Manager) syncWith(ctx context.Context, applyFn func(context.Context) error, tag string) error {
	if !m.Enabled() {
		return nil
	}
	active, err := m.profiles.GetAllActive(ctx)
	if err != nil {
		return fmt.Errorf("list active mtproto profiles: %w", err)
	}
	if err := WriteConfig(m.cfg, active, m.cfg.MTProtoConfigPath); err != nil {
		return err
	}
	if err := applyFn(ctx); err != nil {
		return err
	}
	log.Printf("MTProto: %s with %d active profiles", tag, len(active))
	return nil
}

func (m *Manager) Reload(ctx context.Context) error {
	return m.dockerCall(ctx, "/kill?signal=USR2", "SIGUSR2")
}

// Restart полностью рестартит контейнер mtprotoproxy. Это рвёт ВСЕ TCP-сессии
// (включая живых юзеров — они автоматически переподключатся за 1-2 сек), но это
// единственный способ выкинуть только что забаненного юзера: SIGUSR2 в upstream
// (alexbers/mtprotoproxy) перечитывает USERS, но уже открытые соединения не закрывает,
// потому что аутентификация в MTProto-протоколе одноразовая на handshake.
func (m *Manager) Restart(ctx context.Context) error {
	return m.dockerCall(ctx, "/restart", "restart")
}

func (m *Manager) dockerCall(ctx context.Context, suffix, op string) error {
	if m.cfg.MTProtoDockerSocket == "" || m.cfg.MTProtoContainer == "" {
		return nil
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", m.cfg.MTProtoDockerSocket)
		},
	}
	timeout := 5 * time.Second
	if op == "restart" {
		timeout = 30 * time.Second
	}
	client := &http.Client{Transport: tr, Timeout: timeout}
	url := "http://docker/containers/" + m.cfg.MTProtoContainer + suffix
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("docker %s %s: %w", op, m.cfg.MTProtoContainer, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("docker %s %s: status=%d body=%s", op, m.cfg.MTProtoContainer, resp.StatusCode, strings.TrimSpace(string(body)))
}

func (m *Manager) Run(ctx context.Context) {
	if !m.Enabled() {
		log.Println("MTProto manager disabled")
		return
	}
	log.Println("MTProto manager started")
	m.InitCumulative(ctx)
	if err := m.Sync(ctx); err != nil {
		log.Printf("MTProto initial sync error: %v", err)
	}

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	flushTicker := time.NewTicker(1 * time.Minute)
	defer flushTicker.Stop()
	retentionTicker := time.NewTicker(24 * time.Hour)
	defer retentionTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("MTProto manager stopping, final flush...")
			flushCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			m.flushTrafficLogs(flushCtx)
			cancel()
			log.Println("MTProto manager stopped")
			return
		case <-ticker.C:
			m.collectAndEnforce(ctx)
		case <-flushTicker.C:
			m.flushTrafficLogs(ctx)
		case <-retentionTicker.C:
			m.cleanupTrafficLogs(ctx)
		}
	}
}

func (m *Manager) collectAndEnforce(ctx context.Context) {
	metrics, err := m.fetchMetrics(ctx)
	if err != nil {
		log.Printf("MTProto metrics error: %v", err)
		return
	}

	type deltaEntry struct {
		profileID int
		userID    int
		up        int64
		down      int64
		total     int64
		limit     int64
	}
	var deltas []deltaEntry
	byUser := make(map[int]int64)

	m.mu.Lock()
	for label, counters := range metrics {
		id, ok := profileIDFromMetric(label)
		if !ok {
			continue
		}
		userID, known := m.profileToUser[id]
		if !known {
			continue
		}

		prev := m.lastCounters[id]
		upDelta, downDelta := int64(0), int64(0)
		if m.counterSeen[id] {
			upDelta = counters.Up - prev[0]
			downDelta = counters.Down - prev[1]
			if upDelta < 0 {
				upDelta = 0
			}
			if downDelta < 0 {
				downDelta = 0
			}
		}
		m.counterSeen[id] = true
		m.lastCounters[id] = [2]int64{counters.Up, counters.Down}
		m.currentConns[id] = counters.CurrentConns
		m.online[id] = counters.CurrentConns > 0

		if upDelta == 0 && downDelta == 0 {
			continue
		}
		delta := upDelta + downDelta
		m.liveTraffic[id] = [2]int64{upDelta, downDelta}
		m.cumulative[id] += delta
		total := m.cumulative[id]
		limit := m.limits[id]
		byUser[userID] += delta
		entry := m.trafficBuffer[id]
		entry[0] += upDelta
		entry[1] += downDelta
		m.trafficBuffer[id] = entry
		deltas = append(deltas, deltaEntry{
			profileID: id,
			userID:    userID,
			up:        upDelta,
			down:      downDelta,
			total:     total,
			limit:     limit,
		})
	}
	m.mu.Unlock()

	updated := 0
	for _, d := range deltas {
		if err := m.profiles.UpdateTrafficByID(ctx, d.profileID, d.up, d.down); err != nil {
			log.Printf("MTProto update traffic profile=%d: %v", d.profileID, err)
			continue
		}
		updated++
		if d.limit > 0 && d.total >= d.limit {
			m.disconnectProfile(ctx, d.profileID, d.total, d.limit)
		}
	}

	for uid, sum := range byUser {
		if sum <= 0 {
			continue
		}
		m.mu.RLock()
		skip := m.disabledUsers[uid]
		m.mu.RUnlock()
		if skip {
			continue
		}
		remaining, err := m.users.DeductTraffic(ctx, uid, sum)
		if err != nil {
			log.Printf("MTProto: DeductTraffic user=%d delta=%d: %v", uid, sum, err)
			continue
		}
		if remaining <= 0 {
			m.DisconnectUserAll(ctx, uid, "balance exhausted")
			m.notifyBlock(uid, "balance")
		} else if remaining <= 1*1024*1024*1024 {
			m.notifyTrafficLow(uid, remaining)
		}
	}

	if updated > 0 {
		log.Printf("MTProto stats collected: %d profiles updated", updated)
	}

	if time.Since(m.lastFullEnforce) > time.Minute {
		m.enforceAll(ctx)
		m.lastFullEnforce = time.Now()
	}
}

type proxyMetrics struct {
	Up           int64
	Down         int64
	CurrentConns int64
}

func (m *Manager) fetchMetrics(ctx context.Context) (map[string]proxyMetrics, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+m.cfg.MTProtoMetricsAddr+"/metrics", nil)
	if err != nil {
		return nil, err
	}
	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return parseMetrics(resp.Body), nil
}

func parseMetrics(r io.Reader) map[string]proxyMetrics {
	result := make(map[string]proxyMetrics)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, labels, value, ok := splitMetricLine(line)
		if !ok {
			continue
		}
		user := labelValue(labels, "user")
		if user == "" {
			continue
		}
		n, err := strconv.ParseFloat(value, 64)
		if err != nil {
			continue
		}
		entry := result[user]
		switch {
		case strings.HasSuffix(name, "user_octets_from"):
			entry.Up = int64(n)
		case strings.HasSuffix(name, "user_octets_to"):
			entry.Down = int64(n)
		case strings.HasSuffix(name, "user_connects_curr"):
			entry.CurrentConns = int64(n)
		default:
			continue
		}
		result[user] = entry
	}
	return result
}

func splitMetricLine(line string) (name, labels, value string, ok bool) {
	nameEnd := strings.IndexAny(line, "{ ")
	if nameEnd < 0 {
		return "", "", "", false
	}
	name = line[:nameEnd]
	if line[nameEnd] == ' ' {
		return "", "", "", false
	}
	labelsEnd := strings.Index(line[nameEnd:], "}")
	if labelsEnd < 0 {
		return "", "", "", false
	}
	labelsEnd += nameEnd
	labels = line[nameEnd+1 : labelsEnd]
	value = strings.TrimSpace(line[labelsEnd+1:])
	if i := strings.IndexByte(value, ' '); i >= 0 {
		value = value[:i]
	}
	return name, labels, value, true
}

func labelValue(labels, key string) string {
	prefix := key + `="`
	start := strings.Index(labels, prefix)
	if start < 0 {
		return ""
	}
	start += len(prefix)
	end := strings.IndexByte(labels[start:], '"')
	if end < 0 {
		return ""
	}
	return labels[start : start+end]
}

func profileIDFromMetric(label string) (int, bool) {
	if !strings.HasPrefix(label, "mtproto_") {
		return 0, false
	}
	id, err := strconv.Atoi(strings.TrimPrefix(label, "mtproto_"))
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

func (m *Manager) disconnectProfile(ctx context.Context, profileID int, total, limit int64) {
	m.mu.Lock()
	m.limits[profileID] = 0
	m.mu.Unlock()

	p, err := m.profiles.GetByID(ctx, profileID)
	if err != nil {
		log.Printf("MTProto enforce: profile %d not found: %v", profileID, err)
		return
	}
	if err := m.profiles.SetActive(ctx, profileID, false); err != nil {
		log.Printf("MTProto enforce: failed to deactivate profile=%d: %v", profileID, err)
		return
	}
	if err := m.SyncForceDisconnect(ctx); err != nil {
		log.Printf("MTProto enforce: sync after profile limit profile=%d: %v", profileID, err)
	}
	log.Printf("MTProto enforce: profile=%d disabled — limit %d total %d", profileID, limit, total)
	m.notifyProfileLimit(p.ID, p.UserID, p.Name, limit)
}

func (m *Manager) enforceAll(ctx context.Context) {
	exceeded, err := m.profiles.GetExceeded(ctx)
	if err != nil {
		log.Printf("MTProto enforceAll: get exceeded: %v", err)
		return
	}
	if len(exceeded) == 0 {
		m.InitCumulative(ctx)
		return
	}

	needSync := false
	for _, p := range exceeded {
		if err := m.profiles.SetActive(ctx, p.ID, false); err != nil {
			log.Printf("MTProto enforceAll: deactivate profile=%d: %v", p.ID, err)
			continue
		}
		needSync = true
		expired := p.ExpiresAt != nil && p.ExpiresAt.Before(time.Now())
		limitHit := p.TrafficLimit > 0 && p.TrafficUp+p.TrafficDown >= p.TrafficLimit
		if limitHit && !expired {
			m.notifyProfileLimit(p.ID, p.UserID, p.Name, p.TrafficLimit)
		}
	}
	if needSync {
		if err := m.SyncForceDisconnect(ctx); err != nil {
			log.Printf("MTProto enforceAll: sync: %v", err)
		}
	}
	log.Printf("MTProto enforceAll: %d profiles disabled", len(exceeded))
	m.InitCumulative(ctx)
}

func (m *Manager) flushTrafficLogs(ctx context.Context) {
	m.mu.Lock()
	if len(m.trafficBuffer) == 0 {
		m.mu.Unlock()
		return
	}
	batch := m.trafficBuffer
	m.trafficBuffer = make(map[int][2]int64)
	m.mu.Unlock()

	bucket := models.BucketTime(time.Now())
	if err := m.trafficLogs.UpsertBatch(ctx, bucket, batch); err != nil {
		log.Printf("MTProto: traffic_logs flush error (dropping %d rows): %v", len(batch), err)
	}
}

func (m *Manager) cleanupTrafficLogs(ctx context.Context) {
	cutoff := time.Now().Add(-trafficLogRetention)
	n, err := m.trafficLogs.DeleteOlderThan(ctx, cutoff)
	if err != nil {
		log.Printf("MTProto: traffic_logs cleanup error: %v", err)
		return
	}
	if n > 0 {
		log.Printf("MTProto: traffic_logs cleanup deleted %d rows older than %s", n, cutoff.Format(time.RFC3339))
	}
}

func (m *Manager) notifyBlock(userID int, reason string) {
	if m.mailer == nil {
		return
	}
	m.mailer.Submit(fmt.Sprintf("mtproto notify block user=%d reason=%s", userID, reason), func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		u, err := m.users.GetByID(ctx, userID)
		if err != nil {
			log.Printf("MTProto notify block: get user %d: %v", userID, err)
			return nil
		}
		if !u.NotifyBlock {
			return nil
		}
		first, err := m.users.TryMarkBlockNotified(ctx, userID)
		if err != nil || !first {
			return nil
		}
		return m.mailer.SendBlockNotification(u.Email, u.Username, reason, m.baseURL)
	})
}

func (m *Manager) notifyTrafficLow(userID int, remaining int64) {
	if m.mailer == nil {
		return
	}
	m.mailer.Submit(fmt.Sprintf("mtproto notify traffic-low user=%d", userID), func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		u, err := m.users.GetByID(ctx, userID)
		if err != nil || !u.NotifyTrafficLow {
			return nil
		}
		first, err := m.users.TryMarkTrafficLowNotified(ctx, userID)
		if err != nil || !first {
			return nil
		}
		return m.mailer.SendTrafficLowNotification(u.Email, u.Username, remaining, m.baseURL)
	})
}

func (m *Manager) notifyProfileLimit(profileID, userID int, profileName string, limitBytes int64) {
	if m.mailer == nil {
		return
	}
	m.mailer.Submit(fmt.Sprintf("mtproto notify profile-limit user=%d profile=%d", userID, profileID), func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		u, err := m.users.GetByID(ctx, userID)
		if err != nil || !u.NotifyProfileLimit {
			return nil
		}
		first, err := m.profiles.TryMarkLimitNotified(ctx, profileID)
		if err != nil || !first {
			return nil
		}
		return m.mailer.SendProfileLimitNotification(u.Email, u.Username, profileName, limitBytes, m.baseURL)
	})
}

func WriteConfig(cfg *config.Config, profiles []models.MTProtoProfile, path string) error {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# Generated by xray-panel. Manual edits will be overwritten.\n")
	fmt.Fprintf(&b, "PORT = %s\n", strconv.Quote(cfg.MTProtoServerPort))
	fmt.Fprintf(&b, "PORT = int(PORT)\n")
	fmt.Fprintf(&b, "LISTEN_ADDR_IPV4 = %s\n", strconv.Quote(cfg.MTProtoListenAddr))
	fmt.Fprintf(&b, "LISTEN_ADDR_IPV6 = %s\n", strconv.Quote(cfg.MTProtoListenAddr6))
	fmt.Fprintf(&b, "TLS_DOMAIN = %s\n", strconv.Quote(cfg.MTProtoTLSDomain))
	fmt.Fprintf(&b, "MASK = True\n")
	fmt.Fprintf(&b, "MASK_HOST = %s\n", strconv.Quote(cfg.MTProtoMaskHost))
	fmt.Fprintf(&b, "MASK_PORT = 443\n")
	fmt.Fprintf(&b, "MODES = {'classic': False, 'secure': True, 'tls': True}\n")
	fmt.Fprintf(&b, "USE_MIDDLE_PROXY = False\n")
	fmt.Fprintf(&b, "PREFER_IPV6 = False\n")
	fmt.Fprintf(&b, "METRICS_PORT = %d\n", cfg.MTProtoMetricsPort)
	fmt.Fprintf(&b, "METRICS_LISTEN_ADDR_IPV4 = '127.0.0.1'\n")
	fmt.Fprintf(&b, "METRICS_LISTEN_ADDR_IPV6 = '::1'\n")
	fmt.Fprintf(&b, "METRICS_WHITELIST = ['127.0.0.1', '::1']\n")
	fmt.Fprintf(&b, "METRICS_EXPORT_LINKS = False\n")
	fmt.Fprintf(&b, "STATS_PRINT_PERIOD = 10\n")
	fmt.Fprintf(&b, "USERS = {\n")
	for _, p := range profiles {
		fmt.Fprintf(&b, "    %s: %s,\n", strconv.Quote(p.MetricName()), strconv.Quote(p.SecretHex))
	}
	fmt.Fprintf(&b, "}\n")
	if cfg.MTProtoSocks5Host != "" && cfg.MTProtoSocks5Port > 0 {
		fmt.Fprintf(&b, "SOCKS5_HOST = %s\n", strconv.Quote(cfg.MTProtoSocks5Host))
		fmt.Fprintf(&b, "SOCKS5_PORT = %d\n", cfg.MTProtoSocks5Port)
		if cfg.MTProtoSocks5User != "" {
			fmt.Fprintf(&b, "SOCKS5_USERNAME = %s\n", strconv.Quote(cfg.MTProtoSocks5User))
			fmt.Fprintf(&b, "SOCKS5_PASSWORD = %s\n", strconv.Quote(cfg.MTProtoSocks5Pass))
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir mtproto config dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write mtproto config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename mtproto config: %w", err)
	}
	return nil
}
