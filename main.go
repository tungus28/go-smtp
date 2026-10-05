package main

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"log"

	//_ "net/http/pprof"
	"net/smtp"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-sasl"
	gosmtp "github.com/emersion/go-smtp"
	"golang.org/x/time/rate"
)

type bucket struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

type EmailRateLimiter struct {
	buckets sync.Map
	rate    rate.Limit
	burst   int
	ttl     time.Duration
}

func NewEmailRateLimiter(r rate.Limit, b int, ttl time.Duration) *EmailRateLimiter {
	limiter := &EmailRateLimiter{
		rate:  r,
		burst: b,
		ttl:   ttl,
	}
	go limiter.startCleanupWorker(120 * time.Minute)
	return limiter
}

func (el *EmailRateLimiter) hashEmail(email string) string {
	cleanEmail := strings.ToLower(strings.TrimSpace(email))
	hash := sha256.Sum256([]byte(cleanEmail))
	return hex.EncodeToString(hash[:])
}

func (el *EmailRateLimiter) Allow(email string) bool {
	if email == "" {
		return false
	}
	key := el.hashEmail(email)
	now := time.Now()

	if v, exists := el.buckets.Load(key); exists {
		b := v.(*bucket)
		b.lastSeen = now
		return b.limiter.Allow()
	}

	newBucket := &bucket{
		limiter:  rate.NewLimiter(el.rate, el.burst),
		lastSeen: now,
	}

	actual, _ := el.buckets.LoadOrStore(key, newBucket)
	b := actual.(*bucket)
	b.lastSeen = now
	return b.limiter.Allow()
}

func (el *EmailRateLimiter) startCleanupWorker(interval time.Duration) {
	ticker := time.NewTicker(interval)
	for range ticker.C {
		now := time.Now()
		el.buckets.Range(func(key, value any) bool {
			b := value.(*bucket)
			if now.Sub(b.lastSeen) > el.ttl {
				el.buckets.Delete(key)
			}
			return true
		})
	}
}

func getYandexCredentials() (string, string) {
	user := os.Getenv("YANDEX_USER")
	pass := os.Getenv("YANDEX_PASSWORD")
	if user == "" || pass == "" {
		log.Fatal("Критические переменные окружения YANDEX_USER или YANDEX_PASSWORD не заданы!")
	}
	return user, pass
}

// === СТРУКТУРА BACKEND ===

type Backend struct {
	Limiter *EmailRateLimiter
}

func (bkd *Backend) NewSession(c *gosmtp.Conn) (gosmtp.Session, error) {
	return &ProxySession{Limiter: bkd.Limiter, Authenticated: false}, nil
}

func (bkd *Backend) LoginMechanisms() []string {
	return []string{sasl.Plain}
}

// Метод Auth теперь возвращает ProxySession, так как она реализует и Session, и AuthSession
func (bkd *Backend) Auth(mech string) (gosmtp.AuthSession, error) {
	if mech != sasl.Plain {
		return nil, gosmtp.ErrAuthUnsupported
	}
	return &ProxySession{Limiter: bkd.Limiter, Authenticated: false}, nil
}

// === ЕДИНАЯ СТРУКТУРА СЕССИИ (РЕАЛИЗУЕТ ВСЕ МЕТОДЫ) ===

type ProxySession struct {
	Limiter       *EmailRateLimiter
	From          string
	To            []string
	Authenticated bool
}

// AuthMechanisms — обязательный метод интерфейса AuthSession
func (s *ProxySession) AuthMechanisms() []string {
	return []string{sasl.Plain}
}

// Auth обрабатывает валидацию данных SASL PLAIN
func (s *ProxySession) Auth(mech string) (sasl.Server, error) {
	return sasl.NewPlainServer(func(identity, username, password string) error {
		yandexUser, yandexPass := getYandexCredentials()

		cleanUsername := strings.ToLower(strings.TrimSpace(username))
		cleanYandexUser := strings.ToLower(strings.TrimSpace(yandexUser))

		if cleanUsername == cleanYandexUser && password == yandexPass {
			s.Authenticated = true // Поднимаем флаг внутри текущей сессии
			return nil
		}

		log.Printf("[БЕЗОПАСНОСТЬ] Ошибка входа! Неверный пароль для пользователя: %s\n", username)
		return errors.New("Authentication failed. Invalid username or password.")
	}), nil
}

func (s *ProxySession) AuthPlain(username, password string) error {
	return nil
}

