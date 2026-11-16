package advertise

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"recall-miner/internal/config"
)

func TestResolve_MinerAPIURLWins(t *testing.T) {
	cfg := config.Config{MinerAPIURL: "http://miner.example.com:7101", ExternalIP: "203.0.113.10"}
	u, src, err := Resolve(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if u != "http://miner.example.com:7101" || src != "url" {
		t.Fatalf("got %q %q", u, src)
	}
}

func TestResolve_ExplicitIP(t *testing.T) {
	cfg := config.Config{ExternalIP: "203.0.113.10", ExternalPort: 7101, ListenAddr: ":8001"}
	u, src, err := Resolve(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if u != "http://203.0.113.10:7101" || src != "explicit" {
		t.Fatalf("got %q %q", u, src)
	}
}

func TestResolve_FailClosedProduction(t *testing.T) {
	cfg := config.Config{AdvertiseMode: "off", Environment: "production", ListenAddr: ":8001"}
	_, _, err := Resolve(cfg)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestResolve_DevDefault(t *testing.T) {
	cfg := config.Config{AdvertiseMode: "off", Environment: "dev", ListenAddr: ":8001"}
	u, src, err := Resolve(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if u != "http://127.0.0.1:8001" || src != "dev-default" {
		t.Fatalf("got %q %q", u, src)
	}
}

func TestResolve_AutoAllowlist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("203.0.113.50"))
	}))
	defer srv.Close()
	old := HTTPSEchoAllowlist[SourceE1]
	HTTPSEchoAllowlist[SourceE1] = srv.URL
	defer func() { HTTPSEchoAllowlist[SourceE1] = old }()
	cfg := config.Config{AdvertiseMode: "auto", AdvertiseSource: SourceE1, ExternalPort: 7102, AdvertiseRequirePublicSet: true, AdvertiseRequirePublic: false, AllowPrivateAdvertise: true}
	u, src, err := Resolve(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if u != "http://203.0.113.50:7102" || src != "auto:E1" {
		t.Fatalf("got %q %q", u, src)
	}
}

func TestResolve_AutoFailClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	old := HTTPSEchoAllowlist[SourceE1]
	HTTPSEchoAllowlist[SourceE1] = srv.URL
	defer func() { HTTPSEchoAllowlist[SourceE1] = old }()
	cfg := config.Config{AdvertiseMode: "auto", AdvertiseSource: SourceE1}
	_, _, err := Resolve(cfg)
	if err == nil {
		t.Fatal("expected fail-closed error")
	}
}