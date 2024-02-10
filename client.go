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

type RegisterDownstream struct {
	Endpoints []string // slice of endpoint name
}

type Upstream struct {
	mu     sync.Mutex
	config *ClientUpstreamConfig
	uc     []*UpstreamConn
	es     []string // endpoints
}

type UpstreamConn struct {
	Session *smux.Session
	Conn    net.Conn
	ConnNum int32
}

type Client struct {
	mu     sync.Mutex
	us     map[string]*Upstream
	config Config
	logger *slog.Logger
	ce     map[string]*clientEndpoint
}

type clientEndpoint struct {
	config *EndpointConfig
}

func NewClient(config Config, logger *slog.Logger) (client *Client) {
	client = &Client{
		config: config,
		us:     make(map[string]*Upstream, 8),
		ce:     make(map[string]*clientEndpoint, 16),
		logger: logger,
	}
	return
}

func (c *Client) initConn(up *Upstream) (conn net.Conn, err error) {
	var retries = up.config.MaxRetries

	for retries > 0 {
		conn, err = net.Dial(up.config.Protocol, up.config.Addr)
		if err != nil {
			retries--
			time.Sleep(time.Duration(up.config.RetryInterval) * time.Second)
			continue
		}

		c.logger.Debug("dial upstream done", "addr", conn.RemoteAddr())
		return
	}

	c.logger.Debug("dial upstream failed", "error", err)
	return
}

func (c *Client) initUpstreamConnPool() (err error) {
	for _, up := range c.us {
		up.uc = make([]*UpstreamConn, 0, c.config.Client.ConnPoolSize)

		for range c.config.Client.ConnPoolSize {
			uc := new(UpstreamConn)
			uc.Conn, err = c.initConn(up)
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

			up.mu.Lock()
			up.uc = append(up.uc, uc)
			up.mu.Unlock()
		}
	}
	return
}

func (c *Client) registerEndpoint() (err error) {
	for _, up := range c.us {
		req := RegisterDownstream{Endpoints: up.config.Endpoints}

		up.mu.Lock()
		for _, uc := range up.uc {
			err = gob.NewEncoder(uc.Conn).Encode(req)
			if err != nil {
				err = fmt.Errorf("unable to register endpint: %w", err)
				break
			}

			c.logger.Debug("register done", "addr", uc.Conn.RemoteAddr())
		}
		up.mu.Unlock()
	}
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

func (c *Client) initUpstream() {
	for _, cu := range c.config.Client.Upstreams {
		up := &Upstream{
			config: &cu,
			es:     cu.Endpoints,
		}
		c.us[cu.Name] = up
	}
	return
}

func (c *Client) handleUpstreamStream(uc *UpstreamConn, ss *smux.Stream) {
	defer func() {
		uc.ConnNum--

		err := ss.Close()
		if err != nil {
			c.logger.Debug("close upstream stream error", "error", err)
			return
		}
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

	c.logger.Debug("handle request", "endpoint", enp.config.Name)

	switch req.Op {
	case ServerRequestOperationConnect:
		conn, err := net.Dial("tcp", enp.config.TargetAddr)
		if err != nil {
			c.logger.Error("unable to dial target", "error", err)
			return
		}
		defer func(conn net.Conn) {
			err := conn.Close()
			if err != nil {
				c.logger.Debug("close upstream stream error", "error", err)
				return
			}
		}(conn)

		err = proxyConn(conn, ss)
		if err != nil {
			c.logger.Error("proxy conn error", "error", err)
		}
	case ServerRequestOperationTestBandwidth:
		c.handleTestBandwidth(enp, ss)
	default:
		c.logger.Error("unknown server request operation", "error", ErrUnknownServerRequestOperation, "operation", req.Op)
	}

	c.logger.Debug("handle request done", "endpoint", enp.config.Name)
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
	for _, up := range c.us {
		up.mu.Lock()
		for _, uc := range up.uc {
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
		up.mu.Unlock()
	}
	return
}

func (c *Client) Run() (err error) {
	c.initEndpoint()
	c.initUpstream()
	err = c.initUpstreamConnPool()
	if err != nil {
		err = fmt.Errorf("run client error: %w", err)
		return err
	}
	err = c.registerEndpoint()
	if err != nil {
		err = fmt.Errorf("run client error: %w", err)
		return err
	}

	var wg sync.WaitGroup
	for _, up := range c.us {
		for _, uc := range up.uc {
			wg.Add(1)

			go func(uc *UpstreamConn) {
				defer wg.Done()

				c.handleUpstreamConn(uc)
			}(uc)
		}
	}
	c.logger.Info("running")
	wg.Wait()
	return
}

func (c *Client) handleTestBandwidth(endpoint *clientEndpoint, conn net.Conn) {
	var (
		buf       = make([]byte, 4096)
		startTime = time.Now()
	)
	for {
		// too long, stop it
		if time.Now().Sub(startTime) > time.Minute {
			break
		}

		_, err := conn.Read(buf)
		if err != nil {
			c.logger.Error("handle test bandwidth error", "error", err, "endpoint", endpoint.config.Name)
			break
		}
	}
}
