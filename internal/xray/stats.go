package xray

import (
	"context"
	"log"
	"sync"
	"time"

	"xray-panel/internal/email"
	"xray-panel/internal/models"
)

type StatsCollector struct {
	client   *Client
	profiles *models.VPNProfileStore
	users    *models.UserStore
	firewall *Firewall
	mailer   *email.Sender // nil если SMTP не настроен — уведомления пропускаем
	baseURL  string

	// pending хранит трафик, который не удалось записать в БД
	pending map[string][2]int64

	// Кумулятивный трафик в памяти — для быстрого enforce без запроса к БД.
	// Ключ — UUID, значение — total bytes (из БД + все дельты).
	cumulative   map[string]int64
	cumulativeMu sync.Mutex

	// Лимиты в памяти — чтобы не дёргать БД на каждую проверку.
	// Это personal-лимит на профиль (пользовательская фича),
	// НЕ общий лимит подписки — тот считается на уровне user.
	limits   map[string]int64 // uuid → traffic_limit (0 = без лимита)
	limitsMu sync.RWMutex

	// Мэппинг UUID профиля → user_id владельца.
	// Нужен для агрегации дельт трафика по юзеру и вызова DeductTraffic.
	uuidToUser   map[string]int
	uuidToUserMu sync.RWMutex

	// disabledUsers — юзеры, чей баланс уже исчерпан в этом цикле enforcement.
	// Чтобы не спамить DeductTraffic и DisconnectUserAll в каждом тике
	// до ближайшей ресинхронизации.
	disabledUsers   map[int]bool
	disabledUsersMu sync.Mutex

	// Кэш для HTTP-хендлеров
	snapMu        sync.RWMutex
	lastTraffic   map[string][2]int64
	lastOnline    map[string]bool
	lastOnlineIPs map[string][]string

	lastFullEnforce time.Time
}

func NewStatsCollector(
	client *Client,
	profiles *models.VPNProfileStore,
	users *models.UserStore,
	firewall *Firewall,
	mailer *email.Sender,
	baseURL string,
) *StatsCollector {
	return &StatsCollector{
		client:        client,
		profiles:      profiles,
		users:         users,
		firewall:      firewall,
		mailer:        mailer,
		baseURL:       baseURL,
		pending:       make(map[string][2]int64),
		cumulative:    make(map[string]int64),
		limits:        make(map[string]int64),
		uuidToUser:    make(map[string]int),
		disabledUsers: make(map[int]bool),
	}
}

// notifyBlock асинхронно шлёт юзеру письмо о блокировке. reason:
// "balance" (кончился трафик) или "expired" (истёк срок подписки).
// Идемпотентно: TryMarkBlockNotified не даст отправить повторно, пока
// юзер не пополнит баланс (что сбросит флаг).
func (s *StatsCollector) notifyBlock(userID int, reason string) {
	if s.mailer == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		first, err := s.users.TryMarkBlockNotified(ctx, userID)
		if err != nil {
			log.Printf("Notify block: mark user %d: %v", userID, err)
			return
		}
		if !first {
			return
		}

		u, err := s.users.GetByID(ctx, userID)
		if err != nil {
			log.Printf("Notify block: get user %d: %v", userID, err)
			return
		}
		if !u.NotifyBlock {
			return
		}
		if err := s.mailer.SendBlockNotification(u.Email, u.Username, reason, s.baseURL); err != nil {
			log.Printf("Notify block email user=%d: %v", userID, err)
		}
	}()
}

// trafficLowThreshold — порог «скоро закончится», при котором шлём
// предупредительное письмо. Один раз, пока юзер не пополнит баланс.
const trafficLowThreshold int64 = 1 * 1024 * 1024 * 1024 // 1 GiB

