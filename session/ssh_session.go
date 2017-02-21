package session

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"strconv"
	"time"

	msgpack "gopkg.in/vmihailenco/msgpack.v2"

	"github.com/Sirupsen/logrus"
	"github.com/garyburd/redigo/redis"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/xid"
	"github.com/superfly/wormhole/messages"
	"github.com/superfly/wormhole/utils"
	"golang.org/x/crypto/ssh"
)

const (
	sshRemoteForwardRequest      = "tcpip-forward"
	sshForwardedTCPReturnRequest = "forwarded-tcpip"
)

var (
	openSessionsMetric = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "wormhole",
			Subsystem: "ssh_session",
			Name:      "open_sessions_total",
			Help:      "Number of active sessions, partitioned by backend, node and cluster.",
		},
		[]string{
			// Which backend this session belongs to?
			"backend",
			// What wormhole instance this session is running on?
			"node",
			// What region this session belongs to?
			"cluster",
		},
	)

	openChannelsMetric = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "wormhole",
			Subsystem: "ssh_session",
			Name:      "open_channels_total",
			Help:      "Number of active channels, partitioned by backend, node and cluster.",
		},
		[]string{
			// Which backend this channel belongs to?
			"backend",
			// What wormhole instance this session is running on?
			"node",
			// What region this session belongs to?
			"cluster",
		},
	)
)

func init() {
	prometheus.MustRegister(openSessionsMetric)
	prometheus.MustRegister(openChannelsMetric)
}

type SSHSession struct {
	baseSession

	config  *ssh.ServerConfig
	tcpConn net.Conn
	conn    *ssh.ServerConn
	reqs    <-chan *ssh.Request
	chans   <-chan ssh.NewChannel
}

type tcpipForward struct {
	Host string
	Port uint32
}

type directForward struct {
	Host1 string
	Port1 uint32
	Host2 string
	Port2 uint32
}

type instrumentedIO struct {
	rwc        io.ReadWriteCloser
	metricFunc func()
}

func (io instrumentedIO) Close() error {
	go io.metricFunc()
	return io.rwc.Close()
}

func (io instrumentedIO) Read(p []byte) (int, error) {
	return io.rwc.Read(p)
}

func (io instrumentedIO) Write(p []byte) (int, error) {
	return io.rwc.Write(p)
}

// NewSSHSession creates new SshSession struct
func NewSSHSession(logger *logrus.Logger, clusterURL, nodeID string, redisPool *redis.Pool, tcpConn net.Conn, config *ssh.ServerConfig) *SSHSession {
	base := baseSession{
		id:         xid.New().String(),
		nodeID:     nodeID,
		store:      NewRedisStore(redisPool),
		ClusterURL: clusterURL,
		logger:     logger.WithFields(logrus.Fields{"prefix": "SSHSession"}),
	}
	s := &SSHSession{
		tcpConn:     tcpConn,
		baseSession: base,
	}
	config.PasswordCallback = s.authFromToken
	s.config = config
	return s
}

func (s *SSHSession) RequireStream() error {
	// Before use, a handshake must be performed on the incoming net.Conn.
	sshConn, chans, reqs, err := ssh.NewServerConn(s.tcpConn, s.config)
	if err != nil {
		s.logger.Printf("Failed to handshake %s (%s): %s", s.nodeID, s.tcpConn.RemoteAddr(), err)
		return err
	}
	s.conn = sshConn
	s.chans = chans
	s.reqs = reqs
	go handleChannels(chans)
	go openSessionsMetric.With(labels(s)).Add(1)
	return nil
}

func (s *SSHSession) HandleRequests(ln *net.TCPListener) {
	for req := range s.reqs {
		switch req.Type {
		case sshRemoteForwardRequest:
			go func() {
				s.handleRemoteForward(req, ln)
			}()
		case "register-release":
			go s.registerRelease(req)
		case "keepalive":
			go s.handleKeepalive(req)
		}
	}
}

func (s *SSHSession) RequireAuthentication() error {
	// done as a hook to ssh handshake
	go s.RegisterConnection(time.Now())
	return nil
}

func (s *SSHSession) Close() {
	s.RegisterDisconnection()
	s.logger.Infof("Closed session %s for %s (%s).", s.ID(), s.NodeID(), s.Client())
	go func() {
		openSessionsMetric.With(labels(s)).Sub(1)
	}()
	s.conn.Close()
}

func (s *SSHSession) authFromToken(c ssh.ConnMetadata, pass []byte) (*ssh.Permissions, error) {
	backendID, err := s.store.BackendIDFromToken(string(pass))
	if err != nil {
		return nil, err
	}
	if backendID == "" {
		return nil, errors.New("token rejected")
	}
	s.backendID = backendID
	s.agent = string(c.ClientVersion())
	s.id = hex.EncodeToString(c.SessionID())
	s.clientAddr = c.RemoteAddr().String()

	return nil, nil
}

