package xray

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"xray-panel/internal/email"
	"xray-panel/internal/models"
)

type StatsCollector struct {
	client      *Client
	profiles    *models.VPNProfileStore
	users       *models.UserStore
	trafficLogs *models.TrafficLogStore
	firewall    *Firewall
	mailer      *email.Sender // nil если SMTP не настроен — уведомления пропускаем
	baseURL     string

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

	// Мэппинг UUID → profile_id. Нужен, чтобы при flush в traffic_logs писать
	// по profile_id, а не делать лишний SELECT по UUID на каждый тик.
	uuidToProfile   map[string]int
	uuidToProfileMu sync.RWMutex

	// trafficBuffer копит дельты для записи в traffic_logs. Ключ — profile_id,
	// значение — [bytes_up, bytes_down] с момента последнего flush. Сбрасывается
	// в БД раз в минуту одним batch-UPSERT'ом (см. flushTrafficLogs).
	trafficBuffer   map[int][2]int64
	trafficBufferMu sync.Mutex

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

// trafficLogRetention — сколько храним 5-минутные бакеты. Дневной retention-job
// в Run удаляет записи старше этого срока. 90 дней хватает на графики
// 24h/7d/30d/90d без второго слоя агрегации.
const trafficLogRetention = 90 * 24 * time.Hour

func NewStatsCollector(
	client *Client,
	profiles *models.VPNProfileStore,
	users *models.UserStore,
	trafficLogs *models.TrafficLogStore,
	firewall *Firewall,
	mailer *email.Sender,
	baseURL string,
) *StatsCollector {
	return &StatsCollector{
		client:        client,
		profiles:      profiles,
		users:         users,
		trafficLogs:   trafficLogs,
		firewall:      firewall,
		mailer:        mailer,
		baseURL:       baseURL,
		pending:       make(map[string][2]int64),
		cumulative:    make(map[string]int64),
		limits:        make(map[string]int64),
		uuidToUser:    make(map[string]int),
		uuidToProfile: make(map[string]int),
		trafficBuffer: make(map[int][2]int64),
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
	s.mailer.Submit(fmt.Sprintf("notify block user=%d reason=%s", userID, reason), func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		// Сначала читаем настройку юзера: если notify_block выключен — не
		// расходуем попытку (не ставим флаг). Юзер может включить галку —
		// и при следующем тике коллектора письмо отправится. TryMark идёт
		// после проверки и атомарно защищает от гонок.
		u, err := s.users.GetByID(ctx, userID)
		if err != nil {
			log.Printf("Notify block: get user %d: %v", userID, err)
			return nil
		}
		if !u.NotifyBlock {
			return nil
		}
		first, err := s.users.TryMarkBlockNotified(ctx, userID)
		if err != nil {
			log.Printf("Notify block: mark user %d: %v", userID, err)
			return nil
		}
		if !first {
			return nil
		}
		return s.mailer.SendBlockNotification(u.Email, u.Username, reason, s.baseURL)
	})
}

// trafficLowThreshold — порог «скоро закончится», при котором шлём
// предупредительное письмо. Один раз, пока юзер не пополнит баланс.
const trafficLowThreshold int64 = 1 * 1024 * 1024 * 1024 // 1 GiB

// notifyProfileLimit асинхронно шлёт юзеру письмо «исчерпан personal-лимит
// профиля». Идемпотентно через vpn_profiles.limit_notified_at: при смене
// лимита (SetLimit), сбросе счётчика (ResetTraffic) или ручной активации
// профиля флаг сбрасывается — следующий цикл превышения снова отправит письмо.
func (s *StatsCollector) notifyProfileLimit(profileID, userID int, profileName string, limitBytes int64) {
	if s.mailer == nil {
		return
	}
	s.mailer.Submit(fmt.Sprintf("notify profile-limit user=%d profile=%d", userID, profileID), func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		// См. комментарий в notifyBlock: настройку проверяем до TryMark,
		// иначе включение галки post-factum никогда не разморозит уведомление.
		u, err := s.users.GetByID(ctx, userID)
		if err != nil {
			log.Printf("Notify profile limit: get user %d: %v", userID, err)
			return nil
		}
		if !u.NotifyProfileLimit {
			return nil
		}
		first, err := s.profiles.TryMarkLimitNotified(ctx, profileID)
		if err != nil {
			log.Printf("Notify profile limit: mark profile %d: %v", profileID, err)
			return nil
		}
		if !first {
			return nil
		}
		return s.mailer.SendProfileLimitNotification(u.Email, u.Username, profileName, limitBytes, s.baseURL)
	})
}

