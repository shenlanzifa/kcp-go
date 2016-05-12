package kcp

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"math/rand"
	"net"
	"runtime"
	"sync"
	"time"
	"unsafe"
)

var (
	errTimeout    = errors.New("i/o timeout")
	errBrokenPipe = errors.New("broken pipe")
	initialVector = []byte{167, 115, 79, 156, 18, 172, 27, 1, 164, 21, 242, 193, 252, 120, 230, 107}
	xor           XORFunc
)

func init() {
	xor = safeXORBytes
	if supportsUnaligned {
		xor = fastXORWords
	}
}

// Mode specifies the working mode of kcp
type Mode int

// XORFunc is the prototype of an xor function for cryptography
type XORFunc func(a, b []byte)

const (
	// MODE_DEFAULT slowest
	MODE_DEFAULT Mode = iota
	// MODE_NORMAL normal kcp mode, faster
	MODE_NORMAL
	// MODE_FAST fastest mode
	MODE_FAST
)

const (
	wordSize          = int(unsafe.Sizeof(uintptr(0)))
	supportsUnaligned = runtime.GOARCH == "386" || runtime.GOARCH == "amd64"
)

const (
	basePort       = 20000 // minimum port for listening
	maxPort        = 65535 // maximum port for listening
	defaultWndSize = 128   // default window size, in packet
	headerSize     = aes.BlockSize + md5.Size
)

type (
	// UDPSession defines a KCP session implemented by UDP
	UDPSession struct {
		kcp           *KCP         // the core ARQ
		conn          *net.UDPConn // the underlying UDP socket
		block         cipher.Block
		l             *Listener // point to server listener if it's a server socket
		local, remote net.Addr
		rd            time.Time // read deadline
		sockbuff      []byte    // kcp receiving is based on packet, I turn it into stream
		die           chan struct{}
		isClosed      bool
		needUpdate    bool
		mu            sync.Mutex
		chReadEvent   chan bool
		chTicker      chan time.Time
		chUDPOutput   chan []byte
	}
)

// newUDPSession create a new udp session for client or server
func newUDPSession(conv uint32, mode Mode, l *Listener, conn *net.UDPConn, remote *net.UDPAddr, block cipher.Block) *UDPSession {
	sess := new(UDPSession)
	sess.chTicker = make(chan time.Time, 1)
	sess.chUDPOutput = make(chan []byte, defaultWndSize)
	sess.die = make(chan struct{})
	sess.local = conn.LocalAddr()
	sess.chReadEvent = make(chan bool, 1)
	sess.remote = remote
	sess.conn = conn
	sess.l = l
	sess.block = block
	sess.kcp = NewKCP(conv, func(buf []byte, size int) {
		if size >= IKCP_OVERHEAD {
			if sess.block != nil {
				ext := make([]byte, headerSize+size)
				copy(ext[headerSize:], buf)
				sess.chUDPOutput <- ext
			} else {
				ext := make([]byte, size)
				copy(ext, buf)
				sess.chUDPOutput <- ext
			}
		}
	})
	sess.kcp.WndSize(defaultWndSize, defaultWndSize)
	if block != nil {
		sess.kcp.SetMtu(IKCP_MTU_DEF - headerSize)
	} else {
		sess.kcp.SetMtu(IKCP_MTU_DEF)
	}

	switch mode {
	case MODE_FAST:
		sess.kcp.NoDelay(1, 10, 2, 1)
	case MODE_NORMAL:
		sess.kcp.NoDelay(0, 20, 0, 1)
	default:
		sess.kcp.NoDelay(0, 40, 0, 0)
	}

	go sess.updateTask()
	go sess.outputTask()
	if l == nil { // it's a client connection
		go sess.readLoop()
	}
	return sess
}

