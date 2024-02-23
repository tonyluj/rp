package main

import (
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/xtaci/smux"
)

const (
	ClientMaxAcceptConsecutiveErrors = 8
)

const (
	EventTypeClientSetupConn EventType = "EVENT_TYPE_CLIENT_SETUP_CONN"
)

type eventClientSetupConn struct {
	upstream *Upstream
}

type RegisterDownstreamRequest struct {
	UUID      [16]byte // for unique ID for server side
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
}

type Client struct {
	mu     sync.Mutex
	us     map[string]*Upstream
	config Config
	logger *slog.Logger
	ce     map[string]*clientEndpoint
	uuid   uuid.UUID
	eh     *EventHub
}

type clientEndpoint struct {
	config *EndpointConfig
	addr   net.Addr
}

func NewClient(config Config, logger *slog.Logger) (client *Client) {
	id, err := uuid.NewRandom()
	if err != nil {
		err = fmt.Errorf("new client error: %w", err)
		return
	}

	client = &Client{
		config: config,
		us:     make(map[string]*Upstream, 8),
		ce:     make(map[string]*clientEndpoint, 16),
		logger: logger,
		uuid:   id,
		eh:     NewEventHub(logger),
	}
	return
}

func (c *Client) initEventHub() {
	c.eh.RegisterHandler(EventTypeServerSetupConn, c.handleSetupConn, false)
}

func (c *Client) handleSetupConn(payload any) (err error) {
	event, ok := payload.(*eventClientSetupConn)
	if !ok {
		err = errors.New("unknown event type")
		return
	}

	err = c.trySetupUpstreamConn(event.upstream)
	if err != nil {
		err = fmt.Errorf("unable to setup conn: %w", err)
		return
	}
	return
}

func (c *Client) initConn(up *Upstream) (conn net.Conn, err error) {
	conn, err = net.Dial(up.config.Protocol, up.config.Addr)
	if err != nil {
		err = fmt.Errorf("init conn error: %w", err)
		return
	}
	return
}

func (c *Client) trySetupUpstreamConn(up *Upstream) (err error) {
	for tries := 0; tries < up.config.MaxRetries+1; tries++ {
		err = c.setupUpstreamConn(up)
		if err != nil {
			time.Sleep(time.Second * time.Duration(up.config.RetryInterval))
			continue
		}
		break
	}
	if err != nil {
		err = fmt.Errorf("try setup upstream conn error: %w", err)
		return
	}

	return
}

func (c *Client) setupUpstreamConn(up *Upstream) (err error) {
	uc := new(UpstreamConn)
	uc.Conn, err = c.initConn(up)
	if err != nil {
		goto out
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
		goto cleanConn
	}

	err = c.registerUpstreamConnEndpoint(up, uc)
	if err != nil {
		goto cleanSession
	}

	up.mu.Lock()
	up.uc = append(up.uc, uc)
	up.mu.Unlock()
	return

cleanSession:
	if er := uc.Session.Close(); er != nil {
		c.logger.Debug("clean up conn session error", "error", er)
		return err
	}
cleanConn:
	if er := uc.Conn.Close(); er != nil {
		c.logger.Debug("clean up conn error", "error", er)
		return err
	}
out:
	err = fmt.Errorf("setup upstream conn error: %w", err)
	return
}

func (c *Client) registerUpstreamConnEndpoint(up *Upstream, uc *UpstreamConn) (err error) {
	req := RegisterDownstreamRequest{
		UUID:      c.uuid,
		Endpoints: up.config.Endpoints,
	}

	err = gob.NewEncoder(uc.Conn).Encode(req)
	if err != nil {
		err = fmt.Errorf("unable to register endpint: %w", err)
		return
	}

	c.logger.Debug("register done", "addr", uc.Conn.RemoteAddr())
	return
}

func (c *Client) initEndpoint() (err error) {
	for _, cfg := range c.config.Endpoints {
		ep := &clientEndpoint{config: &cfg}

		switch cfg.Protocol {
		case "tcp":
			addr, er := net.ResolveTCPAddr(cfg.Protocol, cfg.TargetAddr)
			if er != nil {
				err = fmt.Errorf("init endpoint error: %w", er)
				return
			}
			ep.addr = addr
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
			uc:     make([]*UpstreamConn, 0, c.config.Client.ConnPoolSize),
		}
		c.us[cu.Name] = up
	}
	return
}

