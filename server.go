package main

import (
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/puzpuzpuz/xsync/v3"
	"github.com/samber/lo"
	"github.com/xtaci/smux"
)

type DownstreamState uint8

func (ds DownstreamState) IsOk() (ok bool) {
	if ds == DownstreamStateOk {
		ok = true
		return
	}
	return
}

const (
	DownstreamStateOk            DownstreamState = 1 // all connections work fine
	DownstreamStatePartialBroken DownstreamState = 2 // only partial connections work fine
	DownstreamStateBroken        DownstreamState = 3 // all connections are broken
)

type DownstreamConnState uint8

const (
	DownstreamConnStateReady  DownstreamConnState = 1 // ready to use
	DownstreamConnStateFailed DownstreamConnState = 2 // to be removed later
)

const (
	ServerCalculateBandwidthInterval    = time.Second * 30
	ServerCalculateBandwidthCapDuration = time.Second * 3
)

type ServerRequestOperation uint8

const (
	ServerRequestOperationConnect       ServerRequestOperation = 0
	ServerRequestOperationTestBandwidth ServerRequestOperation = 1
	ServerRequestOperationSetupConn     ServerRequestOperation = 2
)

var ErrUnknownServerRequestOperation = errors.New("unknown server request operation")

type DownstreamSelectionStrategy uint8

const (
	DownstreamSelectionStrategyConn      DownstreamSelectionStrategy = 0 // select less connections number
	DownstreamSelectionStrategyBandwidth DownstreamSelectionStrategy = 1 // select smaller current bandwidth
)

const (
	EventTypeServerSetupConn EventType = "EVENT_TYPE_SERVER_SETUP_CONN"
)

type eventServerSetupConn struct {
	ds *downstream
}

func (d DownstreamSelectionStrategy) String() string {
	switch d {
	case DownstreamSelectionStrategyConn:
		return "conn"
	case DownstreamSelectionStrategyBandwidth:
		return "bandwidth"
	default:
		return "unknown"
	}
}

var (
	ErrUnknownDownstreamSelectionStrategy = errors.New("unknown downstream selection strategy")
	ErrNoAvailableDownstream              = errors.New("no available downstream")
)

type ServerRequestEndpoint struct {
	Name string
	Op   ServerRequestOperation
}

type Server struct {
	mu       sync.Mutex
	config   Config
	ds       *xsync.MapOf[uuid.UUID, *downstream]
	listener net.Listener
	logger   *slog.Logger
	se       *xsync.MapOf[string, *serverEndpoint] // serverEndpoint
	eh       *EventHub
}

type downstream struct {
	mu           sync.RWMutex
	state        DownstreamState
	uuid         uuid.UUID
	endpointName []string // registered endpoint name
	dc           []*downstreamConn

	bandwidthCap        int64     // cap, calculate by testBandWidth(), all connections shares
	bandwidth           int64     // now, update every 15s
	bandwidthLastUpdate time.Time // last update timestamp
}

func (ds *downstream) State() (state DownstreamState) {
	ds.mu.RLock()
	defer ds.mu.RUnlock()

	state = ds.state
	return
}

func (ds *downstream) checkAndUpdateState() (readyConn, failedConn int) {
	for _, dc := range ds.dc {
		switch dc.state {
		case DownstreamConnStateReady:
			readyConn++
		case DownstreamConnStateFailed:
			failedConn++
		}
	}
	if readyConn == len(ds.dc) {
		ds.state = DownstreamStateOk
	} else if failedConn == len(ds.dc) {
		ds.state = DownstreamStateBroken
	} else if failedConn < len(ds.dc) {
		ds.state = DownstreamStatePartialBroken
	}
	return
}

func (ds *downstream) CheckAndUpdateState() (state DownstreamState) {
	ds.mu.Lock()
	defer ds.mu.Lock()

	ds.checkAndUpdateState()
	state = ds.state
	return
}

func (ds *downstream) UpdateState(state DownstreamState) {
	ds.mu.Lock()
	defer ds.mu.Lock()

	if state == ds.state {
		return
	}

	ds.state = state
}

type downstreamConn struct {
	mu      sync.RWMutex
	state   DownstreamConnState
	ds      *downstream
	session *smux.Session
	conn    net.Conn
	load    *xsync.Counter
	proxier *Proxier
}

func (dc *downstreamConn) State() (state DownstreamConnState) {
	dc.mu.RLock()
	defer dc.mu.RUnlock()

	state = dc.state
	return
}

func (dc *downstreamConn) UpdateState(state DownstreamConnState) {
	dc.mu.Lock()
	defer dc.mu.Lock()

	if state == dc.state {
		return
	}

	dc.state = state
}

