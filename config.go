package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/samber/lo"
)

const (
	DefaultConnPoolSize = 16
)

type Config struct {
	Role      string           `toml:"role"`
	Endpoints []EndpointConfig `toml:"endpoint"`
	LogLevel  string           `toml:"log_level"`
	Server    ServerConfig     `toml:"server"`
	Client    ClientConfig     `toml:"client"`
}

type ServerConfig struct {
	Protocol             string `toml:"protocol"`
	ListenDownstreamAddr string `toml:"listen_downstream_addr"`
}

type ClientUpstreamConfig struct {
	Name          string   `toml:"name"`
	Protocol      string   `toml:"protocol"`
	Addr          string   `toml:"addr"`
	MaxRetries    int      `toml:"max_retries"`
	RetryInterval int      `toml:"retry_interval"`
	Endpoints     []string `toml:"endpoints"`
}

type ClientConfig struct {
	ConnPoolSize int                    `toml:"conn_pool_size"`
	Upstreams    []ClientUpstreamConfig `toml:"upstream"`
}

type EndpointConfig struct {
	Name       string `toml:"name"`
	Mode       string `toml:"mode"` // downstream or direct
	Protocol   string `toml:"protocol"`
	ListenAddr string `toml:"listen"`
	TargetAddr string `toml:"target"`
}

type EndpointMode int

const (
	EndpointModeDownstream EndpointMode = 0
	EndpointModeDirect     EndpointMode = 1
)

var defaultLogger *slog.Logger

func init() {
	defaultLogger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func parseEndpointMode(mode string) (m EndpointMode, err error) {
	switch strings.ToUpper(mode) {
	case "downstream":
		m = EndpointModeDownstream
	case "direct":
		m = EndpointModeDirect
	default:
		err = errors.New("unknown endpoint mode")
	}
	return
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
	if config.Client.ConnPoolSize == 0 {
		config.Client.ConnPoolSize = DefaultConnPoolSize
	}
	if role != "" {
		config.Role = role
	}

	// check endpoints
	if len(config.Endpoints) <= 0 {
		err = errors.New("no available endpoints")
		return
	}
	// check endpoint duplicate
	if ds := lo.FindDuplicatesBy(config.Endpoints, func(endpoints EndpointConfig) (name string) {
		return endpoints.Name
	}); len(ds) > 0 {
		err = fmt.Errorf("duplicated endpoints: %v", ds)
		return
	}
	// check client upstream
	if len(config.Client.Upstreams) <= 0 {
		err = errors.New("no available client upstream")
		return
	}
	// check upstream duplicate
	if ds := lo.FindDuplicatesBy(config.Client.Upstreams, func(upstream ClientUpstreamConfig) (name string) {
		return upstream.Name
	}); len(ds) > 0 {
		err = fmt.Errorf("duplicated upstream: %v", ds)
		return
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