// Read implements the Conn Read method.
func (s *UDPSession) Read(b []byte) (n int, err error) {
	for {
		s.mu.Lock()
		if len(s.sockbuff) > 0 { // copy from buffer
			n := copy(b, s.sockbuff)
			s.sockbuff = s.sockbuff[n:]
			s.mu.Unlock()
			return n, nil
		}

		if s.isClosed {
			s.mu.Unlock()
			return 0, errBrokenPipe
		}

		if !s.rd.IsZero() {
			if time.Now().After(s.rd) { // timeout
				s.mu.Unlock()
				return 0, errTimeout
			}
		}

		if n := s.kcp.PeekSize(); n > 0 { // data arrived
			buf := make([]byte, n)
			if s.kcp.Recv(buf) > 0 { // if Recv() succeeded
				n := copy(b, buf)
				s.sockbuff = buf[n:] // store remaining bytes into sockbuff for next read
				s.mu.Unlock()
				return n, nil
			}
		}
		s.mu.Unlock()

		// wait for read event or timeout
		select {
		case <-s.chReadEvent:
		case <-time.After(1 * time.Second):
		}
	}
}

// Write implements the Conn Write method.
func (s *UDPSession) Write(b []byte) (n int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.isClosed {
		return 0, errBrokenPipe
	}

	n = len(b)
	max := int(s.kcp.mss * 255)
	if s.kcp.snd_wnd < 255 {
		max = int(s.kcp.mss * s.kcp.snd_wnd)
	}
	for {
		if len(b) <= max { // in most cases
			s.kcp.Send(b)
			break
		} else {
			s.kcp.Send(b[:max])
			b = b[max:]
		}
	}
	s.needUpdate = true
	return
}

// Close closes the connection.
func (s *UDPSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.isClosed {
		return errBrokenPipe
	}
	close(s.die)
	s.isClosed = true
	if s.l == nil { // client socket close
		s.conn.Close()
	}
	return nil
}

// LocalAddr returns the local network address. The Addr returned is shared by all invocations of LocalAddr, so do not modify it.
func (s *UDPSession) LocalAddr() net.Addr {
	return s.local
}

// RemoteAddr returns the remote network address. The Addr returned is shared by all invocations of RemoteAddr, so do not modify it.
func (s *UDPSession) RemoteAddr() net.Addr { return s.remote }

// SetDeadline sets the deadline associated with the listener. A zero time value disables the deadline.
func (s *UDPSession) SetDeadline(t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rd = t
	return nil
}

// SetReadDeadline implements the Conn SetReadDeadline method.
func (s *UDPSession) SetReadDeadline(t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rd = t
	return nil
}

// SetWriteDeadline implements the Conn SetWriteDeadline method.
func (s *UDPSession) SetWriteDeadline(t time.Time) error {
	return nil
}

// SetWindowSize set maximum window size
func (s *UDPSession) SetWindowSize(sndwnd, rcvwnd int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kcp.WndSize(sndwnd, rcvwnd)
}

// SetMtu sets the maximum transmission unit
func (s *UDPSession) SetMtu(mtu int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.block != nil {
		s.kcp.SetMtu(mtu - headerSize)
	} else {
		s.kcp.SetMtu(mtu)
	}
}

// SetRetries influences the timeout of an alive KCP connection,
// when RTO retransmissions remain unacknowledged.
// default is 10, the total timeout is calculated as:
// (1+1.5+...+5.5) * 200ms = 200ms * 10 * (2*1 +(10-1)*0.5)/2 = 6.5s
func (s *UDPSession) SetRetries(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kcp.dead_link = uint32(n)
}

func (s *UDPSession) outputTask() {
	for {
		select {
		case ext := <-s.chUDPOutput:
			if s.block != nil {
				io.ReadFull(crand.Reader, ext[:aes.BlockSize]) // OTP
				checksum := md5.Sum(ext[headerSize:])
				copy(ext[aes.BlockSize:], checksum[:])
				encrypt(s.block, ext)
			}
			n, err := s.conn.WriteTo(ext, s.remote)
			if err != nil {
				log.Println(err, n)
			}
		case <-s.die:
			return
		}
	}
}

// kcp update, input loop
func (s *UDPSession) updateTask() {
	var tc <-chan time.Time
	if s.l == nil { // client
		ticker := time.NewTicker(10 * time.Millisecond)
		tc = ticker.C
		defer ticker.Stop()
	} else {
		tc = s.chTicker
	}

	var nextupdate uint32
	for {
		select {
		case now := <-tc:
			current := uint32(now.UnixNano() / int64(time.Millisecond))
			s.mu.Lock()
			if current >= nextupdate || s.needUpdate {
				s.kcp.Update(current)
				nextupdate = s.kcp.Check(current)
			}
			s.needUpdate = false
			state := s.kcp.state
			s.mu.Unlock()
			if state != 0 { // deadlink
				s.Close()
			}
		case <-s.die:
			if s.l != nil { // has listener
				s.l.chDeadlinks <- s.remote
			}
			return
		}
	}
}

