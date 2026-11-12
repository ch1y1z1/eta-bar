package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"eta-bar/internal/backend"
)

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func main() {
	config := flag.String("config", env("ETA_CONFIG", "config.json"), "configuration JSON file")
	listen := flag.String("listen", env("ETA_LISTEN", "127.0.0.1:8080"), "HTTP listen address")
	data := flag.String("data-dir", env("ETA_DATA_DIR", "data"), "checkpoint directory")
	flag.Parse()
	token := os.Getenv("ETA_API_TOKEN")
	if err := backend.ValidateListen(*listen, token); err != nil {
		log.Fatal(err)
	}
	c, err := backend.LoadConfig(*config)
	if err != nil {
		log.Fatal(err)
	}
	app, err := backend.New(c, *data, token)
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	app.Start(ctx)
	server := &http.Server{Addr: *listen, Handler: app, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 * 1024}
	go func() {
		<-ctx.Done()
		shutdown, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = server.Shutdown(shutdown)
	}()
	log.Print("ETA backend starting")
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal("HTTP server failed")
	}
}