func (s *SSHSession) setSshPort(req *ssh.Request, ln net.Listener) tcpipForward {
	t := tcpipForward{}
	ssh.Unmarshal(req.Payload, &t)

	reply := (t.Port == 0) && req.WantReply

	if reply { // Client sent port 0. let them know which port is actually being used
		_, port, _ := net.SplitHostPort(ln.Addr().String())
		portNum, _ := strconv.Atoi(port)
		b := make([]byte, 4)
		binary.BigEndian.PutUint32(b, uint32(portNum))
		t.Port = uint32(portNum)
		req.Reply(true, b)
	} else {
		req.Reply(true, nil)
	}

	return t
}

func (s *SSHSession) handleRemoteForward(req *ssh.Request, ln *net.TCPListener) {
	defer func() {
		err := ln.Close()
		if err != nil {
			s.logger.Debugf("Couldn't close ingress conn: %s", err)
			return
		}
		s.logger.Debugf("Closed ingress conn: %s", ln.Addr().String())
	}()

	t := s.setSshPort(req, ln)
	p := directForward{}
	quit := make(chan bool)
	go func(ln *net.TCPListener) { // Handle incoming connections on this new listener
		for {
			select {
			case <-quit:
				return
			default:
				ln.SetDeadline(time.Now().Add(time.Second))
				tcpConn, err := ln.AcceptTCP()

				if err != nil {
					netErr, ok := err.(net.Error)

					//If this is a timeout, then continue to wait for
					//new connections
					if ok && netErr.Timeout() && netErr.Temporary() {
						continue
					}
					s.logger.Errorln("Could not accept Ingress TCP conn:", err)
					return
				}
				s.logger.Debugln("Accepted Ingress TCP conn from:", tcpConn.RemoteAddr())

				host, port, err := net.SplitHostPort(tcpConn.RemoteAddr().String())
				if err != nil {
					return
				}
				portnum, err := strconv.Atoi(port)
				if err != nil {
					return
				}

				p.Host1 = t.Host
				p.Port1 = t.Port
				p.Host2 = host
				p.Port2 = uint32(portnum)

				ch, reqs, err := s.conn.OpenChannel(sshForwardedTCPReturnRequest, ssh.Marshal(p))
				if err != nil {
					s.logger.Errorf("Open forwarded Channel error: %s", err.Error())
					return
				}
				go ssh.DiscardRequests(reqs)
				go openChannelsMetric.With(labels(s)).Add(1)
				go func() {
					metricFunc := func() {
						openChannelsMetric.With(labels(s)).Sub(1)
					}
					conn := instrumentedIO{rwc: ch, metricFunc: metricFunc}
					err := utils.CopyCloseIO(conn, tcpConn)
					if err != nil && err != io.EOF {
						s.logger.Error(err)
					}
				}()
			}
		}
	}(ln)

	s.conn.Wait()
	quit <- true
}

func handleChannels(chans <-chan ssh.NewChannel) {
	for _ = range chans {
		// nothing for now.
	}
}

func (s *SSHSession) handleKeepalive(req *ssh.Request) {
	if req.WantReply {
		req.Reply(true, nil)
	}
	go func() {
		if err := s.RegisterHeartbeat(); err != nil {
			s.logger.Warnf("Failed to register session heartbeat: %s", err.Error())
		}
	}()
}

func (s *SSHSession) registerRelease(req *ssh.Request) {
	if req.WantReply {
		req.Reply(true, nil)
	}
	var release messages.Release
	if err := msgpack.Unmarshal(req.Payload, &release); err == nil {
		s.release = &release
		s.store.RegisterRelease(s)
	} else {
		s.logger.Warnf("Couldn't process release info: %s", err.Error())
	}
}

// RegisterConnection ...
func (s *SSHSession) RegisterConnection(t time.Time) error {
	return s.store.RegisterConnection(s)
}

// RegisterDisconnection ...
func (s *SSHSession) RegisterDisconnection() error {
	return s.store.RegisterDisconnection(s)
}

// RegisterEndpoint ...
func (s *SSHSession) RegisterEndpoint() error {
	return s.store.RegisterEndpoint(s)
}

// UpdateAttribute ...
func (s *SSHSession) UpdateAttribute(name string, value interface{}) error {
	return s.store.UpdateAttribute(s, name, value)
}

// RegisterHeartbeat ...
func (s *SSHSession) RegisterHeartbeat() error {
	return s.store.RegisterHeartbeat(s)
}

func labels(s Session) prometheus.Labels {
	return prometheus.Labels{"backend": s.BackendID(),
		"node":    s.NodeID(),
		"cluster": s.Cluster(),
	}
}
