package transport

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/musix/backhaul/internal/utils"
	"github.com/musix/backhaul/internal/web"

	"github.com/sirupsen/logrus"
	"github.com/xtaci/smux"
)

type TcpMuxTransport struct {
	config         *TcpMuxConfig
	smuxConfig     *smux.Config
	parentctx      context.Context
	ctx            context.Context
	cancel         context.CancelFunc
	logger         *logrus.Logger
	tunnelChannel  chan net.Conn
	controlChannel net.Conn
	heartbeatSig   string
	chanSignal     string
	usageMonitor   *web.Usage
	restartMutex   sync.Mutex
}

type TcpMuxConfig struct {
	BindAddr         string
	Nodelay          bool
	KeepAlive        time.Duration
	Token            string
	ChannelSize      int
	Ports            []string
	MuxCon           int
	MuxVersion       int
	MaxFrameSize     int
	MaxReceiveBuffer int
	MaxStreamBuffer  int
	Sniffer          bool
	WebPort          int
	SnifferLog       string
	Heartbeat        time.Duration // in seconds
	TunnelStatus     string
}

func NewTcpMuxServer(parentCtx context.Context, config *TcpMuxConfig, logger *logrus.Logger) *TcpMuxTransport {
	// Create a derived context from the parent context
	ctx, cancel := context.WithCancel(parentCtx)

	// Initialize the TcpTransport struct
	server := &TcpMuxTransport{
		smuxConfig: &smux.Config{
			Version:           config.MuxVersion,
			KeepAliveInterval: 10 * time.Second,
			KeepAliveTimeout:  30 * time.Second,
			MaxFrameSize:      config.MaxFrameSize,
			MaxReceiveBuffer:  config.MaxReceiveBuffer,
			MaxStreamBuffer:   config.MaxStreamBuffer,
		},
		config:         config,
		parentctx:      parentCtx,
		ctx:            ctx,
		cancel:         cancel,
		logger:         logger,
		tunnelChannel:  make(chan net.Conn, config.ChannelSize),
		controlChannel: nil, // will be set when a control connection is established
		heartbeatSig:   "0", // Default heartbeat signal
		chanSignal:     "1", // Default channel signal
		usageMonitor:   web.NewDataStore(fmt.Sprintf(":%v", config.WebPort), ctx, config.SnifferLog, config.Sniffer, &config.TunnelStatus, logger),
	}

	return server
}

func (s *TcpMuxTransport) Restart() {
	if !s.restartMutex.TryLock() {
		s.logger.Warn("server restart already in progress, skipping restart attempt")
		return
	}
	defer s.restartMutex.Unlock()

	s.logger.Info("restarting server...")
	if s.cancel != nil {
		s.cancel()
	}

	time.Sleep(2 * time.Second)

	ctx, cancel := context.WithCancel(s.parentctx)
	s.ctx = ctx
	s.cancel = cancel

	// Re-initialize variables
	s.tunnelChannel = make(chan net.Conn, s.config.ChannelSize)
	s.controlChannel = nil
	s.usageMonitor = web.NewDataStore(fmt.Sprintf(":%v", s.config.WebPort), ctx, s.config.SnifferLog, s.config.Sniffer, &s.config.TunnelStatus, s.logger)
	s.config.TunnelStatus = ""

	go s.TunnelListener()

}

func (s *TcpMuxTransport) monitorControlChannel() {
	ticker := time.NewTicker(s.config.Heartbeat)
	defer ticker.Stop()

	// Channel to receive the message or error
	resultChan := make(chan struct {
		message string
		err     error
	})

	go func() {
		message, err := utils.ReceiveBinaryString(s.controlChannel)
		resultChan <- struct {
			message string
			err     error
		}{message, err}
	}()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			if s.controlChannel == nil {
				s.logger.Warn("control channel is nil, attempting to restart server...")
				go s.Restart()
				return
			}

			err := utils.SendBinaryString(s.controlChannel, s.heartbeatSig)
			if err != nil {
				s.logger.Error("failed to send heartbeat signal, attempting to restart server...")
				go s.Restart()
				return
			}
			s.logger.Trace("heartbeat signal sent successfully")

		case result := <-resultChan:
			if result.err != nil {
				s.logger.Errorf("failed to receive message from tunnel connection: %v", result.err)
				go s.Restart()
				return
			}
			if result.message == "closed" {
				s.logger.Info("control channel has been closed by the client")
				go s.Restart()
				return
			}
		}
	}
}