// notifyTrafficLow асинхронно шлёт юзеру письмо «скоро закончится трафик».
// Вызывается после DeductTraffic, когда remaining в пороге (0, trafficLowThreshold].
// Идемпотентно через traffic_low_notified_at — при пополнении флаг сбрасывается.
func (s *StatsCollector) notifyTrafficLow(userID int, remaining int64) {
	if s.mailer == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		first, err := s.users.TryMarkTrafficLowNotified(ctx, userID)
		if err != nil {
			log.Printf("Notify low: mark user %d: %v", userID, err)
			return
		}
		if !first {
			return
		}

		u, err := s.users.GetByID(ctx, userID)
		if err != nil {
			log.Printf("Notify low: get user %d: %v", userID, err)
			return
		}
		if !u.NotifyTrafficLow {
			return
		}
		if err := s.mailer.SendTrafficLowNotification(u.Email, u.Username, remaining, s.baseURL); err != nil {
			log.Printf("Notify low email user=%d: %v", userID, err)
		}
	}()
}

// Snapshot возвращает потокобезопасный снимок для HTTP-хендлеров.
func (s *StatsCollector) Snapshot() (
	traffic map[string][2]int64,
	online map[string]bool,
	ips map[string][]string,
) {
	s.snapMu.RLock()
	defer s.snapMu.RUnlock()
	return s.lastTraffic, s.lastOnline, s.lastOnlineIPs
}

// InitCumulative загружает текущий трафик и лимиты из БД в память.
// Вызывать при старте и периодически для ресинхронизации.
// Также сбрасывает disabledUsers: следующий цикл перепроверит баланс юзера,
// и если он был пополнен — spam DeductTraffic возобновится штатно.
func (s *StatsCollector) InitCumulative(ctx context.Context) {
	profiles, err := s.profiles.ListAll(ctx)
	if err != nil {
		log.Printf("Stats: failed to init cumulative: %v", err)
		return
	}

	s.cumulativeMu.Lock()
	s.limitsMu.Lock()
	s.uuidToUserMu.Lock()

	clear(s.uuidToUser)
	for _, p := range profiles {
		s.cumulative[p.UUID] = p.TrafficUp + p.TrafficDown
		s.limits[p.UUID] = p.TrafficLimit
		s.uuidToUser[p.UUID] = p.UserID
	}

	s.uuidToUserMu.Unlock()
	s.limitsMu.Unlock()
	s.cumulativeMu.Unlock()

	s.disabledUsersMu.Lock()
	clear(s.disabledUsers)
	s.disabledUsersMu.Unlock()

	log.Printf("Stats: cumulative initialized for %d profiles", len(profiles))
}

// UpdateLimit обновляет personal-лимит профиля в кэше.
// Вызывать из админки при смене лимита существующего профиля.
func (s *StatsCollector) UpdateLimit(uuid string, limitBytes int64) {
	s.limitsMu.Lock()
	s.limits[uuid] = limitBytes
	s.limitsMu.Unlock()
}

// RegisterProfile регистрирует только что созданный профиль в кэшах коллектора:
// mapping UUID → user_id и personal-лимит. Без этого списание с user-баланса
// для свежего профиля не работало бы до ближайшей ресинхронизации InitCumulative.
func (s *StatsCollector) RegisterProfile(uuid string, userID int, limitBytes int64) {
	s.uuidToUserMu.Lock()
	s.uuidToUser[uuid] = userID
	s.uuidToUserMu.Unlock()

	s.limitsMu.Lock()
	s.limits[uuid] = limitBytes
	s.limitsMu.Unlock()

	// Флаг disabledUsers снимаем: юзер создаёт профиль, значит баланс >0
	// (это проверяет хендлер). Если снят в этом тике — пусть следующий тик
	// сразу начнёт списывать дельты.
	s.disabledUsersMu.Lock()
	delete(s.disabledUsers, userID)
	s.disabledUsersMu.Unlock()
}

// UnregisterProfile удаляет все следы профиля из кэшей. Вызывать при удалении
// профиля (из дашборда / админки), чтобы старый UUID не числился в статистике
// и не блокировал следующие списания.
func (s *StatsCollector) UnregisterProfile(uuid string) {
	s.uuidToUserMu.Lock()
	delete(s.uuidToUser, uuid)
	s.uuidToUserMu.Unlock()

	s.limitsMu.Lock()
	delete(s.limits, uuid)
	s.limitsMu.Unlock()

	s.cumulativeMu.Lock()
	delete(s.cumulative, uuid)
	s.cumulativeMu.Unlock()
}

