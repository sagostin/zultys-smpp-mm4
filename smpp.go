package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/fiorix/go-smpp/smpp"
	"github.com/fiorix/go-smpp/smpp/pdu"
	"github.com/fiorix/go-smpp/smpp/pdu/pdufield"
)

type SMSMessage struct {
	Source      string
	Destination string
	Content     string
	Client      *Client
	Route       *Route
}

type Route struct {
	Prefix   string
	Type     string // "carrier" or "smpp"
	Endpoint string
	Handler  CarrierHandler
}

// Server is an SMPP server for testing purposes.
type Server struct {
	TLS        *tls.Config
	Handler    HandlerFunc
	Clients    map[string]*Client // Map of Username to Client
	conns      map[string]smpp.Conn
	mu         sync.RWMutex
	l          net.Listener
	smsChannel chan SMSMessage
	routes     []Route
}

// HandlerFunc is the signature of a function that handles PDUs.
type HandlerFunc func(s *Server, c smpp.Conn, m pdu.Body)

// NewServer creates a new Server with default settings.
func NewServer() (*Server, error) {
	clients, err := loadClients()
	if err != nil {
		return nil, fmt.Errorf("failed to load clients: %v", err)
	}

	clientMap := make(map[string]*Client)
	for i := range clients {
		clientMap[clients[i].Username] = &clients[i]
	}

	return &Server{
		Clients:    clientMap,
		Handler:    CustomHandler,
		l:          newLocalListener(),
		conns:      make(map[string]smpp.Conn),
		smsChannel: make(chan SMSMessage, 1000),
		routes:     make([]Route, 0),
	}, nil
}

func newLocalListener() net.Listener {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err == nil {
		return l
	}
	if l, err = net.Listen("tcp6", "[::1]:0"); err != nil {
		panic(fmt.Sprintf("smpptest: failed to listen on a port: %v", err))
	}
	return l
}

// Start starts the server.
func (srv *Server) Start() {
	go srv.Serve()
	go srv.ProcessSMS()
}

// Addr returns the local address of the server.
func (srv *Server) Addr() string {
	if srv.l == nil {
		return ""
	}
	return srv.l.Addr().String()
}

// Close stops the server.
func (srv *Server) Close() {
	if srv.l == nil {
		panic("smpptest: server is not started")
	}
	srv.l.Close()
}

// Serve accepts new clients and handles them.
func (srv *Server) Serve() {
	for {
		cli, err := srv.l.Accept()
		if err != nil {
			break // on srv.l.Close
		}
		c := newConn(cli)
		go srv.handle(c)
	}
}

// BroadcastMessage broadcasts a PDU to all bound clients.
func (srv *Server) BroadcastMessage(p pdu.Body) {
	srv.mu.RLock()
	defer srv.mu.RUnlock()
	for _, c := range srv.conns {
		c.Write(p)
	}
}

// Conn implements a server side connection.
type conn struct {
	rwc net.Conn
	r   *bufio.Reader
	w   *bufio.Writer
}

func newConn(c net.Conn) *conn {
	return &conn{
		rwc: c,
		r:   bufio.NewReader(c),
		w:   bufio.NewWriter(c),
	}
}

func (c *conn) RemoteAddr() net.Addr {
	return c.rwc.RemoteAddr()
}

func (c *conn) Read() (pdu.Body, error) {
	return pdu.Decode(c.r)
}

func (c *conn) Write(p pdu.Body) error {
	var b bytes.Buffer
	err := p.SerializeTo(&b)
	if err != nil {
		return err
	}
	_, err = io.Copy(c.w, &b)
	if err != nil {
		return err
	}
	return c.w.Flush()
}

func (c *conn) Close() error {
	return c.rwc.Close()
}

func (srv *Server) handle(c *conn) {
	defer func() {
		c.Close()
		srv.mu.Lock()
		for username, conn := range srv.conns {
			if conn == c {
				delete(srv.conns, username)
				break
			}
		}
		srv.mu.Unlock()
	}()
	if err := srv.auth(c); err != nil {
		if err != io.EOF {
			log.Println("smpptest: server auth failed:", err)
		}
		return
	}
	for {
		p, err := c.Read()
		if err != nil {
			if err != io.EOF {
				log.Println("smpptest: read failed:", err)
			}
			break
		}
		srv.Handler(srv, c, p)
	}
}

