package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		log.Printf("pproxy: %v", err)
		os.Exit(1)
	}
}

func run() error {
	listenAddress := flag.String("listen", "127.0.0.1:7895", "local proxy listen address")
	upstreamValue := flag.String("proxy", "http://127.0.0.1:7890", "upstream HTTP proxy URL")
	listPath := flag.String("list", "./DIRECT_LIST", "destination list file")
	verbose := flag.Bool("verbose", false, "log the route selected for every request")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return nil
	}

	file, err := os.Open(*listPath)
	if err != nil {
		return fmt.Errorf("open destination list: %w", err)
	}
	matcher, err := loadDestinationMatcher(file)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return fmt.Errorf("close destination list: %w", closeErr)
	}

	upstream, err := url.Parse(*upstreamValue)
	if err != nil {
		return fmt.Errorf("parse upstream proxy URL: %w", err)
	}
	logger := log.New(os.Stderr, "pproxy: ", log.LstdFlags)
	proxy, err := newForwardProxy(matcher, upstream, logger, *verbose)
	if err != nil {
		return err
	}
	defer proxy.close()

	address, err := normalizeListenAddress(*listenAddress)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", address, err)
	}

	server := &http.Server{
		Handler:           proxy,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveError := make(chan error, 1)
	go func() {
		serveError <- server.Serve(listener)
	}()
	logger.Printf("listening on http://%s (upstream %s, %d rules)", listener.Addr(), upstream.Redacted(), len(matcher.rules))

	select {
	case err := <-serveError:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shut down server: %w", err)
		}
		if err := <-serveError; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

func normalizeListenAddress(value string) (string, error) {
	if value == "" {
		return "", errors.New("listen address cannot be empty")
	}
	if parsed, err := url.Parse(value); err == nil && parsed.Scheme != "" {
		if parsed.Scheme != "http" || parsed.Host == "" || parsed.Path != "" {
			return "", fmt.Errorf("invalid listen URL %q", value)
		}
		value = parsed.Host
	}
	if _, _, err := net.SplitHostPort(value); err != nil {
		return "", fmt.Errorf("invalid listen address %q: %w", value, err)
	}
	return value, nil
}