// ResetCumulative сбрасывает кумулятивный счётчик (вызывать при сбросе трафика)
func (s *StatsCollector) ResetCumulative(uuid string) {
	s.cumulativeMu.Lock()
	s.cumulative[uuid] = 0
	s.cumulativeMu.Unlock()
}

func (s *StatsCollector) Run(ctx context.Context) {
	log.Println("Stats collector started")

	// Единый тикер 5 секунд: порядок важен — collectAndEnforce пишет
	// s.lastTraffic, из которого updateOnlineStatus определяет активных юзеров.
	// Секундный тик был избыточен: enforcement'у хватает 5 сек (overspend при
	// 100 Мбит/с ~60 МБ — шум), UI-поллинг фронтенда тоже 3–10 сек, а ценой
	// была 1 gRPC + N DB-writes каждую секунду. Страховка enforceAll раз в
	// минуту живёт внутри collectAndEnforce.
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Stats collector stopped")
			return
		case <-ticker.C:
			s.collectAndEnforce(ctx)
			s.updateOnlineStatus(ctx)
		}
	}
}

// collectAndEnforce — быстрый цикл: один gRPC → in-memory enforce → запись в БД
func (s *StatsCollector) collectAndEnforce(ctx context.Context) {
	// Один вызов со сбросом — получаем дельту и обнуляем счётчики Xray
	traffic, err := s.client.QueryAllUserTraffic(ctx, true)
	if err != nil {
		log.Printf("Stats collect error: %v", err)
		return
	}

	// Объединяем с pending (незаписанные ранее дельты)
	for email, stats := range s.pending {
		entry := traffic[email]
		entry[0] += stats[0]
		entry[1] += stats[1]
		traffic[email] = entry
	}
	clear(s.pending)

	// Сохраняем дельту в snapshot для HTTP-хендлеров
	s.snapMu.Lock()
	s.lastTraffic = traffic
	s.snapMu.Unlock()

	// Агрегация дельт по user_id для списания с подписочного баланса.
	byUser := make(map[int]int64)

	updated := 0
	for uuid, stats := range traffic {
		up, down := stats[0], stats[1]
		if up == 0 && down == 0 {
			continue
		}

		delta := up + down

		// 1. Обновляем кумулятивный счётчик в памяти
		s.cumulativeMu.Lock()
		s.cumulative[uuid] += delta
		currentTotal := s.cumulative[uuid]
		s.cumulativeMu.Unlock()

		// 2. Проверяем personal-лимит профиля (пользовательская фича)
		s.limitsMu.RLock()
		limit := s.limits[uuid]
		s.limitsMu.RUnlock()

		if limit > 0 && currentTotal >= limit {
			s.disconnectUser(ctx, uuid, currentTotal, limit)
		}

		// 3. Копим дельту в байтах для списания с баланса юзера
		s.uuidToUserMu.RLock()
		if uid, ok := s.uuidToUser[uuid]; ok {
			byUser[uid] += delta
		}
		s.uuidToUserMu.RUnlock()

		// 4. Пишем per-profile статистику в БД (для отображения)
		if err := s.profiles.UpdateTraffic(ctx, uuid, up, down); err != nil {
			log.Printf("Stats update error for %s: %v — will retry", uuid, err)
			s.pending[uuid] = [2]int64{up, down}
			continue
		}
		updated++
	}

	// Списываем суммарные дельты с подписочного баланса юзеров.
	// Если баланс исчерпан — отключаем все профили юзера разом.
	for uid, sum := range byUser {
		if sum <= 0 {
			continue
		}
		s.disabledUsersMu.Lock()
		skip := s.disabledUsers[uid]
		s.disabledUsersMu.Unlock()
		if skip {
			continue
		}

		remaining, err := s.users.DeductTraffic(ctx, uid, sum)
		if err != nil {
			log.Printf("Stats: DeductTraffic user=%d delta=%d: %v", uid, sum, err)
			continue
		}
		if remaining <= 0 {
			s.DisconnectUserAll(ctx, uid, "balance exhausted")
			s.disabledUsersMu.Lock()
			s.disabledUsers[uid] = true
			s.disabledUsersMu.Unlock()
			s.notifyBlock(uid, "balance")
		} else if remaining <= trafficLowThreshold {
			// Порог «скоро закончится» — одно письмо на цикл подписки.
			s.notifyTrafficLow(uid, remaining)
		}
	}

	if updated > 0 {
		log.Printf("Stats collected: %d users updated", updated)
	}

	// Подстраховка: полная проверка раз в минуту
	if time.Since(s.lastFullEnforce) > time.Minute {
		s.enforceAll(ctx)
		s.lastFullEnforce = time.Now()
	}
}

