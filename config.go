package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/samber/lo"
)

const (
	DefaultConnPoolSize = 16

	ClientDialUpstreamMaxRetries    = 15
	ClientDialUpstreamRetryInterval = 5

	RoleClient = "client"
	RoleServer = "server"
)

var (
	AvailableUpstreamProtocol            = []string{"tcp"}
	AvailableEndpointProtocol            = []string{"tcp"}
	AvailableDownstreamSelectionStrategy = []string{"conn", "bandwidth"}
)

type Config struct {
	Role      string           `toml:"role"`
	Endpoints []EndpointConfig `toml:"endpoint"`
	LogLevel  string           `toml:"log_level"`
	Server    *ServerConfig    `toml:"server"`
	Client    *ClientConfig    `toml:"client"`
}

type ServerConfig struct {
	Protocol                    string `toml:"protocol"`
	ListenDownstreamAddr        string `toml:"listen_downstream_addr"`
	DownstreamSelectionStrategy string `toml:"downstream_selection_strategy"`
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

func ParseConfig(configFile, role, logLevel string) (config Config, err error) {
	_, err = toml.DecodeFile(configFile, &config)
	if err != nil {
		err = fmt.Errorf("parse config error: %w", err)
		return
	}
	// correct role
	if role != "" {
		config.Role = role
	}
	switch strings.ToLower(config.Role) {
	case RoleClient, RoleServer:
	default:
		err = errors.New("unknown role")
		return
	}

	// check log level
	var (
		ll  = parseLogLevel(logLevel)
		cll = parseLogLevel(config.LogLevel)
	)
	if logLevel != "" {
		defaultLogger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: ll}))
	} else {
		defaultLogger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: cll}))
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
	lo.ForEach(config.Endpoints, func(enp EndpointConfig, _ int) {
		if enp.Name == "" {
			err = errors.New("empty endpoint name")
			return
		}
		if enp.ListenAddr == "" || enp.TargetAddr == "" {
			err = errors.New("empty endpoint addr")
			return
		}
		if lo.IndexOf(AvailableEndpointProtocol, strings.ToLower(enp.Protocol)) == -1 {
			err = fmt.Errorf("unknown protocol: %v", enp.Protocol)
			return
		}
		return
	})

	// check client config
	if config.Role == RoleClient {
		if config.Client == nil {
			err = errors.New("role is client, but no client config found")
			return
		}
		// correct conn pool size
		if config.Client.ConnPoolSize <= 0 {
			config.Client.ConnPoolSize = DefaultConnPoolSize
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
		enps := lo.Map(config.Endpoints, func(enp EndpointConfig, _ int) string {
			return enp.Name
		})
		for i := range config.Client.Upstreams {
			up := &config.Client.Upstreams[i]

			if up.Name == "" {
				err = errors.New("empty upstream name")
				return
			}
			if up.Addr == "" {
				err = errors.New("empty upstream addr")
				return
			}
			if lo.IndexOf(AvailableUpstreamProtocol, strings.ToLower(up.Protocol)) == -1 {
				err = errors.New("no available upstream protocol")
				return
			}
			// correct retries
			if up.MaxRetries == 0 {
				up.MaxRetries = ClientDialUpstreamMaxRetries
			}
			if up.RetryInterval == 0 {
				up.RetryInterval = ClientDialUpstreamRetryInterval
			}
			// check endpoints existed
			up.Endpoints = lo.Uniq(up.Endpoints)
			if len(up.Endpoints) == 1 && up.Endpoints[0] == "*" {
				up.Endpoints = enps
			} else {
				lo.ForEach(up.Endpoints, func(enp string, _ int) {
					if lo.IndexOf(enps, enp) == -1 {
						err = fmt.Errorf("unknown upstream endpoint: %v", enp)
						return
					}
				})
			}
		}
	}

	// check server
	if config.Role == RoleServer {
		if config.Server == nil {
			err = errors.New("role is server, but no server config found")
			return
		}
		if lo.IndexOf(AvailableUpstreamProtocol, config.Server.Protocol) == -1 {
			err = fmt.Errorf("unknown server protocol: %v", config.Server.Protocol)
			return
		}
		if config.Server.ListenDownstreamAddr == "" {
			err = errors.New("empty server listen addr")
			return
		}
		if lo.IndexOf(AvailableDownstreamSelectionStrategy, strings.ToLower(config.Server.DownstreamSelectionStrategy)) == -1 {
			err = ErrUnknownDownstreamSelectionStrategy
			return
		}
	}
	return
}
