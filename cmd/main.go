package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"

	"lilmail/internal/auth"
	"lilmail/internal/cache"
	"lilmail/internal/crypto"
	"lilmail/internal/server/handlers"
)

func main() {
	// Parse command line flags
	var (
		port         = flag.Int("port", 8080, "HTTP server port")
		cacheDir     = flag.String("cache-dir", "./cache", "Directory for file cache")
		cryptoKey    = flag.String("crypto-key", "", "Key for crypto operations")
		cryptoSalt   = flag.String("crypto-salt", "", "Salt for crypto operations")
		maxCacheSize = flag.Int64("cache-size", 100*1024*1024, "Maximum cache size in bytes")
		enableCORS   = flag.Bool("cors", false, "Enable CORS for development")
	)
	flag.Parse()

	// Setup encryption
	if *cryptoKey == "" || *cryptoSalt == "" {
		log.Fatal("crypto key and salt are required")
	}
	crypto, err := crypto.NewManager(*cryptoKey, *cryptoSalt)
	if err != nil {
		log.Fatalf("failed to initialize crypto: %v", err)
	}

	// Setup cache
	cacheDirAbs, err := filepath.Abs(*cacheDir)
	if err != nil {
		log.Fatalf("failed to resolve cache directory: %v", err)
	}
	cache, err := cache.NewFileCache(cacheDirAbs, *maxCacheSize, 24*time.Hour, crypto)
	if err != nil {
		log.Fatalf("failed to initialize cache: %v", err)
	}

	// Setup auth manager
	auth, err := auth.NewManager(
		crypto,
		nil, // email client will be set per session
		"localhost",
		8080,
		24*time.Hour,   // session duration
		30*time.Minute, // cleanup interval
	)
	if err != nil {
		log.Fatalf("failed to initialize auth manager: %v", err)
	}

	// Setup handlers
	h := handlers.NewHandler(auth, cache, crypto)

	// Setup router
	r := chi.NewRouter()

	// Add CORS middleware if enabled
	if *enableCORS {
		r.Use(cors.Handler(cors.Options{
			AllowedOrigins:   []string{"http://localhost:*"},
			AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
			AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type"},
			AllowCredentials: true,
			MaxAge:           300,
		}))
	}

	// Mount handlers
	r.Mount("/", h.Routes())

	// Setup static file serving for development
	workDir, _ := os.Getwd()
	filesDir := filepath.Join(workDir, "static")
	r.Handle("/static/*", http.StripPrefix("/static", http.FileServer(http.Dir(filesDir))))

	// Create server
	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", *port),
		Handler:      r,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Setup graceful shutdown
	done := make(chan bool)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-quit
		log.Println("Server is shutting down...")

		// Create shutdown context
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := srv.Shutdown(ctx); err != nil {
			log.Fatalf("Could not gracefully shutdown the server: %v\n", err)
		}
		close(done)
	}()

	// Start server
	log.Printf("Server is starting on port %d...", *port)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Could not listen on %d: %v\n", *port, err)
	}

	<-done
	log.Println("Server stopped")
}
