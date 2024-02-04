package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

const (
	DefaultConnPoolSize = 16
)

type Config struct {
	Role                 string           `toml:"role"`
	Protocol             string           `toml:"protocol"`
	ListenDownstreamAddr string           `toml:"listen_downstream_addr"`
	UpstreamAddr         string           `toml:"upstream_addr"`
	Endpoints            []EndpointConfig `toml:"endpoints"`
	LogLevel             string           `toml:"log_level"`
	ConnPoolSize         int              `toml:"conn_pool_size"`
}

type EndpointConfig struct {
	Name       string `toml:"name"`
	Protocol   string `toml:"protocol"`
	ListenAddr string `toml:"listen"`
	TargetAddr string `toml:"target"`
}

var defaultLogger *slog.Logger

func init() {
	defaultLogger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func parseLogLevel(logLevel string) (ll slog.Level) {
	switch strings.ToUpper(logLevel) {
	case "INFO":
		ll = slog.LevelInfo
	case "DEBUG":
		ll = slog.LevelDebug
	case "WARN":
		ll = slog.LevelWarn
	case "ERROR":
		ll = slog.LevelError
	default:
		ll = slog.LevelInfo
	}
	return
}

func ParseConfig() (config Config, err error) {
	var (
		configFile string
		role       string
		logLevel   string
	)
	flag.StringVar(&configFile, "c", "./rp.toml", "config file")
	flag.StringVar(&role, "r", "", "run role, override config file")
	flag.StringVar(&logLevel, "l", "", "log level, override config file")
	flag.Parse()

	_, err = toml.DecodeFile(configFile, &config)
	if err != nil {
		err = fmt.Errorf("parse config error: %w", err)
		return
	}
	if config.ConnPoolSize == 0 {
		config.ConnPoolSize = DefaultConnPoolSize
	}
	if role != "" {
		config.Role = role
	}

	var (
		ll  = parseLogLevel(logLevel)
		cll = parseLogLevel(config.LogLevel)
	)
	if logLevel != "" {
		defaultLogger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: ll}))
	} else {
		defaultLogger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: cll}))
	}
	return
}
