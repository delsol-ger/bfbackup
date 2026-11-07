package notify

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// fakeSMTPS is a minimal SMTP server behind TLS that records one session.
type fakeSMTPS struct {
	ln    net.Listener
	host  string
	port  int
	roots *x509.CertPool
	got   chan smtpSession
}

type smtpSession struct {
	auth, from string
	to         []string
	data       string
}

func newFakeSMTPS(t *testing.T, password string) *fakeSMTPS {
	// Borrow httptest's self-signed certificate for 127.0.0.1.
	hs := httptest.NewUnstartedServer(nil)
	hs.StartTLS()
	cert, certs := hs.TLS.Certificates, hs.Certificate()
	hs.Close()
	roots := x509.NewCertPool()
	roots.AddCert(certs)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: cert})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &fakeSMTPS{ln: ln, roots: roots, got: make(chan smtpSession, 4)}
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	s.host = host
	s.port, _ = strconv.Atoi(port)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn, password)
		}
	}()
	return s
}

func (s *fakeSMTPS) serve(conn net.Conn, password string) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	say := func(l string) { conn.Write([]byte(l + "\r\n")) }
	var ses smtpSession
	say("220 fake ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		cmd := strings.ToUpper(strings.SplitN(line, " ", 2)[0])
		switch cmd {
		case "EHLO", "HELO":
			say("250-fake")
			say("250 AUTH PLAIN")
		case "AUTH":
			raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, "AUTH PLAIN "))
			ses.auth = string(raw)
			if !strings.HasSuffix(ses.auth, "\x00"+password) {
				say("535 bad credentials")
				continue
			}
			say("235 ok")
		case "MAIL":
			ses.from = line
			say("250 ok")
		case "RCPT":
			ses.to = append(ses.to, line)
			say("250 ok")
		case "DATA":
			say("354 go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			ses.data = b.String()
			say("250 queued")
		case "QUIT":
			say("221 bye")
			s.got <- ses
			return
		default:
			say("250 ok")
		}
	}
}

func TestEmail(t *testing.T) {
	srv := newFakeSMTPS(t, "secret-pw")
	e := Email{Host: srv.host, Port: srv.port, User: "bfb@example.com", Password: "secret-pw",
		To: []string{"a@example.com", "Bob <b@example.com>"}, TLS: &tls.Config{RootCAs: srv.roots}}
	err := e.Notify(context.Background(), Event{Title: "Backup fehlgeschlagen", Message: "line 1\nline 2 – ü", Priority: 1})
	if err != nil {
		t.Fatal(err)
	}
	ses := <-srv.got
	if ses.auth != "\x00bfb@example.com\x00secret-pw" || ses.from != "MAIL FROM:<bfb@example.com>" ||
		len(ses.to) != 2 || ses.to[1] != "RCPT TO:<b@example.com>" {
		t.Fatalf("session %+v", ses)
	}
	for _, want := range []string{"Subject: Backup fehlgeschlagen\r\n", "X-Priority: 1\r\n", "charset=utf-8", "line 1\r\nline 2 =E2=80=93 =C3=BC"} {
		if !strings.Contains(ses.data, want) {
			t.Fatalf("mail lacks %q:\n%s", want, ses.data)
		}
	}

	e.Password = "wrong-pw"
	if err := e.Notify(context.Background(), Event{}); err == nil || strings.Contains(err.Error(), "wrong-pw") {
		t.Fatalf("bad login: %v", err)
	}
	e.TLS = nil // self-signed certificate is not trusted
	e.Password = "secret-pw"
	if err := e.Notify(context.Background(), Event{}); err == nil {
		t.Fatal("untrusted certificate accepted")
	}
	if err := (Email{Host: "x"}).Notify(context.Background(), Event{}); err == nil {
		t.Fatal("missing recipient accepted")
	}
	if err := (Email{Host: "x", To: []string{"not an address"}}).Notify(context.Background(), Event{}); err == nil {
		t.Fatal("bad recipient accepted")
	}
}

type errNotifier struct{ err error }

func (n errNotifier) Notify(context.Context, Event) error { return n.err }

func TestMulti(t *testing.T) {
	a, b := errors.New("a failed"), errors.New("b failed")
	err := Multi{errNotifier{a}, errNotifier{nil}, errNotifier{b}}.Notify(context.Background(), Event{})
	if !errors.Is(err, a) || !errors.Is(err, b) {
		t.Fatalf("err = %v", err)
	}
	if err := (Multi{errNotifier{nil}}).Notify(context.Background(), Event{}); err != nil {
		t.Fatal(err)
	}
}
