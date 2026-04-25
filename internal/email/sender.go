package email

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/smtp"
	"strings"
	"time"
)

type Sender struct {
	Host     string // smtp.protonmail.ch
	Port     string // 465 (SSL/TLS) или 587 (STARTTLS)
	User     string // mail@xstreamka.dev
	Password string
	From     string // "XStreamka Dev <mail@xstreamka.dev>"
}

func NewSender(host, port, user, password, from string) *Sender {
	return &Sender{
		Host:     host,
		Port:     port,
		User:     user,
		Password: password,
		From:     from,
	}
}

func (s *Sender) SendVerification(to, token, baseURL string) error {
	verifyURL := fmt.Sprintf("%s/verify?token=%s", strings.TrimRight(baseURL, "/"), token)

	subject := "Подтверждение email — XS VPN"
	body := fmt.Sprintf(`Привет!

Для подтверждения email перейдите по ссылке:

%s

Ссылка действует 24 часа.

Если вы не регистрировались — проигнорируйте это письмо.

—
XS VPN`, verifyURL)

	return s.send(to, subject, body)
}

// SendPasswordReset — письмо со ссылкой для восстановления пароля.
// Ссылка живёт 1 час (см. UserStore.CreateResetToken).
func (s *Sender) SendPasswordReset(to, token, baseURL string) error {
	resetURL := fmt.Sprintf("%s/reset?token=%s", strings.TrimRight(baseURL, "/"), token)

	subject := "Восстановление пароля — XS VPN"
	body := fmt.Sprintf(`Привет!

Кто-то запросил восстановление пароля для вашего аккаунта.

Чтобы задать новый пароль, перейдите по ссылке (действует 1 час):

%s

Если запрос отправляли не вы — проигнорируйте это письмо, пароль не изменится.

—
XS VPN`, resetURL)

	return s.send(to, subject, body)
}

// SendTopupNotification — уведомление о зачислении баланса после оплаты.
func (s *Sender) SendTopupNotification(to, username string, trafficGB, amountRub float64, invID int, baseURL string) error {
	dashURL := strings.TrimRight(baseURL, "/") + "/dashboard"
	settingsURL := strings.TrimRight(baseURL, "/") + "/settings"

	subject := fmt.Sprintf("Баланс пополнен на %.1f ГБ — XS VPN", trafficGB)
	body := fmt.Sprintf(`Привет, %s!

Оплата получена, баланс пополнен.

Заказ:      №%d
Сумма:      %.2f ₽
Зачислено:  %.1f ГБ

Создать профиль или посмотреть баланс:
%s

Если оплату совершали не вы — ответьте на это письмо.

Отключить такие уведомления можно в настройках:
%s

—
XS VPN`, username, invID, amountRub, trafficGB, dashURL, settingsURL)

	return s.send(to, subject, body)
}

// SendExpirationReminder — напоминание о скором истечении подписки.
// daysLeft — за сколько дней шлём (5 или 1), expiresAt — точная дата окончания.
func (s *Sender) SendExpirationReminder(to, username string, daysLeft int, expiresAt time.Time, baseURL string) error {
	payURL := strings.TrimRight(baseURL, "/") + "/pay"
	settingsURL := strings.TrimRight(baseURL, "/") + "/settings"

	var subject, when string
	switch daysLeft {
	case 1:
		subject = "Подписка закончится завтра — XS VPN"
		when = "завтра"
	default:
		subject = fmt.Sprintf("Подписка закончится через %d дней — XS VPN", daysLeft)
		when = fmt.Sprintf("через %d дней", daysLeft)
	}

	body := fmt.Sprintf(`Привет, %s!

Ваша подписка истекает %s (%s).

Чтобы не потерять доступ к VPN, продлите подписку заранее:
%s

Отключить такие уведомления можно в настройках:
%s

—
XS VPN`, username, when, expiresAt.Format("02.01.2006 15:04"), payURL, settingsURL)

	return s.send(to, subject, body)
}

