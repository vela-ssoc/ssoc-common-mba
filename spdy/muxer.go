package spdy

import (
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
)

type muxer struct {
	wmu     sync.Mutex
	tran    net.Conn
	stmID   atomic.Uint32
	mutex   sync.RWMutex
	streams map[uint32]*stream
	accepts chan *stream
	passwd  []byte
	pwn     int
	prn     int
	ctx     context.Context
	cancel  context.CancelFunc
}

func (mux *muxer) Addr() net.Addr       { return mux.LocalAddr() }
func (mux *muxer) LocalAddr() net.Addr  { return mux.tran.LocalAddr() }
func (mux *muxer) RemoteAddr() net.Addr { return mux.tran.RemoteAddr() }

func (mux *muxer) Dial() (Streamer, error) {
	stm, err := mux.newStream()
	if err != nil {
		return nil, err
	}

	mux.putStream(stm)

	return stm, nil
}

func (mux *muxer) Accept() (net.Conn, error) {
	select {
	case stm, ok := <-mux.accepts:
		if !ok {
			return nil, io.ErrClosedPipe
		}
		return stm, nil
	case <-mux.ctx.Done():
		return nil, io.ErrClosedPipe
	}
}

func (mux *muxer) Close() error {
	mux.cancel()
	return mux.tran.Close()
}

// Range 循环
func (mux *muxer) Range(fn func(Streamer) bool) {
	// copy on read
	mux.mutex.RLock()
	streams := make(map[uint32]Streamer, len(mux.streams))
	for k, v := range mux.streams {
		streams[k] = v
	}
	mux.mutex.RUnlock()

	for _, stm := range streams {
		if !fn(stm) {
			break
		}
	}
}

func (mux *muxer) newStream() (*stream, error) {
	select {
	case <-mux.ctx.Done():
		return nil, io.ErrClosedPipe
	default:
	}

	stmID := mux.stmID.Add(2)
	ctx, cancel := context.WithCancel(mux.ctx)

	stm := &stream{
		id:        stmID,
		mux:       mux,
		readEvtCh: make(chan struct{}, 1),
		ctx:       ctx,
		cancel:    cancel,
	}

	return stm, nil
}

func (mux *muxer) synStream(stmID uint32) *stream {
	ctx, cancel := context.WithCancel(mux.ctx)
	return &stream{
		id:        stmID,
		syn:       true,
		mux:       mux,
		readEvtCh: make(chan struct{}, 1),
		ctx:       ctx,
		cancel:    cancel,
	}
}

func (mux *muxer) putStream(stm *stream) {
	id := stm.ID()
	mux.mutex.Lock()
	mux.streams[id] = stm
	mux.mutex.Unlock()
}

func (mux *muxer) getStream(id uint32) *stream {
	mux.mutex.RLock()
	stm := mux.streams[id]
	mux.mutex.RUnlock()

	return stm
}

func (mux *muxer) delStream(id uint32) {
	mux.mutex.Lock()
	delete(mux.streams, id)
	mux.mutex.Unlock()
}

func (mux *muxer) read() {
	defer func() {
		recover()
		_ = mux.Close()
		close(mux.accepts)
	}()

	var header frameHeader
	for {
		err := mux.readFull(header[:])
		if err != nil {
			_ = mux.Close()
			break
		}

		stmID := header.streamID()
		size := header.size()
		flag := header.flag()

		var stm *stream
		if flag == flagSYN {
			stm = mux.synStream(stmID)
			mux.putStream(stm)
			mux.accepts <- stm
		} else {
			stm = mux.getStream(stmID)
		}
		if stm == nil {
			continue
		}

		if flag == flagFIN {
			_ = stm.closeError(io.EOF, false)
		} else if size > 0 && (flag == flagSYN || flag == flagDAT) {
			dat := make([]byte, size)
			if err = mux.readFull(dat); err == nil {
				_, err = stm.receive(dat)
				stm.notifyReadEvt()
			}
			if err != nil {
				_ = stm.closeError(err, true)
			}
		}
	}
}

func (mux *muxer) write(flag uint8, sid uint32, p []byte) (int, error) {
	fm := frame{flag: flag, sid: sid, data: p}
	dat := fm.pack()

	mux.wmu.Lock()
	defer mux.wmu.Unlock()

	if psz := len(mux.passwd); psz != 0 {
		for i, b := range dat {
			mux.prn = (mux.prn + 1) % psz
			enc := mux.passwd[mux.prn]
			dat[i] = b ^ enc
		}
	}

	return mux.tran.Write(dat)
}

// readFull 读取消息
func (mux *muxer) readFull(data []byte) error {
	if _, err := io.ReadFull(mux.tran, data); err != nil {
		return err
	}
	if psz := len(mux.passwd); psz != 0 {
		for i, b := range data {
			mux.pwn = (mux.pwn + 1) % psz
			enc := mux.passwd[mux.pwn]
			data[i] = b ^ enc
		}
	}
	return nil
}