// notifyTrafficLow асинхронно шлёт юзеру письмо «скоро закончится трафик».
// Вызывается после DeductTraffic, когда remaining в пороге (0, trafficLowThreshold].
// Идемпотентно через traffic_low_notified_at — при пополнении флаг сбрасывается.
func (s *StatsCollector) notifyTrafficLow(userID int, remaining int64) {
	if s.mailer == nil {
		return
	}
	s.mailer.Submit(fmt.Sprintf("notify traffic-low user=%d", userID), func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		// См. notifyBlock: настройку проверяем до TryMark.
		u, err := s.users.GetByID(ctx, userID)
		if err != nil {
			log.Printf("Notify low: get user %d: %v", userID, err)
			return nil
		}
		if !u.NotifyTrafficLow {
			return nil
		}
		first, err := s.users.TryMarkTrafficLowNotified(ctx, userID)
		if err != nil {
			log.Printf("Notify low: mark user %d: %v", userID, err)
			return nil
		}
		if !first {
			return nil
		}
		return s.mailer.SendTrafficLowNotification(u.Email, u.Username, remaining, s.baseURL)
	})
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
	s.uuidToProfileMu.Lock()

	clear(s.uuidToUser)
	clear(s.uuidToProfile)
	for _, p := range profiles {
		s.cumulative[p.UUID] = p.TrafficUp + p.TrafficDown
		s.limits[p.UUID] = p.TrafficLimit
		s.uuidToUser[p.UUID] = p.UserID
		s.uuidToProfile[p.UUID] = p.ID
	}

	s.uuidToProfileMu.Unlock()
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
// mapping UUID → user_id, UUID → profile_id и personal-лимит. Без этого списание
// с user-баланса для свежего профиля не работало бы до ближайшей ресинхронизации
// InitCumulative, а traffic_logs не знал бы profile_id для UPSERT'а.
func (s *StatsCollector) RegisterProfile(uuid string, profileID, userID int, limitBytes int64) {
	s.uuidToUserMu.Lock()
	s.uuidToUser[uuid] = userID
	s.uuidToUserMu.Unlock()

	s.uuidToProfileMu.Lock()
	s.uuidToProfile[uuid] = profileID
	s.uuidToProfileMu.Unlock()

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

	s.uuidToProfileMu.Lock()
	delete(s.uuidToProfile, uuid)
	s.uuidToProfileMu.Unlock()

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

	// Главный тикер 5 секунд: collectAndEnforce пишет s.lastTraffic, из него
	// updateOnlineStatus определяет активных юзеров — порядок важен. Страховка
	// enforceAll раз в минуту сидит внутри collectAndEnforce.
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	// Flush буфера traffic_logs в БД. Раз в минуту, чтобы не долбить UPSERT
	// каждый 5-секундный тик. При падении теряем до 60 сек дельт — терпимо.
	flushTicker := time.NewTicker(1 * time.Minute)
	defer flushTicker.Stop()

	// Retention: удаление бакетов старше trafficLogRetention. Раз в сутки.
	retentionTicker := time.NewTicker(24 * time.Hour)
	defer retentionTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Stats collector stopping, final flush…")
			// ctx уже отменён — используем fresh context, чтобы успеть записать
			// накопленные дельты перед выходом.
			flushCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			s.flushTrafficLogs(flushCtx)
			cancel()
			log.Println("Stats collector stopped")
			return
		case <-ticker.C:
			s.collectAndEnforce(ctx)
			s.updateOnlineStatus(ctx)
		case <-flushTicker.C:
			s.flushTrafficLogs(ctx)
		case <-retentionTicker.C:
			s.cleanupTrafficLogs(ctx)
		}
	}
}

