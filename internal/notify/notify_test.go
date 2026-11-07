package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPushover(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		got = map[string]string{}
		for k := range r.PostForm {
			got[k] = r.PostForm.Get(k)
		}
		if r.PostForm.Get("token") == "bad" {
			w.WriteHeader(400)
			w.Write([]byte(`{"status":0,"errors":["application token is invalid"]}`))
			return
		}
		w.Write([]byte(`{"status":1}`))
	}))
	defer srv.Close()

	p := Pushover{Token: "tok", User: "usr", Endpoint: srv.URL}
	err := p.Notify(context.Background(), Event{Title: "t", Message: strings.Repeat("m", 2000), Priority: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got["token"] != "tok" || got["user"] != "usr" || got["priority"] != "1" || len(got["message"]) != 1024 {
		t.Fatalf("form %v", got)
	}

	p.Token = "bad"
	if err := p.Notify(context.Background(), Event{}); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("err = %v", err)
	}
	if err := (Pushover{}).Notify(context.Background(), Event{}); err == nil {
		t.Fatal("missing credentials accepted")
	}
	p = Pushover{Token: "secret-token", User: "u", Endpoint: "http://127.0.0.1:1/?x"}
	if err := p.Notify(context.Background(), Event{}); err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("err leaks token or is nil: %v", err)
	}
}
