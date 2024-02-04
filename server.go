package main

import (
	"encoding/gob"
	"fmt"
	"log/slog"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xtaci/smux"
)

type ServerRequestEndpoint struct {
	Name string
}

type Server struct {
	mu       sync.Mutex
	config   Config
	dc       []*downstreamConn
	listener net.Listener
	logger   *slog.Logger
	se       *sync.Map
}

type downstreamConn struct {
	Session      *smux.Session
	Conn         net.Conn
	EndpointName []string // registered endpoint name
	ConnNum      int32
}

type serverEndpoint struct {
	config *EndpointConfig
}

func NewServer(config Config, logger *slog.Logger) (server *Server) {
	server = &Server{
		config: config,
		dc:     make([]*downstreamConn, 0, 8),
		logger: logger,
		se:     new(sync.Map),
	}
	return
}

func (s *Server) Close() {
	s.se.Range(func(_, v any) bool {
		// TODO

		return false
	})
	err := s.listener.Close()
	if err != nil {
		s.logger.Error("server close error", "error", err)
		return
	}
}

func (s *Server) initDownstreamListener() (err error) {
	s.listener, err = net.Listen("tcp", s.config.ListenDownstreamAddr)
	if err != nil {
		err = fmt.Errorf("server could not listen addr: %w", err)
		return
	}
	return
}

func (s *Server) doDownstreamListener() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			s.logger.Error("server accept downstream error", "error", err)

			time.Sleep(time.Millisecond * 10)
			continue
		}

		go s.handleRegisterDownstreamEndpoint(conn)
	}
}

func (s *Server) Run() (err error) {
	err = s.initDownstreamListener()
	if err != nil {
		err = fmt.Errorf("run server error: %w", err)
		return
	}
	go s.doDownstreamListener()

	s.logger.Info("running")
	var wg sync.WaitGroup
	for _, cfg := range s.config.Endpoints {
		wg.Add(1)

		go func(cfg EndpointConfig) {
			defer wg.Done()

			endpoint := &serverEndpoint{config: &cfg}
			s.se.Store(cfg.Name, endpoint)
			s.handleEndpoint(endpoint)
		}(cfg)
	}
	wg.Wait()
	return
}

func (s *Server) handleRegisterDownstreamEndpoint(conn net.Conn) {
	var rd RegisterDownstream
	err := gob.NewDecoder(conn).Decode(&rd)
	if err != nil {
		s.logger.Error("unable to handle register downstream endpoint", "error", err)
		return
	}

	session, err := smux.Client(conn, &smux.Config{
		Version:           2,
		KeepAliveDisabled: false,
		KeepAliveInterval: time.Second * 15,
		KeepAliveTimeout:  time.Second * 30,
		MaxFrameSize:      32 * 1024,
		MaxReceiveBuffer:  512 * 1024,
		MaxStreamBuffer:   512 * 1024,
	})
	if err != nil {
		s.logger.Error("unable to handle register downstream endpoint", "error", err)
		return
	}
	dc := &downstreamConn{
		Session:      session,
		Conn:         conn,
		EndpointName: rd.Endpoints,
		ConnNum:      0,
	}
	s.logger.Debug("registered downstream", "addr", dc.Conn.RemoteAddr(), "endpoints", rd.Endpoints)

	s.mu.Lock()
	s.dc = append(s.dc, dc)
	s.mu.Unlock()
	return
}

func (s *Server) handleUpstreamConn(endpoint *serverEndpoint, conn net.Conn) {
	var (
		adc *downstreamConn
		dsc int32 = math.MaxInt32
	)

	s.mu.Lock()
	for _, ds := range s.dc {
		for _, en := range ds.EndpointName {
			if en == endpoint.config.Name && atomic.LoadInt32(&ds.ConnNum) < dsc {
				adc = ds
				dsc = adc.ConnNum
				break
			}
		}
	}
	s.mu.Unlock()

	if adc == nil {
		s.logger.Error("unable to find available downstream")
		return
	}
	atomic.AddInt32(&adc.ConnNum, 1)
	defer func() {
		atomic.AddInt32(&adc.ConnNum, -1)
	}()

	s.logger.Debug("handle request", "endpoint", endpoint.config.Name, "downstream_addr",
		adc.Session.RemoteAddr(), "remote_addr", conn.RemoteAddr())

	ss, err := adc.Session.OpenStream()
	if err != nil {
		s.logger.Error("unable to setup downstream stream", "error", err)
		return
	}

	// handshake
	req := ServerRequestEndpoint{
		Name: endpoint.config.Name,
	}
	err = gob.NewEncoder(ss).Encode(req)
	if err != nil {
		s.logger.Error("unable to downstream handshake req", "error", err)
		return
	}

	proxyConn(conn, ss)

	s.logger.Debug("handle request done", "endpoint", endpoint.config.Name, "downstream_addr",
		adc.Session.RemoteAddr(), "remote_addr", conn.RemoteAddr())
	return
}

func (s *Server) handleEndpoint(endpoint *serverEndpoint) {
	s.logger.Debug("handle endpoint", "name", endpoint.config.Name, "addr", endpoint.config.ListenAddr)

	l, err := net.Listen(endpoint.config.Protocol, endpoint.config.ListenAddr)
	if err != nil {
		s.logger.Error("server handle endpoint error", "error", err)
		return
	}
	defer func(l net.Listener) {
		err := l.Close()
		if err != nil {
			s.logger.Error("server close endpoint listener error", "error", err)
			return
		}
	}(l)

	for {
		conn, err := l.Accept()
		if err != nil {
			s.logger.Error("server handle endpoint error", "error", err)

			time.Sleep(time.Millisecond * 10)
			continue
		}

		go s.handleUpstreamConn(endpoint, conn)
	}
}
