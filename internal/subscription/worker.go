package subscription

import (
	"context"
	"fmt"
	"log"
	"time"

	"xray-panel/internal/email"
	"xray-panel/internal/models"
	"xray-panel/internal/xray"
)

// Worker фоновый процесс подписочной модели:
//   - раз в expireEvery ищет юзеров с истёкшим тарифом, замораживает их
//     extra в frozen_extra и отключает все профили;
//   - раз в reminderEvery шлёт email-напоминания за 5 и 1 день до окончания.
type Worker struct {
	users    *models.UserStore
	profiles *models.VPNProfileStore
	mailer   *email.Sender // nil если SMTP не настроен — reminder-цикл станет no-op
	holder   *xray.Holder
	baseURL  string

	expireEvery   time.Duration
	reminderEvery time.Duration
}

func NewWorker(
	users *models.UserStore,
	profiles *models.VPNProfileStore,
	mailer *email.Sender,
	holder *xray.Holder,
	baseURL string,
) *Worker {
	return &Worker{
		users:         users,
		profiles:      profiles,
		mailer:        mailer,
		holder:        holder,
		baseURL:       baseURL,
		expireEvery:   1 * time.Minute,
		reminderEvery: 1 * time.Hour,
	}
}

func (w *Worker) Run(ctx context.Context) {
	log.Println("Subscription worker started")

	expireTicker := time.NewTicker(w.expireEvery)
	defer expireTicker.Stop()
	reminderTicker := time.NewTicker(w.reminderEvery)
	defer reminderTicker.Stop()

	// Сразу прогоним expire при старте, не ждём первого тика.
	w.runExpire(ctx)

	for {
		select {
		case <-ctx.Done():
			log.Println("Subscription worker stopped")
			return
		case <-expireTicker.C:
			w.runExpire(ctx)
		case <-reminderTicker.C:
			w.runReminders(ctx)
		}
	}
}

// runExpire отмораживает истёкших юзеров (extra → frozen_extra, тариф = NULL)
// и разрывает их активные профили.
func (w *Worker) runExpire(ctx context.Context) {
	expired, err := w.users.ExpireSubscriptions(ctx)
	if err != nil {
		log.Printf("Subscription: ExpireSubscriptions: %v", err)
		return
	}
	if len(expired) == 0 {
		return
	}

	collector := w.holder.GetCollector()
	for _, uid := range expired {
		if collector != nil {
			collector.DisconnectUserAll(ctx, uid, "subscription expired")
		} else {
			// Нет коннекта к Xray — хотя бы снимем is_active в БД.
			// При ближайшем подъёме Xray syncUsersToXray не добавит их обратно.
			if _, err := w.profiles.DeactivateAllByUser(ctx, uid); err != nil {
				log.Printf("Subscription: DeactivateAllByUser %d: %v", uid, err)
			}
		}
		w.notifyBlock(ctx, uid)
	}
	log.Printf("Subscription: expired %d users", len(expired))
}

// notifyBlock шлёт юзеру email об истечении подписки. Идемпотентно:
// TryMarkBlockNotified защищает от повторной отправки, пока юзер не
// продлит подписку или не получит пополнение (что сбросит флаг).
//
// Порядок важен: сначала проверяем NotifyBlock, потом TryMark. Иначе если
// юзер выключил галку — флаг бы выставился вхолостую, и включение галки
// post-factum не разблокировало бы уведомление до следующего пополнения.
func (w *Worker) notifyBlock(ctx context.Context, userID int) {
	if w.mailer == nil {
		return
	}
	u, err := w.users.GetByID(ctx, userID)
	if err != nil {
		log.Printf("Subscription: notify block user=%d: %v", userID, err)
		return
	}
	if !u.NotifyBlock {
		log.Printf("Subscription: block mail skipped for user %d (notify_block=off)", userID)
		return
	}
	first, err := w.users.TryMarkBlockNotified(ctx, userID)
	if err != nil {
		log.Printf("Subscription: mark block user=%d: %v", userID, err)
		return
	}
	if !first {
		return
	}
	to, username := u.Email, u.Username
	w.mailer.Submit(fmt.Sprintf("subscription block user=%d", userID), func() error {
		return w.mailer.SendBlockNotification(to, username, "expired", w.baseURL)
	})
}

// runReminders шлёт напоминания за 5 и 1 день до окончания подписки.
// При отсутствии SMTP — no-op (сохраним state чистым: не помечаем как отправленное).
func (w *Worker) runReminders(ctx context.Context) {
	if w.mailer == nil {
		return
	}
	for _, days := range []int{5, 1} {
		list, err := w.users.UsersForReminder(ctx, days)
		if err != nil {
			log.Printf("Subscription: UsersForReminder(%d): %v", days, err)
			continue
		}
		for _, u := range list {
			if u.TariffExpiresAt == nil {
				continue
			}
			// Юзер отключил этот тип писем: пометим как «отправлено», чтобы
			// повторный тик не перебирал его заново — письма всё равно не будет.
			if !u.NotifyExpiration {
				if err := w.users.MarkReminderSent(ctx, u.ID, days); err != nil {
					log.Printf("Subscription: MarkReminderSent(skip) user=%d days=%d: %v", u.ID, days, err)
				}
				continue
			}
			if err := w.mailer.SendExpirationReminder(u.Email, u.Username, days, *u.TariffExpiresAt, w.baseURL); err != nil {
				log.Printf("Subscription: reminder email user=%d: %v", u.ID, err)
				continue
			}
			if err := w.users.MarkReminderSent(ctx, u.ID, days); err != nil {
				log.Printf("Subscription: MarkReminderSent user=%d days=%d: %v", u.ID, days, err)
			}
		}
		if len(list) > 0 {
			log.Printf("Subscription: sent %d-day reminders to %d users", days, len(list))
		}
	}
}
