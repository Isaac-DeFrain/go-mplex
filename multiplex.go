package multiplex

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"sync"
)

const (
	NewStream = iota
	Receiver
	Initiator
	Unknown
	Close
)

var _ = ioutil.ReadAll
var _ = bufio.NewReadWriter
var _ = binary.MaxVarintLen16

type msg struct {
	header uint64
	data   []byte
	err    chan<- error
}

type Stream struct {
	id       uint64
	name     string
	header   uint64
	closed   chan struct{}
	data_in  chan []byte
	data_out chan<- msg
	buf      *bytes.Buffer
}

func newStream(id uint64, name string, initiator bool, send chan<- msg) *Stream {
	var hfn uint64
	if initiator {
		hfn = 2
	} else {
		hfn = 1
	}
	return &Stream{
		id:       id,
		name:     name,
		header:   (id << 3) | hfn,
		data_in:  make(chan []byte, 8),
		data_out: send,
		closed:   make(chan struct{}),
		buf:      new(bytes.Buffer),
	}
}

func (s *Stream) Name() string {
	return s.name
}

func (s *Stream) receive(b []byte) {
	select {
	case s.data_in <- b:
	case <-s.closed:
	}
}

func (s *Stream) Read(b []byte) (int, error) {
	select {
	case <-s.closed:
		return 0, io.EOF
	default:
	}

	lb := len(b)
	nread := 0

	if s.buf.Len() > 0 {
		n, err := s.buf.Read(b)
		if err != nil {
			return 0, err
		}

		if n >= len(b) {
			return n, nil
		}
		b = b[n:]
		nread += n
	}
	for len(b) > 0 {
		select {
		case data, ok := <-s.data_in:
			if !ok {
				return nread, nil
			}
			n := copy(b, data)
			if n < len(data) {
				s.buf.Write(data[n:])
				return lb, nil
			}
			b = b[n:]
			nread += n
		case <-s.closed:
			return nread, nil
		}
	}
	return lb, nil
}

func (s *Stream) Write(b []byte) (int, error) {
	select {
	case s.data_out <- msg{header: s.header, data: b}:
		return len(b), nil
	case <-s.closed:
		return 0, errors.New("stream closed")
	}
}

func (s *Stream) Close() error {
	select {
	case <-s.closed:
		return nil
	default:
		close(s.closed)
		select {
		case s.data_out <- msg{
			header: (s.id << 3) | Close,
		}:
		default:
		}
		close(s.data_in)
		return nil
	}
}

type Multiplex struct {
	con       io.ReadWriteCloser
	buf       *bufio.Reader
	nextID    uint64
	outchan   chan msg
	closed    chan struct{}
	initiator bool

	channels map[uint64]*Stream
	ch_lock  sync.Mutex
}

func NewMultiplex(con io.ReadWriteCloser, initiator bool) *Multiplex {
	return &Multiplex{
		con:       con,
		initiator: initiator,
		buf:       bufio.NewReader(con),
		channels:  make(map[uint64]*Stream),
		outchan:   make(chan msg),
		closed:    make(chan struct{}),
	}
}

func (mp *Multiplex) Close() error {
	if mp.IsClosed() {
		return nil
	}
	close(mp.closed)
	mp.ch_lock.Lock()
	defer mp.ch_lock.Unlock()
	for _, s := range mp.channels {
		err := s.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (mp *Multiplex) IsClosed() bool {
	select {
	case <-mp.closed:
		return true
	default:
		return false
	}
}

func (mp *Multiplex) handleOutgoing() {
	for {
		select {
		case msg, ok := <-mp.outchan:
			if !ok {
				return
			}

			buf := EncodeVarint(msg.header)
			_, err := mp.con.Write(buf)
			if err != nil {
				panic(err)
			}

			buf = EncodeVarint(uint64(len(msg.data)))
			_, err = mp.con.Write(buf)
			if err != nil {
				panic(err)
			}

			_, err = mp.con.Write(msg.data)
			if err != nil {
				panic(err)
			}
		case <-mp.closed:
			return
		}
	}
}

func (mp *Multiplex) nextChanID() (out uint64) {
	if mp.initiator {
		out = mp.nextID + 1
	} else {
		out = mp.nextID
	}
	mp.nextID += 2
	return
}

func (mp *Multiplex) NewStream() *Stream {
	return mp.NewNamedStream("")
}

func (mp *Multiplex) NewNamedStream(name string) *Stream {
	mp.ch_lock.Lock()
	sid := mp.nextChanID()
	header := (sid << 3) | NewStream

	if name == "" {
		name = fmt.Sprint(sid)
	}
	s := newStream(sid, name, true, mp.outchan)
	mp.channels[sid] = s
	mp.ch_lock.Unlock()

	mp.outchan <- msg{
		header: header,
		data:   []byte(name),
	}
	return s
}

func (mp *Multiplex) Serve(handler func(s *Stream)) error {
	go mp.handleOutgoing()
	mp.buf = bufio.NewReader(mp.con)

	defer mp.shutdown()
	for {
		ch, tag, err := mp.readNextHeader()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}

		b, err := mp.readNext()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}

		mp.ch_lock.Lock()
		msch, ok := mp.channels[ch]
		if !ok {
			var name string
			if tag == NewStream {
				name = string(b)
			}
			msch = newStream(ch, name, false, mp.outchan)
			mp.channels[ch] = msch
			go handler(msch)
			if tag == NewStream {
				mp.ch_lock.Unlock()
				continue
			}
		}
		mp.ch_lock.Unlock()

		if tag == Close {
			msch.Close()
			mp.ch_lock.Lock()
			delete(mp.channels, ch)
			mp.ch_lock.Unlock()
			continue
		}

		msch.receive(b)
	}
}

func (mp *Multiplex) shutdown() {
	mp.ch_lock.Lock()
	defer mp.ch_lock.Unlock()
	for _, s := range mp.channels {
		s.Close()
	}
}

func (mp *Multiplex) readNextHeader() (uint64, uint64, error) {
	h, _, err := DecodeVarint(mp.buf)
	if err != nil {
		return 0, 0, err
	}

	// get channel ID
	ch := h >> 3

	rem := h & 7

	return ch, rem, nil
}

func (mp *Multiplex) readNext() ([]byte, error) {
	// get length
	l, _, err := DecodeVarint(mp.buf)
	if err != nil {
		return nil, err
	}

	buf := make([]byte, l)
	n, err := io.ReadFull(mp.buf, buf)
	if err != nil {
		return nil, err
	}

	if n != int(l) {
		panic("NOT THE SAME")
	}

	return buf, nil
}

func EncodeVarint(x uint64) []byte {
	var buf [10]byte
	var n int
	for n = 0; x > 127; n++ {
		buf[n] = 0x80 | uint8(x&0x7F)
		x >>= 7
	}
	buf[n] = uint8(x)
	n++
	return buf[0:n]
}

func DecodeVarint(r *bufio.Reader) (x uint64, n int, err error) {
	// x, n already 0
	for shift := uint(0); shift < 64; shift += 7 {
		val, err := r.ReadByte()
		if err != nil {
			return 0, 0, err
		}

		b := uint64(val)
		n++
		x |= (b & 0x7F) << shift
		if (b & 0x80) == 0 {
			return x, n, nil
		}
	}

	// The number is too large to represent in a 64-bit value.
	return 0, 0, errors.New("Too large of a number!")
}