func (s *TcpMuxTransport) portConfigReader() {
	for _, portMapping := range s.config.Ports {
		var localAddr string
		parts := strings.Split(portMapping, "=")
		if len(parts) < 2 {
			port, err := strconv.Atoi(parts[0])
			if err != nil {
				s.logger.Fatalf("invalid port mapping format: %s", portMapping)
			}
			localAddr = fmt.Sprintf(":%d", port)
			parts = append(parts, strconv.Itoa(port))
		} else {
			localAddr = strings.TrimSpace(parts[0])
			if _, err := strconv.Atoi(localAddr); err == nil {
				localAddr = ":" + localAddr // :3080 format
			}
		}
		remoteAddr := strings.TrimSpace(parts[1])

		go s.localListener(localAddr, remoteAddr)
	}
}

func (s *TcpMuxTransport) TunnelListener() {
	// for  webui
	if s.config.WebPort > 0 {
		go s.usageMonitor.Monitor()
	}
	s.config.TunnelStatus = "Disconnected (TCPMux)"

	listener, err := net.Listen("tcp", s.config.BindAddr)
	if err != nil {
		s.logger.Fatalf("failed to start listener on %s: %v", s.config.BindAddr, err)
		return
	}

	defer listener.Close()

	s.logger.Infof("server started successfully, listening on address: %s", listener.Addr().String())

	go func() {
		for {
			select {
			case <-s.ctx.Done():
				return
			default:
				s.logger.Debugf("waiting for accept incoming tunnel connection on %s", listener.Addr().String())
				conn, err := listener.Accept()
				if err != nil {
					s.logger.Debugf("failed to accept tunnel connection on %s: %v", listener.Addr().String(), err)
					continue
				}

				//discard any non tcp connection
				tcpConn, ok := conn.(*net.TCPConn)
				if !ok {
					s.logger.Warnf("disarded non-TCP tunnel connection from %s", conn.RemoteAddr().String())
					conn.Close()
					continue
				}

				// Drop all suspicious packets from other address rather than server
				if s.controlChannel != nil && s.controlChannel.RemoteAddr().(*net.TCPAddr).IP.String() != tcpConn.RemoteAddr().(*net.TCPAddr).IP.String() {
					s.logger.Debugf("suspicious packet from %v. expected address: %v. discarding packet...", tcpConn.RemoteAddr().(*net.TCPAddr).IP.String(), s.controlChannel.RemoteAddr().(*net.TCPAddr).IP.String())
					tcpConn.Close()
					continue
				}

				// trying to set tcpnodelay
				if !s.config.Nodelay {
					if err := tcpConn.SetNoDelay(s.config.Nodelay); err != nil {
						s.logger.Warnf("failed to set TCP_NODELAY for %s: %v", tcpConn.RemoteAddr().String(), err)
					} else {
						s.logger.Tracef("TCP_NODELAY disabled for %s", tcpConn.RemoteAddr().String())
					}
				}

				// Set keep-alive settings
				if err := tcpConn.SetKeepAlive(true); err != nil {
					s.logger.Warnf("failed to enable TCP keep-alive for %s: %v", tcpConn.RemoteAddr().String(), err)
				} else {
					s.logger.Tracef("TCP keep-alive enabled for %s", tcpConn.RemoteAddr().String())
				}
				if err := tcpConn.SetKeepAlivePeriod(s.config.KeepAlive); err != nil {
					s.logger.Warnf("failed to set TCP keep-alive period for %s: %v", tcpConn.RemoteAddr().String(), err)
				}

				// try to establish a new channel
				if s.controlChannel == nil {
					s.logger.Info("control channel not found, attempting to establish a new session")
					go s.channelHandshake(conn)
					continue
				}

				select {
				case s.tunnelChannel <- conn:
				default: // The channel is full, do nothing
					s.logger.Warn("tunnel channel is full, discard the connection")
					conn.Close()
				}
			}
		}
	}()
	<-s.ctx.Done()

	close(s.tunnelChannel)
}

func (s *TcpMuxTransport) channelHandshake(conn net.Conn) {
	// Set a read deadline for the token response
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		s.logger.Errorf("failed to set read deadline: %v", err)
		conn.Close()
		return
	}
	msg, err := utils.ReceiveBinaryString(conn)
	if err != nil {
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			s.logger.Warn("timeout while waiting for control channel signal")
		} else {
			s.logger.Errorf("failed to receive control channel signal: %v", err)
		}
		conn.Close() // Close connection on error or timeout
		return
	}

	// Resetting the deadline (removes any existing deadline)
	conn.SetReadDeadline(time.Time{})

	if msg != s.config.Token {
		s.logger.Warnf("invalid security token received: %s", msg)
		return
	}

	err = utils.SendBinaryString(conn, s.config.Token)
	if err != nil {
		s.logger.Errorf("failed to send security token: %v", err)
		return
	}

	s.controlChannel = conn

	s.logger.Info("tcpmux control channel successfully established.")

	// call the functions
	go s.monitorControlChannel()
	go s.portConfigReader()

	s.config.TunnelStatus = "Connected (TCPMux)"
}

