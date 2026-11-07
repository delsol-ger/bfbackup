package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// DefaultSMTPPort is the SMTPS port (TLS from the first byte).
const DefaultSMTPPort = 465

// Email sends events by SMTP over implicit TLS (SMTPS). STARTTLS and
// plain connections are not supported.
type Email struct {
	Host     string
	Port     int // defaults to DefaultSMTPPort
	User     string
	Password string // used with User; no login when User is empty
	From     string // defaults to User
	To       []string
	TLS      *tls.Config // tests override it
}

// Notify sends one mail to all recipients.
func (e Email) Notify(ctx context.Context, ev Event) error {
	if e.Host == "" || len(e.To) == 0 {
		return errors.New("email: SMTP server and recipient are required")
	}
	from := e.From
	if from == "" {
		from = e.User
	}
	fromAddr, err := mail.ParseAddress(from)
	if err != nil {
		return fmt.Errorf("email: sender %q: %v", from, err)
	}
	var to []string
	for _, t := range e.To {
		a, err := mail.ParseAddress(t)
		if err != nil {
			return fmt.Errorf("email: recipient %q: %v", t, err)
		}
		to = append(to, a.Address)
	}
	port := e.Port
	if port == 0 {
		port = DefaultSMTPPort
	}
	cfg := e.TLS
	if cfg == nil {
		cfg = &tls.Config{}
	}
	cfg = cfg.Clone()
	if cfg.ServerName == "" {
		cfg.ServerName = e.Host
	}
	if err := e.send(ctx, net.JoinHostPort(e.Host, strconv.Itoa(port)), cfg, fromAddr, to, ev); err != nil {
		return errors.New("email: " + scrub(err.Error(), e.Password))
	}
	return nil
}

func (e Email) send(ctx context.Context, addr string, cfg *tls.Config, from *mail.Address, to []string, ev Event) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}
	d := tls.Dialer{NetDialer: &net.Dialer{}, Config: cfg}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	dl, _ := ctx.Deadline()
	conn.SetDeadline(dl)
	c, err := smtp.NewClient(conn, cfg.ServerName)
	if err != nil {
		return err
	}
	defer c.Close()
	if e.User != "" {
		if ok, _ := c.Extension("AUTH"); !ok {
			return errors.New("the server does not offer a login (AUTH)")
		}
		if err := c.Auth(smtp.PlainAuth("", e.User, e.Password, cfg.ServerName)); err != nil {
			return fmt.Errorf("login: %v", err)
		}
	}
	if err := c.Mail(from.Address); err != nil {
		return fmt.Errorf("sender: %v", err)
	}
	for _, t := range to {
		if err := c.Rcpt(t); err != nil {
			return fmt.Errorf("recipient %s: %v", t, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(message(from, to, ev)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// message builds the mail: UTF-8 plain text, quoted-printable.
func message(from *mail.Address, to []string, ev Event) []byte {
	var b bytes.Buffer
	hdr := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	hdr("From", from.String())
	hdr("To", strings.Join(to, ", "))
	hdr("Subject", mime.QEncoding.Encode("utf-8", oneLine(ev.Title)))
	hdr("Date", time.Now().Format(time.RFC1123Z))
	var id [12]byte
	rand.Read(id[:])
	domain := "bfbackup"
	if i := strings.LastIndexByte(from.Address, '@'); i >= 0 {
		domain = from.Address[i+1:]
	}
	hdr("Message-ID", fmt.Sprintf("<%x@%s>", id, domain))
	if ev.Priority >= 1 {
		hdr("X-Priority", "1")
		hdr("Importance", "high")
	}
	hdr("MIME-Version", "1.0")
	hdr("Content-Type", "text/plain; charset=utf-8")
	hdr("Content-Transfer-Encoding", "quoted-printable")
	b.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&b)
	qp.Write([]byte(strings.ReplaceAll(ev.Message, "\n", "\r\n") + "\r\n"))
	qp.Close()
	return b.Bytes()
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// Multi sends every event through all its notifiers.
type Multi []Notifier

// Notify tries every notifier and joins their errors.
func (m Multi) Notify(ctx context.Context, ev Event) error {
	var errs []error
	for _, n := range m {
		if err := n.Notify(ctx, ev); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
