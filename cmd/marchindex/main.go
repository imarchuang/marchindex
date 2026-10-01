package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/imarchuang/marchindex/index"
)

func main() {
	var (
		dataDir = flag.String("dataDir", "./data", "directory where index data is stored")
		addr    = flag.String("addr", ":9200", "HTTP listen address")
	)
	flag.Parse()

	log.Printf("starting marchindex on %s (dataDir: %s)", *addr, *dataDir)

	mgr, err := index.NewManager(*dataDir)
	if err != nil {
		log.Fatalf("failed to initialize index manager: %v", err)
	}

	server := &http.Server{
		Addr:    *addr,
		Handler: NewServer(mgr),
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen error: %v", err)
		}
	}()

	<-stop
	log.Println("shutting down marchindex...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Printf("shutdown error: %v", err)
	}
}
