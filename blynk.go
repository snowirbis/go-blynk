package blynk

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"time"

	certs "github.com/OloloevReal/go-blynk/certs"
	slog "github.com/OloloevReal/go-simple-log"
)

const Version = "0.0.7"

type Blynk struct {
	APIkey          string
	server          string
	port            int
	OnReadFunc      func(*BlynkRespose)
	conn            net.Conn
	msgID           uint16
	processingUsing bool
	disableLogo     bool
	heartbeat       time.Duration
	timeout         time.Duration
	timeoutMAX      time.Duration
	lock            sync.Mutex
	ssl             bool
	cancel          chan bool
	readers         map[uint]func(uint, io.Writer)
	writers         map[uint]func(uint, io.Reader)
	recvMsg         chan []byte
}

func NewBlynk(APIkey string) *Blynk {
	return &Blynk{APIkey: APIkey,
		server:          "blynk-cloud.com",
		port:            443,
		conn:            nil,
		msgID:           0,
		processingUsing: false,
		disableLogo:     false,
		heartbeat:       time.Second * 10,
		timeout:         time.Millisecond * 50,
		timeoutMAX:      time.Second * 5,
		lock:            sync.Mutex{},
		ssl:             true,
		cancel:          make(chan bool, 1),
		writers:         make(map[uint]func(uint, io.Reader)),
		readers:         make(map[uint]func(uint, io.Writer)),
		recvMsg:         make(chan []byte, 10),
	}
}

func (g *Blynk) SetUseSSL(ssl bool) {
	g.ssl = ssl
	if !ssl {
		g.port = 80
	} else {
		g.port = 443
	}
}

func (g *Blynk) SetServer(Server string, Port int, SSL bool) {
	g.server = Server
	g.port = Port
	g.ssl = SSL
}

func (g *Blynk) SetDebug() {
	slog.SetOptions(slog.SetDebug)
}

func (g *Blynk) DisableLogo(state bool) {
	g.disableLogo = state
}

func (g *Blynk) printLogo() {
	if g.disableLogo {
		return
	}

	logo := `
     ___  __          __
    / _ )/ /_ _____  / /__
   / _  / / // / _ \/  '_/
  /____/_/\_, /_//_/_/\_\
         /___/ for Go v%s (%s)


`
	slog.Printf(logo, Version, runtime.GOOS)
}

func (g *Blynk) AddReaderHandler(pin uint, fn func(pin uint, writer io.Writer)) {
	g.lock.Lock()
	defer g.lock.Unlock()
	g.readers[pin] = fn
}

func (g *Blynk) DeleteReaderHandler(pin uint) {
	g.lock.Lock()
	defer g.lock.Unlock()
	delete(g.readers, pin)
}

func (g *Blynk) AddWriterHandler(pin uint, fn func(pin uint, reader io.Reader)) {
	g.lock.Lock()
	defer g.lock.Unlock()
	g.writers[pin] = fn
}

func (g *Blynk) DeleteWriterHandler(pin uint) {
	g.lock.Lock()
	defer g.lock.Unlock()
	delete(g.writers, pin)
}

func (g *Blynk) Connect() error {

	g.printLogo()

	addr, err := net.ResolveTCPAddr("tcp", fmt.Sprintf("%s:%d", g.server, g.port))
	if err != nil {
		return err
	}

	if g.ssl {
		g.conn, err = g.dialTLS(addr)
	} else {
		g.conn, err = net.DialTCP("tcp", nil, addr)
	}

	if err != nil {
		return err
	}
	//defer conn.Close()

	if err = g.auth(); err != nil {
		return err
	}
	slog.Printf("Connect: Auth success (SSL: %v)", g.ssl)

	g.sendInternal()
	return nil
}

func (g *Blynk) dialTLS(addr *net.TCPAddr) (*tls.Conn, error) {
	roots := x509.NewCertPool()
	rootPEM, err := g.loadCA()
	if err != nil {
		return nil, err
	}
	ok := roots.AppendCertsFromPEM(rootPEM)
	if !ok {
		return nil, fmt.Errorf("failed to parse root certificate")
	}

	//w := os.Stdout
	conf := tls.Config{
		InsecureSkipVerify:     false,
		MinVersion:             tls.VersionTLS12,
		RootCAs:                roots,
		ServerName:             g.server,
		SessionTicketsDisabled: true,
		//KeyLogWriter:           w,
	}
	conn, err := tls.Dial("tcp", addr.String(), &conf)
	return conn, err
}

func (g *Blynk) loadCA() ([]byte, error) {
	return []byte(certs.CertServer), nil
}

func (g *Blynk) Processing() {
	g.processingUsing = true
	defer func() { g.processingUsing = false }()
	go g.keepAlive()
	go g.processor()
	g.receiver()
}

func (g *Blynk) getMessageID() uint16 {
	g.lock.Lock()
	defer g.lock.Unlock()
	g.msgID++
	if g.msgID > 0xFFFF {
		g.msgID = 1
	}
	return g.msgID
}

func (g *Blynk) auth() error {
	_, err := g.sendString(BLYNK_CMD_HW_LOGIN, g.APIkey)
	if err != nil {
		return err
	}

	response, err := g.receiveMessage(g.timeoutMAX)
	if err != nil {
		return err
	}

	if response != nil && (response.MessageId != g.msgID || response.Command != BLYNK_CMD_RESPONSE || response.Length != BLYNK_SUCCESS) {
		return fmt.Errorf("auth: failed, message id-%d, code-%d", response.MessageId, response.Length)
	}
	return nil
}

