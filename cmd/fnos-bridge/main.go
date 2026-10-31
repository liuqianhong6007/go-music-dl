package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	fnosbridge "github.com/guohuiyuan/go-music-dl/internal/fnos-bridge"
	"github.com/guohuiyuan/go-music-dl/internal/fnos-bridge/sourcecontract"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		log.Printf("fnos-bridge: %v", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		targetSocket    = flag.String("target-socket", "/var/run/trim_music.socket", "official fnOS Music socket")
		upstreamSocket  = flag.String("upstream-socket", "/var/run/trim_music_upstream.socket", "official backend socket after takeover")
		sourceSocket    = flag.String("source-socket", "/var/apps/go-music-dl/target/go-music-dl.sock", "go-music-dl shared Web/source API socket")
		sourceTokenFile = flag.String("source-token-file", "", "source API token file; defaults to <source-socket>.source-token")
		sourcesCSV      = flag.String("sources", "netease,qq,kugou,kuwo,migu", "comma-separated source names")
		maxOnline       = flag.Int("max-online", 30, "maximum online tracks per search")
		searchTimeout   = flag.Duration("search-timeout", 12*time.Second, "source search timeout")
		stateFile       = flag.String("state-file", "data/fnos-music/guid-map.json", "persistent GUID map")
		socketMode      = flag.String("socket-mode", "0666", "official socket mode after takeover")
		showVersion     = flag.Bool("version", false, "print version")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}

	mode, err := parseSocketMode(*socketMode)
	if err != nil {
		return err
	}
	statePath, err := filepath.Abs(strings.TrimSpace(*stateFile))
	if err != nil {
		return err
	}
	mappings := fnosbridge.NewMappingStore(statePath)
	if err := mappings.Load(); err != nil {
		return err
	}
	tokenPath := strings.TrimSpace(*sourceTokenFile)
	if tokenPath == "" {
		tokenPath = strings.TrimSpace(*sourceSocket) + ".source-token"
	}
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		return fmt.Errorf("read source API token: %w", err)
	}
	source := sourcecontract.NewClient(*sourceSocket, strings.TrimSpace(string(token)))
	defer source.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	healthCtx, healthCancel := context.WithTimeout(ctx, 5*time.Second)
	defer healthCancel()
	if err := source.Health(healthCtx); err != nil {
		return fmt.Errorf("go-music-dl source API unavailable: %w", err)
	}

	takeover, err := fnosbridge.TakeOver(*targetSocket, *upstreamSocket, mode)
	if err != nil {
		return err
	}
	defer func() {
		if err := takeover.Close(); err != nil {
			log.Printf("restore official socket: %v", err)
		}
	}()

	bridge, err := fnosbridge.NewBridge(fnosbridge.BridgeConfig{
		Upstream:      *upstreamSocket,
		Source:        source,
		Mappings:      mappings,
		Sources:       splitCSV(*sourcesCSV),
		MaxOnline:     *maxOnline,
		SearchTimeout: *searchTimeout,
		Version:       version,
	})
	if err != nil {
		return err
	}
	defer bridge.Close()

	server := &http.Server{
		Handler:           bridge.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() {
		err := server.Serve(takeover.Listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	log.Printf("fnOS music bridge ready: %s -> %s", *targetSocket, *upstreamSocket)
	log.Printf("source API: %s", *sourceSocket)
	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return <-serveErr
	}
}

func splitCSV(raw string) []string {
	var values []string
	for _, value := range strings.Split(raw, ",") {
		value = strings.TrimSpace(value)
		if value != "" {
			values = append(values, value)
		}
	}
	return values
}

func parseSocketMode(value string) (os.FileMode, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseUint(strings.TrimPrefix(value, "0o"), 8, 32)
	if err != nil {
		return 0, err
	}
	if parsed > 0o777 {
		return 0, fmt.Errorf("socket mode is out of range")
	}
	return os.FileMode(parsed), nil
}