// ReactivateUserAll включает все профили юзера обратно: снимает is_active=false
// в БД, добавляет UUID в Xray, восстанавливает кэши (uuidToUser, limits) и
// снимает пометку disabledUsers. Вызывать после успешной оплаты подписки/аддона
// или админского пополнения баланса. Если профили уже активны — no-op.
func (s *StatsCollector) ReactivateUserAll(ctx context.Context, userID int) {
	// Снимаем флаг сразу — следующий тик collectAndEnforce увидит
	// свежий баланс и начнёт списывать штатно.
	s.disabledUsersMu.Lock()
	delete(s.disabledUsers, userID)
	s.disabledUsersMu.Unlock()

	profiles, err := s.profiles.GetByUserID(ctx, userID)
	if err != nil {
		log.Printf("Reactivate: GetByUserID %d: %v", userID, err)
		return
	}

	// Обновим кэши для ВСЕХ профилей юзера (в т.ч. уже активных — на случай,
	// если их personal-лимит изменили).
	s.uuidToUserMu.Lock()
	s.limitsMu.Lock()
	for _, p := range profiles {
		s.uuidToUser[p.UUID] = p.UserID
		s.limits[p.UUID] = p.TrafficLimit
	}
	s.limitsMu.Unlock()
	s.uuidToUserMu.Unlock()

	uuids, err := s.profiles.ReactivateAllByUser(ctx, userID)
	if err != nil {
		log.Printf("Reactivate: ReactivateAllByUser %d: %v", userID, err)
		return
	}
	if len(uuids) == 0 {
		return
	}

	added := 0
	for _, uuid := range uuids {
		if err := s.client.AddUser(ctx, uuid, uuid); err != nil {
			log.Printf("Reactivate: AddUser %s: %v", uuid, err)
			continue
		}
		added++
	}
	log.Printf("Reactivate: user=%d — %d/%d profiles returned to Xray", userID, added, len(uuids))
}

// DisconnectUserAll отключает все профили юзера по user_id:
// блокирует TCP всех его UUID, удаляет из Xray, снимает is_active в БД.
// Используется, когда баланс подписки исчерпан или истёк срок подписки.
// Экспортирован, чтобы подписочный воркер мог вызывать на истёкших юзерах.
func (s *StatsCollector) DisconnectUserAll(ctx context.Context, userID int, reason string) {
	profiles, err := s.profiles.GetByUserID(ctx, userID)
	if err != nil {
		log.Printf("Enforce: GetByUserID %d: %v", userID, err)
		return
	}

	blocked := 0
	for _, p := range profiles {
		if !p.IsActive {
			continue
		}
		blocked += s.firewall.BlockUser(ctx, s.client, p.UUID)
		if err := s.client.RemoveUser(ctx, p.UUID); err != nil {
			log.Printf("Enforce: RemoveUser %s: %v (may be already removed)", p.UUID, err)
		}
		// Обнулим personal-лимит в кэше, чтобы per-profile enforcement не гонялся
		// повторно — записи оживут при ресинхронизации InitCumulative.
		s.limitsMu.Lock()
		s.limits[p.UUID] = 0
		s.limitsMu.Unlock()
	}

	uuids, err := s.profiles.DeactivateAllByUser(ctx, userID)
	if err != nil {
		log.Printf("Enforce: DeactivateAllByUser %d: %v", userID, err)
		return
	}

	log.Printf("Enforce: user=%d disabled (%s) — %d profiles, %d IPs blocked",
		userID, reason, len(uuids), blocked)
}

