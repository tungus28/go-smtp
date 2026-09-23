package main

import (
	"crypto/tls"
	"io"
	"log"
	"net/smtp"
	"net/http"
	"time"
	"os"
	_ "net/http/pprof"

	gosmtp "github.com/emersion/go-smtp" 
)


// Вместо констант используем функцию, которая читает переменные окружения
func getYandexCredentials() (string, string) {
	user := os.Getenv("YANDEX_USER")
	pass := os.Getenv("YANDEX_PASSWORD")

	if user == "" || pass == "" {
		log.Fatal("Критические переменные окружения YANDEX_USER или YANDEX_PASSWORD не заданы!")
	}
	return user, pass
}

type Backend struct{}

func (bkd *Backend) NewSession(c *gosmtp.Conn) (gosmtp.Session, error) {
	return &ProxySession{}, nil
}

type ProxySession struct {
	From string
	To   []string
}

func (s *ProxySession) AuthPlain(username, password string) error {
	return nil // Пропускаем локальную авторизацию для простоты теста
}

func (s *ProxySession) Mail(from string, opts *gosmtp.MailOptions) error {
	s.From = from
	return nil
}

func (s *ProxySession) Rcpt(to string, opts *gosmtp.RcptOptions) error {
	s.To = append(s.To, to)
	return nil
}

// Data принимает тело письма и пересылает его на Яндекс
func (s *ProxySession) Data(r io.Reader) error {
	yandexUser, yandexPass := getYandexCredentials()
	yandexSMTP := "smtp.yandex.ru:465"
	// Читаем все тело письма в память
	msgBytes, err := io.ReadAll(r)
	if err != nil {
		log.Printf("Ошибка чтения письма: %v", err)
		return err
	}

	// 1. Подключаемся к Яндексу через явный TLS (порт 465)
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

	// 2. Аутентификация на Яндексе
	auth := smtp.PlainAuth("", yandexUser, yandexPass, "smtp.yandex.ru")
	if err = client.Auth(auth); err != nil {
		log.Printf("Ошибка авторизации на Яндексе: %v", err)
		return err
	}

	// 3. Установка отправителя (Яндекс требует, чтобы From совпадал с логином)
	if err = client.Mail(yandexUser); err != nil {
		log.Printf("Ошибка команды MAIL FROM: %v", err)
		return err
	}

	// 4. Установка получателей (пересылаем всем, кого указал клиент)
	for _, k := range s.To {
		if err = client.Rcpt(k); err != nil {
			log.Printf("Ошибка команды RCPT TO для %s: %v", k, err)
			return err
		}
	}

	// 5. Отправка тела письма
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
	// Запуск pprof на отдельном порту (например, 6060) в отдельной горутине
	go func() {
		log.Println("Запуск pprof-сервера на порту :6060")
		if err := http.ListenAndServe(":6060", nil); err != nil {
			log.Printf("Ошибка запуска pprof: %v", err)
		}
	}()
	be := &Backend{}
	s := gosmtp.NewServer(be)

	s.Addr = ":7070" // Слушаем порт 7070 на всех интерфейсах
	s.Domain = "localhost"
	s.ReadTimeout = 30 * time.Second
	s.WriteTimeout = 30 * time.Second
	s.AllowInsecureAuth = true

	log.Printf("SMTP-прокси запущен на порту %s...", s.Addr)
	if err := s.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
