package main

import (
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
	EventTypeClientSetupConn EventType = "EVENT_TYPE_CLIENT_SETUP_CONN"
)

type eventClientSetupConn struct {
	upstream *Upstream
}

type RegisterDownstream struct {
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

	conn, err := c.setupUpstreamConn(event.upstream)
	if err != nil {
		err = fmt.Errorf("unable to setup conn: %w", err)
		return
	}

	event.upstream.mu.Lock()
	event.upstream.uc = append(event.upstream.uc, conn)
	event.upstream.mu.Unlock()
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
			uc, err := c.setupUpstreamConn(up)
			if err != nil {
				err = fmt.Errorf("unable to setup upstream: %w", err)
				continue
			}

			up.mu.Lock()
			up.uc = append(up.uc, uc)
			up.mu.Unlock()
		}
	}
	return
}

func (c *Client) setupUpstreamConn(up *Upstream) (uc *UpstreamConn, err error) {
	uc = new(UpstreamConn)
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

	return
}

func (c *Client) registerEndpoint() (err error) {
	for _, up := range c.us {
		req := RegisterDownstream{
			UUID:      c.uuid,
			Endpoints: up.config.Endpoints,
		}

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

		err = NewProxier(conn, ss).Proxy()
		if err != nil {
			c.logger.Error("proxy conn error", "error", err)
		}
	case ServerRequestOperationTestBandwidth:
		c.handleTestBandwidth(enp, ss)
	case ServerRequestOperationSetupConn:
		c.eh.Emit(EventTypeClientSetupConn, &eventClientSetupConn{upstream: up})
	default:
		c.logger.Error("unknown server request operation", "error", ErrUnknownServerRequestOperation, "operation", req.Op)
	}

	c.logger.Debug("handle request done", "endpoint", enp.config.Name)
	return
}

func (c *Client) handleUpstreamConn(up *Upstream, uc *UpstreamConn) {
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

		go c.handleUpstreamStream(up, stream)
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
	c.initEventHub()
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

				c.handleUpstreamConn(up, uc)
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
