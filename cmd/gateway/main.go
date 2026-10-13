// Command gateway runs the simulated payment gateway HTTP service.
//
// SIMULATION ONLY: it never connects to a real card network and must only be
// used with synthetic test card numbers.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/robertramos/enterprise-payment-gateway/internal/app"
	"github.com/robertramos/enterprise-payment-gateway/internal/config"
	"github.com/robertramos/enterprise-payment-gateway/internal/logging"
)

var version = "0.1.0"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gateway:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", os.Getenv("GATEWAY_CONFIG_FILE"), "optional JSON config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	checkConfig := flag.Bool("check-config", false, "validate configuration and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	cfg, err := config.Load(*configPath, os.Getenv)
	if err != nil {
		return err
	}
	if *checkConfig {
		fmt.Println("configuration OK")
		return nil
	}
	log, err := logging.New(os.Stdout, cfg.LogLevel, cfg.LogFormat)
	if err != nil {
		return err
	}
	if cfg.EphemeralVault {
		log.Warn("dev mode: using an EPHEMERAL vault key; tokenized cards become unreadable after restart")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := app.New(ctx, cfg, app.Options{Log: log, Version: version})
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		a.Stop()
		return err
	}
	log.Info("gateway started", "addr", ln.Addr().String(), "version", version, "env", cfg.Env,
		"db", cfg.DBPath, "notice", "SIMULATION ONLY - synthetic test data, not PCI certified")
	a.Start(ctx)
	serveErr := a.Serve(ctx, ln)
	stopErr := a.Stop()
	log.Info("gateway stopped")
	if serveErr != nil {
		return serveErr
	}
	return stopErr
}