func (s *TcpMuxTransport) localListener(localAddr string, remoteAddr string) {
	listener, err := net.Listen("tcp", localAddr)
	if err != nil {
		s.logger.Fatalf("failed to start listener on %s: %v", localAddr, err)
		return
	}

	//close local listener after context cancellation
	defer listener.Close()

	s.logger.Infof("listener started successfully, listening on address: %s", listener.Addr().String())

	// channel
	localChannel := make(chan net.Conn, s.config.ChannelSize)

	// handle channel connections
	go s.handleLocalChan(localChannel, remoteAddr)

	go func() {
		for {
			select {
			case <-s.ctx.Done():
				return

			default:
				s.logger.Debugf("waiting to accept incoming connection on %s", listener.Addr().String())
				conn, err := listener.Accept()
				if err != nil {
					s.logger.Debugf("failed to accept connection on %s: %v", listener.Addr().String(), err)
					continue
				}

				// discard any non-tcp connection
				tcpConn, ok := conn.(*net.TCPConn)
				if !ok {
					s.logger.Warnf("disarded non-TCP connection from %s", conn.RemoteAddr().String())
					conn.Close()
					continue
				}

				// trying to disable tcpnodelay
				if !s.config.Nodelay {
					if err := tcpConn.SetNoDelay(s.config.Nodelay); err != nil {
						s.logger.Warnf("failed to set TCP_NODELAY for %s: %v", tcpConn.RemoteAddr().String(), err)
					} else {
						s.logger.Tracef("TCP_NODELAY disabled for %s", tcpConn.RemoteAddr().String())
					}
				}

				tcpConn.SetKeepAlive(true)
				tcpConn.SetKeepAlivePeriod(s.config.KeepAlive)

				select {
				case localChannel <- conn:
					s.logger.Debugf("accepted incoming TCP connection from %s", tcpConn.RemoteAddr().String())

				default: // channel is full, discard the connection
					s.logger.Warnf("channel with listener %s is full, discarding TCP connection from %s", listener.Addr().String(), tcpConn.LocalAddr().String())
					tcpConn.Close()
				}

			}
		}
	}()

	<-s.ctx.Done()

	close(localChannel)
}

func (s *TcpMuxTransport) handleLocalChan(localChan chan net.Conn, remoteAddr string) {
	for tunnelConn := range s.tunnelChannel {
		session, err := smux.Client(tunnelConn, s.smuxConfig)
		if err != nil {
			s.logger.Errorf("failed to create MUX session for connection %s: %v", tunnelConn.RemoteAddr().String(), err)
			tunnelConn.Close()
			return
		}
		next := make(chan struct{})
		go s.handleSession(session, localChan, remoteAddr, next)
		<-next
	}
}

func (s *TcpMuxTransport) handleSession(session *smux.Session, localChan chan net.Conn, remoteAddr string, next chan struct{}) {
	counter := 0
	done := make(chan struct{}, s.config.MuxCon)

	for incomingConn := range localChan {
		stream, err := session.OpenStream()
		if err != nil {
			s.logger.Errorf("failed to open a new mux stream: %v", err)
			if err := session.Close(); err != nil {
				s.logger.Errorf("failed to close mux stream after openstream error: %v", err)
			}
			localChan <- incomingConn // back to local channel
			close(next)
			return
		}

		// Send the target port over the tunnel connection
		if err := utils.SendBinaryString(stream, remoteAddr); err != nil {
			s.logger.Errorf("failed to send address %v over stream: %v", remoteAddr, err)
			localChan <- incomingConn // back to local channel
			if err := session.Close(); err != nil {
				s.logger.Errorf("failed to close mux stream after sendbinarystring: %v", err)
			}
			close(next)
			return
		}

		// Handle data exchange between connections
		go func() {
			utils.TCPConnectionHandler(stream, incomingConn, s.logger, s.usageMonitor, incomingConn.LocalAddr().(*net.TCPAddr).Port, s.config.Sniffer)
			done <- struct{}{}
		}()

		counter += 1

		if counter == s.config.MuxCon {
			close(next)

			err = utils.SendBinaryString(s.controlChannel, s.chanSignal)
			if err != nil {
				s.logger.Error("error sending channel signal, attempting to restart server...")
				go s.Restart()
				return
			}

			for i := 0; i < s.config.MuxCon; i++ {
				<-done
			}
			close(done)
			if err := session.Close(); err != nil {
				s.logger.Errorf("failed to close mux stream after mux completed: %v", err)
			}
			return
		}
	}
}