// disconnectUser отключает ОДИН профиль по personal-лимиту (пользовательская фича).
// Это не связано с балансом подписки — только с лимитом конкретного профиля.
func (s *StatsCollector) disconnectUser(ctx context.Context, uuid string, total, limit int64) {
	// Сразу обнуляем лимит в кэше, чтобы следующие циклы не спамили повторными попытками
	s.limitsMu.Lock()
	s.limits[uuid] = 0
	s.limitsMu.Unlock()

	// 1. Блокируем TCP через iptables REJECT (правила живут до реактивации!)
	blocked := s.firewall.BlockUser(ctx, s.client, uuid)

	// 2. Удаляем из Xray (ошибка "not found" — не страшно, мог быть удалён ранее)
	if err := s.client.RemoveUser(ctx, uuid); err != nil {
		log.Printf("Enforce: RemoveUser %s: %v (may be already removed)", uuid, err)
	}

	// 3. Деактивируем в БД
	p, err := s.profiles.GetByUUID(ctx, uuid)
	if err != nil {
		log.Printf("Enforce: profile %s not found: %v", uuid, err)
		return
	}

	if err := s.profiles.SetActive(ctx, p.ID, false); err != nil {
		log.Printf("Enforce: failed to deactivate %s: %v", uuid, err)
		return
	}

	overshoot := total - limit
	log.Printf("Enforce: %s disabled — limit %d, total %d, overshoot %.1f MB, blocked %d IPs",
		uuid, limit, total, float64(overshoot)/(1024*1024), blocked)
}

// updateOnlineStatus — медленный цикл: онлайн-статус и IP-адреса
func (s *StatsCollector) updateOnlineStatus(ctx context.Context) {
	onlineUsers := make(map[string]bool)

	s.snapMu.RLock()
	liveTraffic := s.lastTraffic
	s.snapMu.RUnlock()

	if online, err := s.client.GetOnlineUsers(ctx, liveTraffic); err == nil {
		onlineUsers = online
	}

	onlineIPs := s.client.GetOnlineIPs(ctx, onlineUsers)

	s.snapMu.Lock()
	s.lastOnline = onlineUsers
	s.lastOnlineIPs = onlineIPs
	s.snapMu.Unlock()
}

// enforceAll — полная проверка всех профилей (подстраховка для expires_at и рассинхрона)
func (s *StatsCollector) enforceAll(ctx context.Context) {
	exceeded, err := s.profiles.GetExceeded(ctx)
	if err != nil {
		log.Printf("EnforceAll: failed to get exceeded profiles: %v", err)
		return
	}

	for _, p := range exceeded {
		s.firewall.BlockUser(ctx, s.client, p.UUID)
		if err := s.client.RemoveUser(ctx, p.UUID); err != nil {
			log.Printf("EnforceAll: RemoveUser %s: %v", p.UUID, err)
		}
		if err := s.profiles.SetActive(ctx, p.ID, false); err != nil {
			log.Printf("EnforceAll: failed to deactivate %s: %v", p.UUID, err)
			continue
		}

		reason := "traffic limit"
		if p.ExpiresAt != nil && p.ExpiresAt.Before(time.Now()) {
			reason = "expired"
		}
		log.Printf("EnforceAll: %s disabled — %s", p.UUID, reason)
	}

	if len(exceeded) > 0 {
		log.Printf("EnforceAll: %d profiles disabled", len(exceeded))
	}

	// Ресинхронизируем cumulative и limits из БД
	s.InitCumulative(ctx)
}