func (srv *Server) auth(c *conn) error {
	p, err := c.Read()
	if err != nil {
		return err
	}
	var resp pdu.Body
	switch p.Header().ID {
	case pdu.BindTransmitterID:
		resp = pdu.NewBindTransmitterResp()
	case pdu.BindReceiverID:
		resp = pdu.NewBindReceiverResp()
	case pdu.BindTransceiverID:
		resp = pdu.NewBindTransceiverResp()
	default:
		return errors.New("unexpected pdu, want bind")
	}
	f := p.Fields()
	username := f[pdufield.SystemID].String()
	password := f[pdufield.Password].String()
	if username == "" || password == "" {
		return errors.New("malformed pdu, missing system_id/password")
	}

	authed, err := authClient(username, password, srv.Clients)
	if err != nil {
		return err
	}
	if !authed {
		return errors.New("authentication failed")
	}

	srv.mu.Lock()
	srv.conns[username] = c
	srv.mu.Unlock()

	resp.Fields().Set(pdufield.SystemID, username)
	return c.Write(resp)
}

func CustomHandler(s *Server, c smpp.Conn, m pdu.Body) {
	switch m.Header().ID {
	case pdu.SubmitSMID:
		handleSubmitSM(s, c, m)
	default:
		log.Printf("Received PDU: %s", m.Header().ID)
		err := c.Write(m)
		if err != nil {
			log.Printf("Error writing PDU: %v", err)
		}
	}
}

func handleSubmitSM(s *Server, c smpp.Conn, m pdu.Body) {
	f := m.Fields()
	sourceAddr := f[pdufield.SourceAddr].String()
	destAddr := f[pdufield.DestinationAddr].String()
	shortMessage := f[pdufield.ShortMessage].String()

	// Find the client associated with this connection
	s.mu.RLock()
	var client *Client
	for username, conn := range s.conns {
		if conn == c {
			client = s.Clients[username]
			break
		}
	}
	s.mu.RUnlock()

	if client == nil {
		log.Printf("Error: Unable to identify client for connection")
		return
	}

	log.Printf("Received SubmitSM from client %s: From=%s, To=%s, Message=%s", client.Username, sourceAddr, destAddr, shortMessage)

	route := s.findRoute(sourceAddr, destAddr)
	if route == nil {
		log.Printf("No route found for source %s and destination %s", sourceAddr, destAddr)
		// Handle the case when no route is found (e.g., send an error response)
		return
	}

	// Send to channel for async processing
	s.smsChannel <- SMSMessage{
		Source:      sourceAddr,
		Destination: destAddr,
		Content:     shortMessage,
		Client:      client,
		Route:       route,
	}

	resp := pdu.NewSubmitSMResp()
	resp.Header().Seq = m.Header().Seq
	err := resp.Fields().Set(pdufield.MessageID, fmt.Sprintf("%d", time.Now().UnixNano()))
	if err != nil {
		log.Printf("Error setting MessageID: %v", err)
		return
	}
	err = c.Write(resp)
	if err != nil {
		log.Printf("Error sending SubmitSMResp: %v", err)
	}
}

func (srv *Server) findRoute(source, destination string) *Route {
	carrier, err := srv.clientOutboundCarrier(source)
	if err != nil {
		log.Printf("Error finding carrier: %v", err)
		return nil
	}

	if carrier != "" {
		for _, route := range srv.routes {
			if route.Type == "carrier" && route.Endpoint == carrier {
				return &route
			}
		}
	}

	// Fallback to prefix-based routing if no carrier route found
	for _, route := range srv.routes {
		if strings.HasPrefix(destination, route.Prefix) {
			return &route
		}
	}

	return nil
}

func (srv *Server) clientOutboundCarrier(source string) (string, error) {
	for _, client := range srv.Clients {
		for _, num := range client.Numbers {
			if strings.Contains(num.Number, source) {
				return num.Carrier, nil
			}
		}
	}

	return "", nil
}

func (srv *Server) AddRoute(prefix, routeType, endpoint string, handler CarrierHandler) {
	srv.routes = append(srv.routes, Route{Prefix: prefix, Type: routeType, Endpoint: endpoint, Handler: handler})
}

func (srv *Server) ProcessSMS() {
	for msg := range srv.smsChannel {
		go func(m SMSMessage) {
			if m.Route == nil {
				log.Printf("No route found for message: From=%s, To=%s", m.Source, m.Destination)
				return
			}

			switch m.Route.Type {
			case "carrier":
				log.Printf("Sending SMS via carrier: %s", m.Route.Endpoint)
				// Implement carrier-specific logic here

				switch m.Route.Endpoint {
				case "twilio":
					sms := SMS{
						From:        m.Source,
						To:          m.Destination,
						Content:     m.Content,
						CarrierData: nil,
					}

					err := m.Route.Handler.SendSMS(&sms)
					if err != nil {
						log.Printf(err.Error())
						return
					}
				default:
					log.Printf("error sending to carrier")
				}
			case "smpp":
				log.Printf("Sending SMS via SMPP: %s", m.Route.Endpoint)
				// Implement SMPP client logic here
			default:
				log.Printf("Unknown route type: %s", m.Route.Type)
			}
		}(msg)
	}
}