// GetConv gets conversation id of a session
func (s *UDPSession) GetConv() uint32 {
	return s.kcp.conv
}

func (s *UDPSession) notifyReadEvent() {
	select {
	case s.chReadEvent <- true:
	default:
	}
}

func (s *UDPSession) kcpInput(data []byte) {
	s.mu.Lock()
	n := s.kcp.Input(data)
	s.needUpdate = true
	s.mu.Unlock()
	if n == 0 {
		s.notifyReadEvent()
	}
}

// read loop for client session
func (s *UDPSession) readLoop() {
	conn := s.conn
	buffer := make([]byte, 4096)
	for {
		if n, err := conn.Read(buffer); err == nil && n >= IKCP_OVERHEAD {
			dataValid := false
			data := buffer[:n]
			if s.block != nil && n >= IKCP_OVERHEAD+headerSize {
				decrypt(s.block, data)
				data = data[aes.BlockSize:]
				checksum := md5.Sum(data[md5.Size:])
				if bytes.Equal(checksum[:], data[:md5.Size]) {
					data = data[md5.Size:]
					dataValid = true
				}
			} else if s.block == nil {
				dataValid = true
			}

			if dataValid {
				s.kcpInput(data)
			}
		} else {
			return
		}
	}
}

type (
	// Listener defines a server listening for connections
	Listener struct {
		block       cipher.Block
		conn        *net.UDPConn
		mode        Mode
		sessions    map[string]*UDPSession
		chAccepts   chan *UDPSession
		chDeadlinks chan net.Addr
		die         chan struct{}
	}

	packet struct {
		from *net.UDPAddr
		data []byte
	}
)

// monitor incoming data for all connections of server
func (l *Listener) monitor() {
	chPacket := make(chan packet, 65535)
	go l.receiver(chPacket)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case p := <-chPacket:
			data := p.data
			from := p.from
			dataValid := false
			if l.block != nil && len(data) >= IKCP_OVERHEAD+headerSize {
				decrypt(l.block, data)
				data = data[aes.BlockSize:]
				checksum := md5.Sum(data[md5.Size:])
				if bytes.Equal(checksum[:], data[:md5.Size]) {
					data = data[md5.Size:]
					dataValid = true
				}
			} else if l.block == nil {
				dataValid = true
			}

			if dataValid {
				addr := from.String()
				s, ok := l.sessions[addr]
				if !ok {
					conv := binary.LittleEndian.Uint32(data)
					s := newUDPSession(conv, l.mode, l, l.conn, from, l.block)
					s.kcpInput(data)
					l.sessions[addr] = s
					l.chAccepts <- s
				} else {
					s.kcpInput(data)
				}
			}
		case deadlink := <-l.chDeadlinks:
			delete(l.sessions, deadlink.String())
		case <-l.die:
			return
		case <-ticker.C:
			now := time.Now()
			for _, s := range l.sessions {
				select {
				case s.chTicker <- now:
				default:
				}
			}
		}
	}
}

func (l *Listener) receiver(ch chan packet) {
	for {
		data := make([]byte, 4096)
		if n, from, err := l.conn.ReadFromUDP(data); err == nil && n >= IKCP_OVERHEAD {
			ch <- packet{from, data[:n]}
		} else {
			return
		}
	}
}

// Accept implements the Accept method in the Listener interface; it waits for the next call and returns a generic Conn.
func (l *Listener) Accept() (*UDPSession, error) {
	select {
	case c := <-l.chAccepts:
		return c, nil
	case <-l.die:
		return nil, errors.New("listener stopped")
	}
}

// Close stops listening on the UDP address. Already Accepted connections are not closed.
func (l *Listener) Close() error {
	if err := l.conn.Close(); err == nil {
		close(l.die)
		return nil
	} else {
		return err
	}
}

