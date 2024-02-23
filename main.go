package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/davecgh/go-spew/spew"
)

var (
	gitCommit string
	buildDate string
	version   string
)

func main() {
	var (
		configFile  string
		role        string
		logLevel    string
		showVersion bool
	)
	flag.StringVar(&configFile, "c", "./rp.toml", "config file")
	flag.StringVar(&role, "r", "", "run role, override config file")
	flag.StringVar(&logLevel, "l", "", "log level, override config file")
	flag.BoolVar(&showVersion, "v", false, "show version")
	flag.Parse()

	if showVersion {
		fmt.Printf("rp - Reverse Proxy\n\nVersion: %s\nGit Commit: %s\nBuild Date: %s\n", version, gitCommit, buildDate)
		return
	}

	config, err := ParseConfig(configFile, role, logLevel)
	if err != nil {
		defaultLogger.Error("run failed", "error", err)
		return
	}

	if defaultLogger.Enabled(context.Background(), slog.LevelDebug) {
		fmt.Print(spew.Sdump(config))
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		timer := time.NewTimer(time.Second * 5)
		<-timer.C
		defaultLogger.Info("cleanup timeout, exit")
		os.Exit(0)
	}()

	if config.HttpPprofAddr != "" {
		go func() {
			defaultLogger.Debug("http pprof is running", "addr", config.HttpPprofAddr)
			err := http.ListenAndServe(config.HttpPprofAddr, nil)
			if err != nil {
				defaultLogger.Warn("http pprof error", "error", err)
				return
			}
		}()
	}

	switch strings.ToUpper(config.Role) {
	case "SERVER":
		s := NewServer(config, defaultLogger)
		err = s.Run(ctx)
	case "CLIENT":
		c := NewClient(config, defaultLogger)
		err = c.Run(ctx)
	default:
		defaultLogger.Error("unknown role in config", "role", config.Role)
	}
	if err != nil {
		defaultLogger.Error("start error", "role", config.Role, "error", err)
		return
	}
}