// flushTrafficLogs сбрасывает накопленный буфер в traffic_logs одним
// batch-UPSERT'ом. Бакет выравнивается на 5-минутную границу, несколько
// flush'ов внутри одного бакета суммируются через ON CONFLICT. При ошибке
// буфер сбрасываем всё равно — retry смысла не имеет, данные для графиков,
// не для биллинга; следующий тик продолжит копить с нуля.
func (s *StatsCollector) flushTrafficLogs(ctx context.Context) {
	s.trafficBufferMu.Lock()
	if len(s.trafficBuffer) == 0 {
		s.trafficBufferMu.Unlock()
		return
	}
	batch := s.trafficBuffer
	s.trafficBuffer = make(map[int][2]int64)
	s.trafficBufferMu.Unlock()

	bucket := models.BucketTime(time.Now())
	if err := s.trafficLogs.UpsertBatch(ctx, bucket, batch); err != nil {
		log.Printf("Stats: traffic_logs flush error (dropping %d rows): %v", len(batch), err)
		return
	}
}

// cleanupTrafficLogs удаляет бакеты старше trafficLogRetention. Вызывается
// раз в сутки; нагрузка на БД разовая, индекс по logged_at покрывает запрос.
func (s *StatsCollector) cleanupTrafficLogs(ctx context.Context) {
	cutoff := time.Now().Add(-trafficLogRetention)
	n, err := s.trafficLogs.DeleteOlderThan(ctx, cutoff)
	if err != nil {
		log.Printf("Stats: traffic_logs cleanup error: %v", err)
		return
	}
	if n > 0 {
		log.Printf("Stats: traffic_logs cleanup deleted %d rows older than %s", n, cutoff.Format(time.RFC3339))
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

		// 5. Копим дельту в буфер для traffic_logs. Сбрасывается раз в минуту
		// (flushTrafficLogs) одним batch-UPSERT'ом в 5-минутный бакет —
		// посекундный INSERT сюда был бы убийством БД.
		s.uuidToProfileMu.RLock()
		pid, ok := s.uuidToProfile[uuid]
		s.uuidToProfileMu.RUnlock()
		if ok {
			s.trafficBufferMu.Lock()
			entry := s.trafficBuffer[pid]
			entry[0] += up
			entry[1] += down
			s.trafficBuffer[pid] = entry
			s.trafficBufferMu.Unlock()
		}
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

	// Письмо юзеру — «лимит профиля исчерпан, профиль выключен». Идемпотентно
	// по profile.limit_notified_at. SetActive(false) выше не сбросил отметку
	// (сброс только на TRUE), так что повторного письма при следующем
	// enforceAll-проходе не будет.
	s.notifyProfileLimit(p.ID, p.UserID, p.Name, limit)
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

		// Разбираем, из-за чего GetExceeded вернул профиль. SQL-фильтр
		// объединяет expired / balance exhausted / personal-лимит; уведомление
		// про personal-лимит шлём только в последнем случае — для expired и
		// balance exhausted юзер получит своё письмо через notifyBlock.
		expired := p.ExpiresAt != nil && p.ExpiresAt.Before(time.Now())
		limitHit := p.TrafficLimit > 0 && p.TrafficUp+p.TrafficDown >= p.TrafficLimit

		reason := "traffic limit"
		if expired {
			reason = "expired"
		}
		log.Printf("EnforceAll: %s disabled — %s", p.UUID, reason)

		if limitHit && !expired {
			s.notifyProfileLimit(p.ID, p.UserID, p.Name, p.TrafficLimit)
		}
	}

	if len(exceeded) > 0 {
		log.Printf("EnforceAll: %d profiles disabled", len(exceeded))
	}

	// Ресинхронизируем cumulative и limits из БД
	s.InitCumulative(ctx)
}