func (c *Client) handleUpstreamStream(up *Upstream, ss *smux.Stream) {
	defer func() {
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

	switch req.Op {
	case ServerRequestOperationConnect:
		t := time.Now()
		c.logger.Debug("handle request", "endpoint", enp.config.Name, "upstream_addr",
			up.config.Addr, "local_addr", enp.config.TargetAddr, "protocol", enp.config.Protocol)

		addr, ok := enp.addr.(*net.TCPAddr)
		if !ok {
			c.logger.Error("unknown target addr", "addr", enp.addr)
			return
		}
		conn, err := net.DialTCP(enp.config.Protocol, nil, addr)
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

		err = NewProxier(conn, ss).Proxy()
		if err != nil {
			c.logger.Error("proxy conn error", "error", err)
		}

		c.logger.Debug("handle request done", "endpoint", enp.config.Name, "upstream_addr",
			up.config.Addr, "local_addr", enp.config.TargetAddr, "protocol", enp.config.Protocol,
			"cost", time.Now().Sub(t))
	case ServerRequestOperationTestBandwidth:
		c.logger.Debug("bandwidth cap test start", "endpoint", enp.config.Name, "upstream_addr",
			up.config.Addr, "protocol", enp.config.Protocol)

		c.handleTestBandwidth(enp, ss)

		c.logger.Debug("bandwidth cap test end", "endpoint", enp.config.Name, "upstream_addr",
			up.config.Addr, "protocol", enp.config.Protocol)
	case ServerRequestOperationSetupConn:
		c.logger.Debug("setup conn start", "endpoint", enp.config.Name, "upstream_addr",
			up.config.Addr, "protocol", enp.config.Protocol)

		c.eh.Emit(EventTypeClientSetupConn, &eventClientSetupConn{upstream: up})

		c.logger.Debug("setup conn end", "endpoint", enp.config.Name, "upstream_addr",
			up.config.Addr, "protocol", enp.config.Protocol)
	default:
		c.logger.Error("unknown server request operation", "error", ErrUnknownServerRequestOperation, "operation", req.Op)
	}

	c.logger.Debug("handle request done", "endpoint", enp.config.Name)
	return
}

func (c *Client) handleUpstreamConn(ctx context.Context, up *Upstream, uc *UpstreamConn) (err error) {
	var retries int
out:
	for {
		select {
		case <-ctx.Done():
			err := uc.Session.Close()
			if err != nil {
				c.logger.Warn("close upstream session error", "error", err)
			}

			break out
		default:
			stream, er := uc.Session.AcceptStream()
			if er != nil {
				retries++
				if uc.Session.IsClosed() || er == io.EOF {
					c.logger.Debug("accept session closed, return")
					err = errors.New("upstream session is closed")
					return
				}
				if retries > ClientMaxAcceptConsecutiveErrors {
					err = errors.New("upstream max retries reached, reconnect")
					return
				}

				c.logger.Error("unable to accept stream", "error", er)
				time.Sleep(time.Millisecond * 10)
				continue
			}
			retries = 0

			go c.handleUpstreamStream(up, stream)
		}
	}
	return
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

func (c *Client) handleUpstream(ctx context.Context, up *Upstream) (err error) {
	var (
		failed int
		er     error
	)
	for range c.config.Client.ConnPoolSize {
		er = c.trySetupUpstreamConn(up)
		if er != nil {
			failed++
			continue
		}
	}
	if failed >= c.config.Client.ConnPoolSize {
		err = fmt.Errorf("handle whole upstream %s error: %w", up.config.Name, er)
		return
	}

	var wg sync.WaitGroup
	for _, uc := range up.uc {
		wg.Add(1)

		go func(up *Upstream, uc *UpstreamConn) {
			defer wg.Done()

		out:
			for {
				select {
				case <-ctx.Done():
					break out
				default:
					er := c.handleUpstreamConn(ctx, up, uc)
					if er == nil {
						// SHOULD NEVER HAPPEN NOW
						c.logger.Error("handle upstream conn exit without error")
						break
					}

					// cleanup
					er = uc.Session.Close()
					if er != nil {
						c.logger.Debug("clean up upstream session error", "error", er, "upstream", up.config.Name)
					}
					er = uc.Conn.Close()
					if er != nil {
						c.logger.Debug("clean up upstream conn error", "error", er, "upstream", up.config.Name)
					}

					c.logger.Info("try to reconnect upstream", "upstream", up.config.Name, "endpoints",
						up.config.Endpoints)
					er = c.trySetupUpstreamConn(up)
					if er != nil {
						c.logger.Error("setup upstream max retries reached, abort", "error", er)
						break
					}
				}
			}
		}(up, uc)
	}
	wg.Wait()
	return
}

func (c *Client) Run(ctx context.Context) (err error) {
	c.logger.Info("client is preparing")

	err = c.initEndpoint()
	if err != nil {
		err = fmt.Errorf("run client error: %w", err)
		return err
	}
	c.initUpstream()
	c.initEventHub()
	go c.eh.Run(ctx)

	var wg sync.WaitGroup
	for _, up := range c.us {
		wg.Add(1)

		go func(up *Upstream) {
			defer wg.Done()

			er := c.handleUpstream(ctx, up)
			if er != nil {
				c.logger.Error("upstream handle error", "upstream", up.config.Name, "error", er)
				return
			}
		}(up)
	}

	c.logger.Info("client is running")
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