type serverEndpoint struct {
	config *EndpointConfig
}

func NewServer(config Config, logger *slog.Logger) (server *Server) {
	server = &Server{
		config: config,
		ds:     xsync.NewMapOf[uuid.UUID, *downstream](),
		logger: logger,
		se:     xsync.NewMapOf[string, *serverEndpoint](),
		eh:     NewEventHub(logger),
	}
	return
}

func (s *Server) Close() {
	err := s.listener.Close()
	if err != nil {
		s.logger.Error("server close error", "error", err)
		return
	}
}

func (s *Server) initDownstreamListener() (err error) {
	s.listener, err = net.Listen("tcp", s.config.Server.ListenDownstreamAddr)
	if err != nil {
		err = fmt.Errorf("server could not listen addr: %w", err)
		return
	}
	return
}

func (s *Server) initEventHub() {
	s.eh.RegisterHandler(EventTypeServerSetupConn, s.downstreamSetupConnHandler, false)
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

func (s *Server) Run(ctx context.Context) (err error) {
	err = s.initDownstreamListener()
	if err != nil {
		err = fmt.Errorf("run server error: %w", err)
		return
	}
	s.initEventHub()
	go s.eh.Run(ctx)
	go s.doDownstreamListener()
	go s.downstreamWatchdog()

	if s.config.Server.DownstreamSelectionStrategy == DownstreamSelectionStrategyBandwidth.String() {
		go s.calculateDownstreamBandwidth()
	}

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
	s.logger.Info("running")
	wg.Wait()
	return
}

func (s *Server) calculateDownstreamBandwidth() {
	ticker := time.NewTicker(ServerCalculateBandwidthInterval)
	defer ticker.Stop()

	// detect bandwidth cap first, serially
	s.ds.Range(func(_ uuid.UUID, ds *downstream) bool {
		name := ds.endpointName[0] // MUST EXIST
		se, ok := s.se.Load(name)
		if !ok {
			// SHOULD NEVER HAPPEN
			s.logger.Error("calculate downstream bandwidth not find available endpoint", "endpoint", name)
			return true
		}

		s.handleUpstreamTcpConn(se, ServerRequestOperationTestBandwidth, nil)
		return true
	})

	for t := range ticker.C {
		s.logger.Debug("update downstream bandwidth")

		s.ds.Range(func(k uuid.UUID, ds *downstream) bool {
			var bandwidth int64

			ds.mu.RLock()
			for _, dc := range ds.dc {
				// TODO: now only calculate server -> client (upstream -> downstream)
				b := dc.proxier.BytesB2A()
				bandwidth += b
			}
			ds.mu.RUnlock()

			ds.bandwidth = int64(float64(bandwidth) / t.Sub(ds.bandwidthLastUpdate).Seconds())
			ds.bandwidthLastUpdate = t
			return true
		})
	}
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

	ds, ok := s.ds.Load(rd.UUID)
	if !ok {
		ds = &downstream{
			state:               DownstreamStateOk,
			uuid:                rd.UUID,
			endpointName:        rd.Endpoints,
			dc:                  make([]*downstreamConn, 0, 8),
			bandwidthLastUpdate: time.Now(),
		}
		s.ds.Store(rd.UUID, ds)
	}

	dc := &downstreamConn{
		state:   DownstreamConnStateReady,
		ds:      ds,
		session: session,
		conn:    conn,
		load:    xsync.NewCounter(),
	}
	ds.mu.Lock()
	ds.dc = append(ds.dc, dc)
	ds.mu.Unlock()

	// update downstream state when new connection added
	ds.CheckAndUpdateState()

	s.logger.Debug("registered downstream", "addr", dc.conn.RemoteAddr(), "endpoints", rd.Endpoints)
	return
}

func (s *Server) downstreamSetupConnHandler(data any) (err error) {
	event, ok := data.(*eventServerSetupConn)
	if !ok {
		err = errors.New("unknown event type")
		return
	}
	if event.ds.state == DownstreamStateBroken {
		// downstream is total broken, unable to recover
		s.logger.Warn("downstream is all broken, unable to recover", "downstream", event.ds.uuid.String())
		return
	}

	err = s.recoverDownstream(event.ds)
	if err != nil {
		s.logger.Error("recovery downstream failed", "downstream", event.ds.uuid.String())
		return
	}

	return
}

func (s *Server) downstreamWatchdog() {
	ticker := time.NewTicker(time.Second * 30)
	defer ticker.Stop()

	for range ticker.C {
		s.ds.Range(func(_ uuid.UUID, ds *downstream) bool {
			if dsState := ds.CheckAndUpdateState(); !dsState.IsOk() {
				s.logger.Debug("downstream watchdog detect failed", "downstream", ds.uuid.String())
				s.eh.Emit(EventTypeServerSetupConn, &eventServerSetupConn{ds: ds})
			}

			return true
		})
	}
}

func (s *Server) recoverDownstream(ds *downstream) (err error) {
	ds.mu.Lock()
	defer ds.mu.Lock()

	_, failed := ds.checkAndUpdateState()
	if ds.state == DownstreamStateOk {
		s.logger.Debug("downstream is ok, no need to recovery", "downstream", ds.uuid.String())
		return
	}

	var (
		tries int
		todo  = failed
	)
	for todo > 0 {
		tries++
		if tries > failed*5 {
			err = errors.New("unable to recovery downstream, too many retries")
			break
		}

		er := s.recoverDownstreamConn(ds)
		if er != nil {
			s.logger.Error("unable to recover downstream conn", "error", err)
			continue
		}
		todo--
	}

	return
}

func (s *Server) recoverDownstreamConn(ds *downstream) (err error) {
	for _, dc := range ds.dc {
		dc.load.Inc()

		ss, err := dc.session.OpenStream()
		if err != nil {
			dc.load.Dec()
			s.logger.Error("recover downstream stream error", "error", err)
			continue
		}

		err = s.sendRequest("", ServerRequestOperationSetupConn, ss)
		if err != nil {
			if er := ss.Close(); er != nil {
				s.logger.Error("recover downstream stream close error", "error", err)
			}

			dc.load.Dec()
			s.logger.Error("recover downstream stream error", "error", err)
			continue
		}

		if er := ss.Close(); er != nil {
			s.logger.Error("recover downstream stream close error", "error", err)
		}
		dc.load.Dec()
		break
	}

	return
}

func (s *Server) findDownstreamConn(name string) (adc *downstreamConn, err error) {
	switch strings.ToUpper(s.config.Server.DownstreamSelectionStrategy) {
	case DownstreamSelectionStrategyConn.String():
		adc, err = s.findDownstreamConnByConnNum(name)
	case DownstreamSelectionStrategyBandwidth.String():
		adc, err = s.findDownstreamConnByBandwidth(name)
	default:
		err = ErrUnknownDownstreamSelectionStrategy
	}
	return
}

func (s *Server) findDownstreamConnByBandwidth(name string) (adc *downstreamConn, err error) {
	var (
		maxbw int64 = -math.MaxInt64
		minbw int64 = math.MaxInt64
		ads   *downstream
	)

	// find available downstream
	s.ds.Range(func(_ uuid.UUID, ds *downstream) bool {
		if lo.IndexOf(ds.endpointName, name) == -1 || ds.State() == DownstreamStateBroken {
			return true
		}

		if ds.bandwidthCap > 0 {
			// find largest available one
			left := ds.bandwidthCap - ds.bandwidth
			if left > maxbw {
				maxbw = left
				ads = ds
			}
		} else {
			// find less used one
			if ds.bandwidth < minbw {
				minbw = ds.bandwidth
				ads = ds
			}
		}
		return true
	})
	if ads == nil {
		err = ErrNoAvailableDownstream
		return
	}

	// find available downstream conn
	var dsc int64 = math.MaxInt64
	ads.mu.RLock()
	for _, dc := range ads.dc {
		if dc.load.Value() < dsc {
			adc = dc
			dsc = dc.load.Value()
		}
	}
	ads.mu.RUnlock()
	if adc == nil {
		err = ErrNoAvailableDownstream
		return
	}
	return
}

func (s *Server) findDownstreamConnByConnNum(name string) (adc *downstreamConn, err error) {
	var dsc int64 = math.MaxInt64

	s.ds.Range(func(_ uuid.UUID, ds *downstream) bool {
		if lo.IndexOf(ds.endpointName, name) == -1 || ds.State() == DownstreamStateBroken {
			return true
		}

		ds.mu.RLock()
		for _, dc := range ds.dc {
			if dc.load.Value() < dsc {
				adc = dc
				dsc = dc.load.Value()
			}
		}
		ds.mu.RUnlock()
		return true
	})
	if adc == nil {
		err = ErrNoAvailableDownstream
		return
	}
	return
}

func (s *Server) setupStream(name string) (dc *downstreamConn, ss *smux.Stream, err error) {
	dc, err = s.findDownstreamConn(name)
	if err != nil {
		err = fmt.Errorf("unable to find available downstream connection: %w", err)
		return
	}
	dc.load.Inc()

	ss, err = dc.session.OpenStream()
	if err != nil {
		dc.load.Dec()
		// downstream conn is broken, update state
		dc.UpdateState(DownstreamConnStateFailed)
		if dsState := dc.ds.CheckAndUpdateState(); !dsState.IsOk() {
			s.logger.Debug("downstream setup detect failed", "downstream", dc.ds.uuid.String())
			s.eh.Emit(EventTypeServerSetupConn, &eventServerSetupConn{ds: dc.ds})
		}

		err = fmt.Errorf("unable to setup available downstream stream: %w", err)
		return
	}
	return
}

func (s *Server) sendRequest(name string, op ServerRequestOperation, ss *smux.Stream) (err error) {
	req := ServerRequestEndpoint{
		Name: name,
		Op:   op,
	}
	err = gob.NewEncoder(ss).Encode(req)
	if err != nil {
		err = fmt.Errorf("unable to send request: %w", err)
		return
	}
	return
}

func (s *Server) handleUpstreamTcpConn(endpoint *serverEndpoint, op ServerRequestOperation, conn net.Conn) {
	defer func(conn net.Conn) {
		if conn != nil {
			err := conn.Close()
			if err != nil {
				s.logger.Debug("close conn error", "error", err)
				return
			}
		}
	}(conn)

	dc, ss, err := s.setupStream(endpoint.config.Name)
	if err != nil {
		s.logger.Error("unable to setup downstream", "error", err)
		return
	}
	defer func(dc *downstreamConn, ss *smux.Stream) {
		dc.load.Dec()
		err := ss.Close()
		if err != nil {
			s.logger.Debug("close stream error", "error", err)
			return
		}
	}(dc, ss)

	err = s.sendRequest(endpoint.config.Name, op, ss)
	if err != nil {
		s.logger.Error("unable to downstream handshake req", "error", err)
		return
	}

	switch op {
	case ServerRequestOperationConnect:
		t := time.Now()
		s.logger.Debug("handle request", "endpoint", endpoint.config.Name, "downstream_addr",
			dc.session.RemoteAddr(), "remote_addr", conn.RemoteAddr(), "protocol", endpoint.config.Protocol)

		dc.proxier = NewProxier(conn, ss) // MUST: conn is B, ss is A
		err = dc.proxier.Proxy()
		if err != nil {
			s.logger.Error("proxy conn error", "error", err)
		}

		s.logger.Debug("handle request done", "endpoint", endpoint.config.Name, "downstream_addr",
			dc.session.RemoteAddr(), "remote_addr", conn.RemoteAddr(), "protocol", endpoint.config.Protocol,
			"cost", time.Now().Sub(t))
	case ServerRequestOperationTestBandwidth:
		s.logger.Debug("bandwidth cap test start", "endpoint", endpoint.config.Name, "downstream_addr",
			dc.session.RemoteAddr(), "protocol", endpoint.config.Protocol)

		s.testBandwidth(dc.ds, ss)

		s.logger.Debug("bandwidth cap test end", "endpoint", endpoint.config.Name, "downstream_addr",
			dc.session.RemoteAddr(), "protocol", endpoint.config.Protocol)
	}

	return
}

func (s *Server) handleEndpointTcp(endpoint *serverEndpoint) {
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

		go s.handleUpstreamTcpConn(endpoint, ServerRequestOperationConnect, conn)
	}
}

func (s *Server) handleEndpoint(endpoint *serverEndpoint) {
	s.logger.Debug("handle endpoint", "name", endpoint.config.Name, "addr", endpoint.config.ListenAddr)

	switch strings.ToLower(endpoint.config.Protocol) {
	case "tcp":
		s.handleEndpointTcp(endpoint)
	default:
		// THIS SHOULD NEVER HAPPEN
		s.logger.Error("unknown protocol", "protocol", endpoint.config.Protocol)
	}
}

// TODO: now only test server to client bandwidth
func (s *Server) testBandwidth(ds *downstream, conn net.Conn) {
	var (
		buf       = make([]byte, 4096)
		startTime = time.Now()
		costTime  time.Duration
		written   uint64
	)
	for {
		costTime := time.Now().Sub(startTime)
		if costTime > ServerCalculateBandwidthCapDuration {
			break
		}

		n, err := conn.Write(buf)
		if err != nil {
			s.logger.Error("test bandwidth error", "error", err)
			return
		}
		if n > 0 {
			written += uint64(n)
		}
	}
	ds.bandwidthCap = int64(float64(written) / costTime.Seconds())
	return
}