// Addr returns the listener's network address, The Addr returned is shared by all invocations of Addr, so do not modify it.
func (l *Listener) Addr() net.Addr {
	return l.conn.LocalAddr()
}

// Listen listens for incoming KCP packets addressed to the local address laddr on the network "udp",
// mode must be one of: MODE_DEFAULT,MODE_NORMAL,MODE_FAST
func Listen(mode Mode, laddr string) (*Listener, error) {
	return ListenEncrypted(mode, laddr, nil)
}

// ListenEncrypted listens for incoming KCP packets addressed to the local address laddr on the network "udp" with packet encryption,
// mode must be one of: MODE_DEFAULT,MODE_NORMAL,MODE_FAST
func ListenEncrypted(mode Mode, laddr string, key []byte) (*Listener, error) {
	udpaddr, err := net.ResolveUDPAddr("udp", laddr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", udpaddr)
	if err != nil {
		return nil, err
	}

	l := new(Listener)
	l.conn = conn
	l.mode = mode
	l.sessions = make(map[string]*UDPSession)
	l.chAccepts = make(chan *UDPSession, 1024)
	l.chDeadlinks = make(chan net.Addr, 1024)
	l.die = make(chan struct{})
	if key != nil {
		pass := sha256.Sum256(key)
		if block, err := aes.NewCipher(pass[:]); err == nil {
			l.block = block
		} else {
			log.Println(err)
		}
	}
	go l.monitor()
	return l, nil
}

// Dial connects to the remote address raddr on the network "udp", mode is same as Listen
func Dial(mode Mode, raddr string) (*UDPSession, error) {
	return DialEncrypted(mode, raddr, nil)
}

// DialEncrypted connects to the remote address raddr on the network "udp" with packet encryption, mode is same as Listen
func DialEncrypted(mode Mode, raddr string, key []byte) (*UDPSession, error) {
	udpaddr, err := net.ResolveUDPAddr("udp", raddr)
	if err != nil {
		return nil, err
	}

	for {
		port := basePort + rand.Int()%(maxPort-basePort)
		if udpconn, err := net.ListenUDP("udp", &net.UDPAddr{Port: port}); err == nil {
			if key != nil {
				pass := sha256.Sum256(key)
				if block, err := aes.NewCipher(pass[:]); err == nil {
					return newUDPSession(rand.Uint32(), mode, nil, udpconn, udpaddr, block), nil
				} else {
					log.Println(err)
				}
			}
			return newUDPSession(rand.Uint32(), mode, nil, udpconn, udpaddr, nil), nil
		}
	}
}

// packet encryption with local CFB mode
func encrypt(block cipher.Block, data []byte) {
	tbl := make([]byte, aes.BlockSize)
	block.Encrypt(tbl, initialVector)
	n := len(data) / aes.BlockSize
	for i := 0; i < n; i++ {
		base := i * aes.BlockSize
		xor(data[base:], tbl)
		block.Encrypt(tbl, data[base:])
	}

	for j := n * aes.BlockSize; j < len(data); j++ {
		data[j] = data[j] ^ tbl[j%aes.BlockSize]
	}
}

func decrypt(block cipher.Block, data []byte) {
	tbl := make([]byte, aes.BlockSize)
	next := make([]byte, aes.BlockSize)
	block.Encrypt(tbl, initialVector)
	n := len(data) / aes.BlockSize
	for i := 0; i < n; i++ {
		base := i * aes.BlockSize
		block.Encrypt(next, data[base:])
		xor(data[base:], tbl)
		copy(tbl, next)
	}

	for j := n * aes.BlockSize; j < len(data); j++ {
		data[j] = data[j] ^ tbl[j%aes.BlockSize]
	}
}

func fastXORWords(a, b []byte) {
	aw := *(*[]uintptr)(unsafe.Pointer(&a))
	bw := *(*[]uintptr)(unsafe.Pointer(&b))
	n := len(b) / wordSize
	for i := 0; i < n; i++ {
		aw[i] = aw[i] ^ bw[i]
	}
}

func safeXORBytes(a, b []byte) {
	n := len(b)
	for i := 0; i < n; i++ {
		a[i] = a[i] ^ b[i]
	}
}