// SendTrafficLowNotification — предупреждение «скоро закончится трафик».
// remainingBytes — сколько осталось на сумме base+extra; отправляется один раз,
// пока юзер не пополнит баланс (флаг сбросит), чтобы не спамить каждым тиком.
func (s *Sender) SendTrafficLowNotification(to, username string, remainingBytes int64, baseURL string) error {
	payURL := strings.TrimRight(baseURL, "/") + "/pay"
	settingsURL := strings.TrimRight(baseURL, "/") + "/settings"
	subject := "Скоро закончится трафик — XS VPN"
	body := fmt.Sprintf(`Привет, %s!

У вас осталось %s доступного трафика. Когда баланс обнулится, все VPN-профили
будут отключены автоматически.

Чтобы не потерять доступ — докупите трафик или оформите новую подписку:
%s

Отключить такие уведомления можно в настройках:
%s

—
XS VPN`, username, formatBytes(remainingBytes), payURL, settingsURL)

	return s.send(to, subject, body)
}

// formatBytes — компактный перевод байт в KB/MB/GB. Дублируем локально,
// чтобы не тянуть в пакет email зависимость от handlers/.
func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %s", float64(b)/float64(div), []string{"KB", "MB", "GB", "TB"}[exp])
}

// SendBlockNotification — уведомление об отключении VPN-профилей.
// reason: "balance" (исчерпан трафик) или "expired" (истёк срок подписки).
func (s *Sender) SendBlockNotification(to, username, reason, baseURL string) error {
	payURL := strings.TrimRight(baseURL, "/") + "/pay"
	settingsURL := strings.TrimRight(baseURL, "/") + "/settings"

	var subject, body string
	switch reason {
	case "expired":
		subject = "Подписка истекла — VPN отключён"
		body = fmt.Sprintf(`Привет, %s!

Срок вашей подписки истёк, VPN-профили отключены.

Докупленный трафик сохранён и вернётся при следующей оплате подписки.

Чтобы восстановить доступ — оформите подписку заново:
%s

Отключить такие уведомления можно в настройках:
%s

—
XS VPN`, username, payURL, settingsURL)

	default: // "balance"
		subject = "Трафик закончился — VPN отключён"
		body = fmt.Sprintf(`Привет, %s!

Базовый трафик подписки исчерпан и весь докупленный тоже, VPN-профили отключены.

Чтобы восстановить доступ — докупите трафик или оформите новый тариф:
%s

Отключить такие уведомления можно в настройках:
%s

—
XS VPN`, username, payURL, settingsURL)
	}

	return s.send(to, subject, body)
}

// SendProfileLimitNotification — письмо «исчерпан personal-лимит профиля».
// Отличается от SendBlockNotification тем, что отключается один конкретный
// профиль (по лимиту, который юзер сам на него выставил), а не вся учётка.
// Остальные профили продолжают работать, общая подписка жива.
func (s *Sender) SendProfileLimitNotification(to, username, profileName string, limitBytes int64, baseURL string) error {
	dashURL := strings.TrimRight(baseURL, "/") + "/dashboard"
	settingsURL := strings.TrimRight(baseURL, "/") + "/settings"
	subject := fmt.Sprintf("Лимит профиля «%s» исчерпан — XS VPN", profileName)
	body := fmt.Sprintf(`Привет, %s!

На профиле «%s» был установлен лимит %s, и он исчерпан. Профиль отключён.
Остальные ваши профили и подписка продолжают работать.

Чтобы снова включить профиль — увеличьте лимит или сбросьте счётчик трафика
в дашборде:
%s

Отключить такие уведомления можно в настройках:
%s

—
XS VPN`, username, profileName, formatBytes(limitBytes), dashURL, settingsURL)

	return s.send(to, subject, body)
}

