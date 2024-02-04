package main

import (
	"encoding/gob"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xtaci/smux"
)

const (
	ClientDialUpstreamMaxRetries    = 15
	ClientDialUpstreamRetryInterval = time.Second * 5
)

type RegisterDownstream struct {
	Endpoints []string // slice of endpoint name
}

type UpstreamConn struct {
	Session *smux.Session
	Conn    net.Conn
	ConnNum int32
}

type Client struct {
	mu     sync.Mutex
	config Config
	uc     []*UpstreamConn
	logger *slog.Logger
	ce     map[string]*clientEndpoint
}

type clientEndpoint struct {
	config *EndpointConfig
}

func NewClient(config Config, logger *slog.Logger) (client *Client) {
	client = &Client{
		config: config,
		uc:     make([]*UpstreamConn, 0, 8),
		ce:     make(map[string]*clientEndpoint, 16),
		logger: logger,
	}
	return
}

func (c *Client) initConn(protocol, addr string) (conn net.Conn, err error) {
	var retries = ClientDialUpstreamMaxRetries

	for retries > 0 {
		conn, err = net.Dial("tcp", c.config.UpstreamAddr)
		if err != nil {
			retries--
			time.Sleep(ClientDialUpstreamRetryInterval)
			continue
		}

		c.logger.Debug("dial upstream done", "addr", conn.RemoteAddr())
		return
	}

	c.logger.Debug("dial upstream failed", "error", err)
	return
}

func (c *Client) initConnPool() (err error) {
	for range c.config.ConnPoolSize {
		uc := new(UpstreamConn)
		uc.Conn, err = c.initConn("tcp", c.config.UpstreamAddr)
		if err != nil {
			err = fmt.Errorf("init conn pool error: %w", err)
			return
		}
		uc.Session, err = smux.Server(uc.Conn, &smux.Config{
			Version:           2,
			KeepAliveDisabled: false,
			KeepAliveInterval: time.Second * 15,
			KeepAliveTimeout:  time.Second * 30,
			MaxFrameSize:      32 * 1024,
			MaxReceiveBuffer:  512 * 1024,
			MaxStreamBuffer:   512 * 1024,
		})
		if err != nil {
			err = fmt.Errorf("init conn pool error: %w", err)
			return
		}

		c.mu.Lock()
		c.uc = append(c.uc, uc)
		c.mu.Unlock()
	}
	return
}

func (c *Client) registerEndpoint() (err error) {
	es := make([]string, 0, len(c.ce))
	for name := range c.ce {
		es = append(es, name)
	}

	req := RegisterDownstream{Endpoints: es}
	c.mu.Lock()
	for _, uc := range c.uc {
		err = gob.NewEncoder(uc.Conn).Encode(req)
		if err != nil {
			err = fmt.Errorf("unable to register endpint: %w", err)
			break
		}

		c.logger.Debug("register done", "addr", uc.Conn.RemoteAddr())
	}
	c.mu.Unlock()
	return
}

func (c *Client) initEndpoint() {
	for _, cfg := range c.config.Endpoints {
		ep := &clientEndpoint{
			config: &cfg,
		}
		c.ce[cfg.Name] = ep
	}
	return
}

func (c *Client) handleUpstreamStream(uc *UpstreamConn, ss *smux.Stream) {
	defer func() {
		uc.ConnNum--
	}()

	// handshake
	var req ServerRequestEndpoint
	err := gob.NewDecoder(ss).Decode(&req)
	if err != nil {
		c.logger.Error("unable to upstream handshake", "error", err)
		return
	}
	enp, ok := c.ce[req.Name]
	if !ok {
		c.logger.Error("unable to find available endpoint", "name", req.Name)
		return
	}
	conn, err := net.Dial("tcp", enp.config.TargetAddr)
	if err != nil {
		c.logger.Error("unable to dial target", "error", err)
		return
	}
	c.logger.Debug("handle request", "endpoint", enp.config.Name, "remote_addr", conn.RemoteAddr())

	proxyConn(conn, ss)

	c.logger.Debug("handle request done", "endpoint", enp.config.Name, "remote_addr", conn.RemoteAddr())
	return
}

func (c *Client) handleUpstreamConn(uc *UpstreamConn) {
	for {
		stream, er := uc.Session.AcceptStream()
		if er != nil {
			if uc.Session.IsClosed() || er == io.EOF {
				c.logger.Debug("accept session closed, return")
				return
			}
			c.logger.Error("unable to accept stream", "error", er)

			time.Sleep(time.Millisecond * 10)
			continue
		}
		atomic.AddInt32(&uc.ConnNum, 1)

		go c.handleUpstreamStream(uc, stream)
	}
}

func (c *Client) Close() {
	c.mu.Lock()
	for _, uc := range c.uc {
		err := uc.Conn.Close()
		if err != nil {
			c.logger.Error("close upstream error", "error", err)
			continue
		}
		err = uc.Session.Close()
		if err != nil {
			c.logger.Error("close upstream error", "error", err)
			continue
		}
	}
	c.mu.Unlock()
	return
}

func (c *Client) Run() (err error) {
	c.initEndpoint()
	err = c.initConnPool()
	if err != nil {
		err = fmt.Errorf("run client error: %w", err)
		return err
	}
	err = c.registerEndpoint()
	if err != nil {
		err = fmt.Errorf("run client error: %w", err)
		return err
	}

	c.logger.Info("running")
	var wg sync.WaitGroup
	for _, uc := range c.uc {
		wg.Add(1)

		go func(uc *UpstreamConn) {
			defer wg.Done()

			c.handleUpstreamConn(uc)
		}(uc)
	}
	wg.Wait()
	return
}
