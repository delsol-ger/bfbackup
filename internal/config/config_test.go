package config

import (
	"strings"
	"testing"

	"bfbackup/internal/proto"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestController(t *testing.T) {
	c, err := Load("", envOf(map[string]string{"BFB_ROLE": "controller", "BFB_ROOTS": "/mnt/user, /mnt/disks/", "BFB_UID": "99", "BFB_GID": "100"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.HTTPAddr != ":7700" || c.PeerAddr != ":7701" || c.DataDir != "/config" || c.HooksDir != "/config/hooks" {
		t.Fatalf("defaults %+v", c)
	}
	if strings.Join(c.Roots, ",") != "/mnt/user,/mnt/disks" || c.UID != 99 || c.GID != 100 {
		t.Fatalf("roots/ids %+v", c)
	}
	if c.Transport != TransportAuto {
		t.Fatalf("transport %q", c.Transport)
	}
	c, _ = Load("controller", envOf(map[string]string{"BFB_TRANSPORT": "TLS"}))
	if c.Transport != TransportTLS {
		t.Fatalf("transport %q", c.Transport)
	}
}

func TestPeer(t *testing.T) {
	tok := proto.NewToken().String()
	c, err := Load("peer", envOf(map[string]string{"BFB_CONTROLLER": "192.168.1.10", "BFB_TOKEN": tok}))
	if err != nil {
		t.Fatal(err)
	}
	if c.ControllerAddr != "192.168.1.10:7701" || c.UID != -1 || c.Transport != TransportTLS {
		t.Fatalf("%+v", c)
	}
	c, _ = Load("peer", envOf(map[string]string{"BFB_CONTROLLER": "x", "BFB_TOKEN": tok, "BFB_TRANSPORT": "plain"}))
	if c.Transport != TransportPlain {
		t.Fatalf("transport %q", c.Transport)
	}
	c, _ = Load("peer", envOf(map[string]string{"BFB_CONTROLLER": "tower:9000", "BFB_TOKEN": tok}))
	if c.ControllerAddr != "tower:9000" {
		t.Fatal(c.ControllerAddr)
	}
}

func TestErrors(t *testing.T) {
	tok := proto.NewToken().String()
	cases := []struct {
		role string
		env  map[string]string
		want string
	}{
		{"", nil, "role"},
		{"peer", map[string]string{"BFB_TOKEN": tok}, "BFB_CONTROLLER"},
		{"peer", map[string]string{"BFB_CONTROLLER": "x"}, "BFB_TOKEN"},
		{"peer", map[string]string{"BFB_CONTROLLER": "x", "BFB_TOKEN": "nope"}, "BFB_TOKEN"},
		{"controller", map[string]string{"BFB_ROOTS": "relative"}, "absolute"},
		{"controller", map[string]string{"BFB_UID": "99"}, "both"},
		{"controller", map[string]string{"BFB_UID": "-5", "BFB_GID": "1"}, "BFB_UID"},
		{"controller", map[string]string{"BFB_LOG_LEVEL": "loud"}, "LOG_LEVEL"},
		{"controller", map[string]string{"BFB_TRANSPORT": "plain"}, "BFB_TRANSPORT"},
		{"peer", map[string]string{"BFB_CONTROLLER": "x", "BFB_TOKEN": tok, "BFB_TRANSPORT": "auto"}, "BFB_TRANSPORT"},
	}
	for _, c := range cases {
		_, err := Load(c.role, envOf(c.env))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: err = %v, want %q", c.env, err, c.want)
		}
	}
}