// SendFeedback — письмо от юзера с формы обратной связи на админский ящик.
// subject уже очищен от CR/LF на стороне хендлера (защита от header injection).
// Reply-To ставим на email юзера, чтобы можно было ответить прямо из почтовика.
func (s *Sender) SendFeedback(to, fromUserEmail, fromUsername, userIP, subject, message string) error {
	fullSubject := "[Feedback] " + subject
	body := fmt.Sprintf(`Новое сообщение с формы обратной связи.

Пользователь: %s
Email:        %s
IP:           %s

Тема: %s

%s

—
XS VPN`, fromUsername, fromUserEmail, userIP, subject, message)

	return s.sendWithReplyTo(to, fullSubject, body, fromUserEmail)
}

// sendWithReplyTo — вариант send с заголовком Reply-To, чтобы ответ админа
// из почтовика уходил отправителю формы, а не обратно на SMTP_FROM.
func (s *Sender) sendWithReplyTo(to, subject, body, replyTo string) error {
	msg := fmt.Sprintf(
		"From: %s\r\nTo: %s\r\nReply-To: %s\r\nSubject: %s\r\nContent-Type: text/plain; charset=utf-8\r\nMIME-Version: 1.0\r\n\r\n%s",
		s.From, to, replyTo, subject, body,
	)

	addr := net.JoinHostPort(s.Host, s.Port)
	auth := smtp.PlainAuth("", s.User, s.Password, s.Host)

	var err error
	if s.Port == "465" {
		err = s.sendSSL(addr, auth, to, []byte(msg))
	} else {
		err = smtp.SendMail(addr, auth, s.User, []string{to}, []byte(msg))
	}

	if err != nil {
		log.Printf("Email send error to %s: %v", to, err)
		return fmt.Errorf("send email: %w", err)
	}

	log.Printf("Email sent to %s: %s", to, subject)
	return nil
}

func (s *Sender) send(to, subject, body string) error {
	msg := fmt.Sprintf(
		"From: %s\r\nTo: %s\r\nSubject: %s\r\nContent-Type: text/plain; charset=utf-8\r\nMIME-Version: 1.0\r\n\r\n%s",
		s.From, to, subject, body,
	)

	addr := net.JoinHostPort(s.Host, s.Port)
	auth := smtp.PlainAuth("", s.User, s.Password, s.Host)

	var err error
	if s.Port == "465" {
		err = s.sendSSL(addr, auth, to, []byte(msg))
	} else {
		// Порт 587 (STARTTLS) или 25 (plain)
		err = smtp.SendMail(addr, auth, s.User, []string{to}, []byte(msg))
	}

	if err != nil {
		log.Printf("Email send error to %s: %v", to, err)
		return fmt.Errorf("send email: %w", err)
	}

	log.Printf("Email sent to %s: %s", to, subject)
	return nil
}

// sendSSL — отправка через порт 465 (implicit TLS / SMTPS)
func (s *Sender) sendSSL(addr string, auth smtp.Auth, to string, msg []byte) error {
	tlsConfig := &tls.Config{
		ServerName:         s.Host,
		InsecureSkipVerify: true, // хостинговые почтовики часто имеют невалидный CN/SAN
	}

	// Устанавливаем TLS-соединение напрямую (не STARTTLS)
	conn, err := tls.Dial("tcp", addr, tlsConfig)
	if err != nil {
		return fmt.Errorf("tls dial: %w", err)
	}
	defer conn.Close()

	client, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		return fmt.Errorf("smtp client: %w", err)
	}
	defer client.Close()

	if err := client.Auth(auth); err != nil {
		return fmt.Errorf("auth: %w", err)
	}

	if err := client.Mail(s.User); err != nil {
		return fmt.Errorf("mail from: %w", err)
	}

	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("rcpt to: %w", err)
	}

	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("data: %w", err)
	}

	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("write: %w", err)
	}

	if err := w.Close(); err != nil {
		return fmt.Errorf("close data: %w", err)
	}

	return client.Quit()
}
