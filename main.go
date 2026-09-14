package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/coalaura/plain"
)

var (
	log = plain.New(plain.WithDate(plain.RFC3339Local))

	cfg *Config
	db  *Database
)

func main() {
	var err error

	log.Println("Loading config...")

	cfg, err = LoadConfig()
	log.MustFail(err)

	log.Println("Loading database...")

	db, err = OpenDatabase()
	log.MustFail(err)

	defer db.Close()

	log.Println("Preparing service...")

	service := NewService()

	log.Println("Preparing store...")

	router := NewRouter(service)

	server := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	listener, err := net.Listen("tcp", cfg.Server.Listen)
	log.MustFail(err)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	serverErrors := make(chan error, 1)

	go func() {
		serveErr := server.Serve(listener)
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			serverErrors <- serveErr

			return
		}

		serverErrors <- nil
	}()

	log.Printf("listening on %s\n", listener.Addr())

	service.Start(ctx)

	select {
	case <-ctx.Done():
	case serveErr := <-serverErrors:
		if serveErr != nil {
			log.Errorf("http server: %v\n", serveErr)
		}

		cancel()
	}

	err = ShutdownServer(server)
	if err != nil {
		log.Warnf("shutdown http server: %v\n", err)
	}

	service.Wait()
}
