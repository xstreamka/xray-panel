package handlers

import (
	"log"
	"net"
	"net/http"
	"strings"
	"unicode/utf8"

	"xray-panel/internal/email"
	"xray-panel/internal/middleware"
)

// Лимиты сообщения обратной связи — чтобы нельзя было заспамить админский ящик
// гигантским текстом. Считаем в символах (рунах), не в байтах: честнее к юзеру,
// пишущему на кириллице (1 русская буква = 2 байта в UTF-8).
const (
	feedbackMaxSubjectRunes = 200
	feedbackMaxMessageRunes = 5000
	// feedbackMaxBodyBytes — жёсткий транспортный лимит на тело POST-запроса,
	// ставится через MaxBytesReader ДО парсинга формы. Без него юзер мог бы
	// прислать 100 МБ и сервер честно прочитал бы всё в память перед проверкой
	// длины полей. Берём запас над max-runes × 4 байта/руна + оверхед формы.
	feedbackMaxBodyBytes int64 = 64 * 1024
)

type FeedbackHandler struct {
	renderer *Renderer
	mailer   *email.Sender
	to       string
	limiter  *middleware.RateLimiter
}

func NewFeedbackHandler(renderer *Renderer, mailer *email.Sender, to string, limiter *middleware.RateLimiter) *FeedbackHandler {
	return &FeedbackHandler{renderer: renderer, mailer: mailer, to: to, limiter: limiter}
}

// Index — GET /feedback — форма обратной связи.
// ?sent=1 приходит после успешного POST (Post/Redirect/Get): нужен, чтобы
// рефреш страницы не отправлял письмо повторно.
func (h *FeedbackHandler) Index(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())
	var success string
	if r.URL.Query().Get("sent") == "1" {
		success = "Спасибо! Сообщение отправлено, мы ответим на ваш email."
	}
	h.renderer.Render(w, r, "feedback.html", map[string]any{
		"Active":  "feedback",
		"User":    user,
		"Success": success,
	})
}

// Send — POST /feedback — валидирует и отправляет письмо админу.
// CR/LF в теме вырезаем, чтобы отправитель не мог через форму вставить
// произвольные SMTP-заголовки (header injection).
func (h *FeedbackHandler) Send(w http.ResponseWriter, r *http.Request) {
	user := middleware.UserFromContext(r.Context())

	// Жёсткий лимит на размер тела запроса ДО ParseForm/FormValue: иначе
	// r.FormValue сначала прочитает в память сколько бы юзер ни прислал,
	// и только потом мы проверим длину — классический DoS-вектор.
	r.Body = http.MaxBytesReader(w, r.Body, feedbackMaxBodyBytes)
	if err := r.ParseForm(); err != nil {
		h.render(w, r, user, "", "", "Сообщение слишком большое. Сократите текст.", "")
		return
	}

	subject := sanitizeHeader(strings.TrimSpace(r.FormValue("subject")))
	message := strings.TrimSpace(r.FormValue("message"))

	if subject == "" || message == "" {
		h.render(w, r, user, subject, message, "Заполните тему и текст сообщения.", "")
		return
	}
	if utf8.RuneCountInString(subject) > feedbackMaxSubjectRunes {
		h.render(w, r, user, subject, message, "Тема слишком длинная (максимум 200 символов).", "")
		return
	}
	if utf8.RuneCountInString(message) > feedbackMaxMessageRunes {
		h.render(w, r, user, subject, message, "Сообщение слишком длинное (максимум 5000 символов).", "")
		return
	}

	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip == "" {
		ip = r.RemoteAddr
	}

	// Rate-limit по user.ID — защита от флуда/спама через форму.
	if h.limiter != nil && !h.limiter.Allow(ip) {
		h.render(w, r, user, subject, message, "Слишком много сообщений подряд. Попробуйте позже.", "")
		return
	}

	if h.mailer == nil || h.to == "" {
		log.Printf("Feedback: mailer not configured, message from user=%d lost: %s", user.ID, subject)
		h.render(w, r, user, subject, message, "Отправка почты временно недоступна. Напишите позже.", "")
		return
	}

	if err := h.mailer.SendFeedback(h.to, user.Email, user.Username, ip, subject, message); err != nil {
		log.Printf("Feedback: send error user=%d: %v", user.ID, err)
		h.render(w, r, user, subject, message, "Не удалось отправить сообщение. Попробуйте позже.", "")
		return
	}

	log.Printf("Feedback: sent from user=%d (%s): %s", user.ID, user.Email, subject)
	// Post/Redirect/Get: после успешной отправки редиректим на GET, чтобы
	// рефреш страницы не повторял submit и не слал второе письмо.
	http.Redirect(w, r, "/feedback?sent=1", http.StatusSeeOther)
}

func (h *FeedbackHandler) render(w http.ResponseWriter, r *http.Request, user any, subject, message, errMsg, successMsg string) {
	h.renderer.Render(w, r, "feedback.html", map[string]any{
		"Active":  "feedback",
		"User":    user,
		"Subject": subject,
		"Message": message,
		"Error":   errMsg,
		"Success": successMsg,
	})
}

// sanitizeHeader убирает CR/LF из строки, идущей в SMTP-заголовок Subject,
// чтобы юзер не мог инжектить дополнительные заголовки (BCC, Content-Type и т.п.).
func sanitizeHeader(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}