func (g *Blynk) sendInternal() error {
	if _, err := g.sendString(BLYNK_CMD_INTERNAL, g.formatInternal()); err != nil {
		return err
	}

	resp, err := g.receiveMessage(g.timeoutMAX)
	if err != nil {
		return err
	}

	if resp.Length != BLYNK_SUCCESS {
		return fmt.Errorf("sendInternal: received unsuccessful code %d", resp.Length)
	}

	return nil
}

func (g *Blynk) formatInternal() string {
	rcv_buffer := "1024"
	params := []string{"ver", Version, "buff-in", rcv_buffer, "h-beat", fmt.Sprintf("%.0f", g.heartbeat.Seconds()), "dev", "go"}
	return strings.Join(params, string(0x00))
}

func (g *Blynk) keepAlive() {
	slog.Printf("Keep-Alive: started")
	defer slog.Printf("Keep-Alive: finished")
	t := time.NewTicker(g.heartbeat)
	for {
		select {
		case <-t.C:
			slog.Printf("[DEBUG] Keep-Alive: send")
			g.sendCommand(BLYNK_CMD_PING)
		case <-g.cancel:
			slog.Printf("[DEBUG] Keep-Alive: Stop received")
			t.Stop()
			return
		}
	}
}

func (g *Blynk) VirtualWrite(pin int, value string) error {
	msg := BlynkMessage{}
	msg.Head.Command = BLYNK_CMD_HARDWARE
	msg.Head.MessageId = g.getMessageID()
	msg.Body.AddString("vw")
	msg.Body.AddInt(pin)
	msg.Body.AddString(value)
	msg.Head.Length = msg.Body.Len()

	if _, err := g.sendMessage(msg); err != nil {
		return err
	}
	return nil
}

func (g *Blynk) VirtualRead(pins ...int) error {
	msg := BlynkMessage{}
	msg.Head.Command = BLYNK_CMD_HARDWARE_SYNC
	msg.Head.MessageId = g.getMessageID()
	msg.Body.AddString("vr")
	msg.Body.AddInt(pins...)
	msg.Head.Length = msg.Body.Len()

	if _, err := g.sendMessage(msg); err != nil {
		return err
	}

	return nil
}

func (g *Blynk) DigitalWrite(pin int, value bool) error {
	msg := BlynkMessage{}
	msg.Head.Command = BLYNK_CMD_HARDWARE
	msg.Head.MessageId = g.getMessageID()
	msg.Body.AddString("dw")
	msg.Body.AddInt(pin)
	msg.Body.AddBool(value)
	msg.Head.Length = msg.Body.Len()

	if _, err := g.sendMessage(msg); err != nil {
		return err
	}
	return nil
}

func (g *Blynk) DigitalRead(pin int) error {
	msg := BlynkMessage{}
	msg.Head.Command = BLYNK_CMD_HARDWARE_SYNC
	msg.Head.MessageId = g.getMessageID()
	msg.Body.AddString("dr")
	msg.Body.AddInt(pin)
	msg.Head.Length = msg.Body.Len()

	if _, err := g.sendMessage(msg); err != nil {
		return err
	}

	return nil
}

func (g *Blynk) Notify(msg string) error {
	_, err := g.sendString(BLYNK_CMD_NOTIFY, msg)
	if err != nil {
		return fmt.Errorf("send notify failed, %s", err.Error())
	}

	//if receiver is using dont use standalone receive func
	if g.processingUsing {
		return err
	}

	bh, err := g.receiveMessage(time.Duration(time.Second * 5))
	if err != nil {
		return err
	}
	if bh.Length != BLYNK_SUCCESS {
		return fmt.Errorf("notify failed, cause: %s (%d)", GetBlynkStatus(bh.Length), bh.Length)
	}

	return nil
}

func (g *Blynk) Tweet(msg string) error {
	_, err := g.sendString(BLYNK_CMD_TWEET, msg)
	if err != nil {
		return fmt.Errorf("send tweet failed, %s", err.Error())
	}

	if g.processingUsing {
		return err
	}

	bh, err := g.receiveMessage(time.Duration(time.Second * 5))
	if err != nil {
		return err
	}
	if bh.Length != BLYNK_SUCCESS {
		return fmt.Errorf("tweet failed, cause: %s (%d)", GetBlynkStatus(bh.Length), bh.Length)
	}

	return nil
}

func (g *Blynk) EMail(to string, subject string, msg string) error {

	bmsg := BlynkMessage{}
	bmsg.Head.MessageId = g.getMessageID()
	bmsg.Head.Command = BLYNK_CMD_EMAIL
	bmsg.Body.AddString(to)
	bmsg.Body.AddString(subject)
	bmsg.Body.AddString(msg)
	bmsg.Head.Length = bmsg.Body.Len()

	_, err := g.sendMessage(bmsg)

	//if receiver is using dont use standalone receive func
	if g.processingUsing {
		return err
	}

	bh, err := g.receiveMessage(time.Duration(time.Second * 5))
	if err != nil {
		return err
	}
	if bh.Length != BLYNK_SUCCESS {
		return fmt.Errorf("email failed, cause: %s (%d)", GetBlynkStatus(bh.Length), bh.Length)
	}

	return nil
}

func (g *Blynk) Stop() error {
	if g == nil {
		return fmt.Errorf("Blynk: source object blynk is nil")
	}
	slog.Printf("[DEBUG] Sending to cancle channel")
	g.conn.SetReadDeadline(time.Now().Add(time.Millisecond * 500))
	close(g.cancel)
	close(g.recvMsg)
	time.Sleep(time.Second * 1)
	return g.Disconnect()
}

func (g *Blynk) Disconnect() error {
	if g == nil || g.conn == nil {
		return fmt.Errorf("disconnect: *Blynk or *net.TCPConn is nil")
	}
	err := g.conn.Close()
	return err
}
