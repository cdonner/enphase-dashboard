// Enphase dashboard server.
//
//   - Creates / stores / renews the gateway JWT (Enlighten login -> Entrez token)
//   - Polls the IQ Gateway meters and records today's power curve + daily energy totals
//   - Backfills from the Enphase cloud API what the gateway can't provide (cloud.go)
//   - Serves the React app (built into dist/ and embedded in the binary)
//
// Written in Go so it builds to a single static binary: it runs on NAS models whose
// kernels can't load ordinary Linux userlands (e.g. 32-bit ARM QNAPs with 32K pages).
package main

import (
	"bufio"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // TZ works without zoneinfo on the host (scratch image, NAS)

	_ "golang.org/x/crypto/x509roots/fallback" // CA roots when the host has none
)

//go:embed all:dist
var distFS embed.FS

type Config struct {
	DataDir           string
	EnvoyHost         string
	Host              string
	Port              int
	Poll              time.Duration
	DashboardPassword string
	APIKey            string
	ClientID          string
	ClientSecret      string
	MQTT              MQTTConfig
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// loadDotEnv sets variables from ./.env that aren't already set in the environment.
func loadDotEnv() {
	f, err := os.Open(".env")
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
}

func loadConfig() Config {
	loadDotEnv()
	port, _ := strconv.Atoi(env("PORT", "3001"))
	pollMs, _ := strconv.Atoi(env("POLL_MS", "1000"))
	dataDir, _ := filepath.Abs(env("DATA_DIR", "data"))
	return Config{
		DataDir:           dataDir,
		EnvoyHost:         env("ENVOY_HOST", "envoy.local"),
		Host:              env("HOST", "127.0.0.1"),
		Port:              port,
		Poll:              time.Duration(max(pollMs, 1000)) * time.Millisecond,
		DashboardPassword: os.Getenv("DASHBOARD_PASSWORD"),
		APIKey:            os.Getenv("ENPHASE_API_KEY"),
		ClientID:          os.Getenv("ENPHASE_CLIENT_ID"),
		ClientSecret:      os.Getenv("ENPHASE_CLIENT_SECRET"),
		MQTT: MQTTConfig{
			URL:             os.Getenv("MQTT_URL"),
			Username:        os.Getenv("MQTT_USERNAME"),
			Password:        os.Getenv("MQTT_PASSWORD"),
			Topic:           os.Getenv("MQTT_TOPIC"),
			DiscoveryPrefix: os.Getenv("HA_DISCOVERY_PREFIX"),
		},
	}
}

func main() {
	log.SetFlags(log.LstdFlags)
	cfg := loadConfig()
	// Re-read TZ after .env: time.Local is initialized before main runs.
	if tz := os.Getenv("TZ"); tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			time.Local = loc
		} else {
			log.Printf("Unknown TZ %q: %v", tz, err)
		}
	}
	log.Printf("Starting: %s %s/%s, data dir %s, gateway %s, TZ %s",
		runtime.Version(), runtime.GOOS, runtime.GOARCH, cfg.DataDir, cfg.EnvoyHost, time.Local)

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatalf("Cannot create data dir: %v", err)
	}

	s := NewServer(cfg)
	dist, err := fs.Sub(distFS, "dist")
	if err != nil {
		log.Fatal(err)
	}
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	srv := &http.Server{Addr: addr, Handler: s.routes(dist), ReadHeaderTimeout: 10 * time.Second}

	// Listen first so the page is reachable even while the slower startup steps run.
	go func() {
		log.Printf("Listening on http://%s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("Cannot listen on %s: %v", addr, err)
		}
	}()

	s.start()
	select {}
}