func (s *ProxySession) Mail(from string, opts *gosmtp.MailOptions) error {
	if !s.Authenticated {
		log.Printf("[БЕЗОПАСНОСТЬ] Блокировка анонимного запроса MAIL FROM от %s\n", from)
		return &gosmtp.SMTPError{
			Code:         530,
			EnhancedCode: gosmtp.EnhancedCode{5, 7, 0},
			Message:      "Authentication required. Please authenticate first.",
		}
	}
	s.From = from
	return nil
}

func (s *ProxySession) Rcpt(to string, opts *gosmtp.RcptOptions) error {
	userEmail := to
	allowed := s.Limiter.Allow(userEmail)

	if allowed {
		log.Printf("[%s] Запрос для %s: Разрешен\n", time.Now().Format("15:04:05"), userEmail)
		s.To = append(s.To, to)
		return nil
	}

	log.Printf("[%s] Запрос для %s: ЗАБЛОКИРОВАН (Превышен лимит писем для адреса!)\n", time.Now().Format("15:04:05"), userEmail)
	return &gosmtp.SMTPError{
		Code:         550,
		EnhancedCode: gosmtp.EnhancedCode{5, 7, 1},
		Message:      "Rate limit exceeded for this recipient. Max 50 emails per hour per mailbox.",
	}
}

func (s *ProxySession) Data(r io.Reader) error {
	yandexUser, yandexPass := getYandexCredentials()
	yandexSMTP := "smtp.yandex.ru:465"

	msgBytes, err := io.ReadAll(r)
	if err != nil {
		log.Printf("Ошибка чтения письма: %v", err)
		return err
	}

	if len(s.To) == 0 {
		return &gosmtp.SMTPError{
			Code:         554,
			EnhancedCode: gosmtp.EnhancedCode{5, 5, 1},
			Message:      "No valid recipients specified.",
		}
	}

	tlsConfig := &tls.Config{
		InsecureSkipVerify: false,
		ServerName:         "smtp.yandex.ru",
	}

	conn, err := tls.Dial("tcp", yandexSMTP, tlsConfig)
	if err != nil {
		log.Printf("Ошибка TLS-подключения к Яндексу: %v", err)
		return err
	}
	defer conn.Close()

	client, err := smtp.NewClient(conn, "smtp.yandex.ru")
	if err != nil {
		log.Printf("Ошибка создания SMTP-клиента: %v", err)
		return err
	}
	defer client.Quit()

	auth := smtp.PlainAuth("", yandexUser, yandexPass, "smtp.yandex.ru")
	if err = client.Auth(auth); err != nil {
		log.Printf("Ошибка авторизации на Яндексе: %v", err)
		return err
	}

	if err = client.Mail(yandexUser); err != nil {
		log.Printf("Ошибка команды MAIL FROM: %v", err)
		return err
	}

	for _, k := range s.To {
		if err = client.Rcpt(k); err != nil {
			log.Printf("Ошибка команды RCPT TO для %s: %v", k, err)
			return err
		}
	}

	w, err := client.Data()
	if err != nil {
		log.Printf("Ошибка команды DATA: %v", err)
		return err
	}

	_, err = w.Write(msgBytes)
	if err != nil {
		log.Printf("Ошибка записи тела письма: %v", err)
		return err
	}

	err = w.Close()
	if err != nil {
		log.Printf("Ошибка закрытия потока DATA: %v", err)
		return err
	}

	log.Printf("Письмо успешно проксировано на Яндекс для: %v", s.To)
	return nil
}

func (s *ProxySession) Reset() {
	s.From = ""
	s.To = nil
}

func (s *ProxySession) Logout() error { return nil }

func main() {
	/*go func() {
		log.Println("Запуск pprof-сервера на порту :6060")
		if err := http.ListenAndServe(":6060", nil); err != nil {
			log.Printf("Ошибка запуска pprof: %v", err)
		}
	}()*/

	hourlyLimit := rate.Every(60 * time.Minute / 50)
	limiter := NewEmailRateLimiter(hourlyLimit, 50, 90*time.Minute)

	be := &Backend{
		Limiter: limiter,
	}

	s := gosmtp.NewServer(be)
	s.Addr = ":7070"
	s.Domain = "localhost"
	s.ReadTimeout = 30 * time.Second
	s.WriteTimeout = 30 * time.Second
	s.AllowInsecureAuth = true

	log.Printf("SMTP-прокси запущен на порту %s...", s.Addr)
	if err := s.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
