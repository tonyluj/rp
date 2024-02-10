package main

import (
	"errors"
	"fmt"
	"io"
	"sync"
)

const DefaultProxyBufferSize = 32 * 1024

var proxyBufferPool *sync.Pool

func init() {
	proxyBufferPool = &sync.Pool{New: func() any {
		return make([]byte, DefaultProxyBufferSize)
	}}
}

var errInvalidWrite = errors.New("invalid write result")

type Proxier struct {
	connA, connB       io.ReadWriter
	bytesA2B, bytesB2A int64 // write to dst
}

func NewProxier(connA, connB io.ReadWriter) (p *Proxier) {
	p = &Proxier{
		connA: connA,
		connB: connB,
	}
	return
}

func (p *Proxier) BytesA2B() int64 {
	return p.bytesA2B
}

func (p *Proxier) BytesB2A() int64 {
	return p.bytesB2A
}

func (p *Proxier) Proxy() (err error) {
	go func() {
		err = p.copyBuffer(p.connA, p.connB, &p.bytesB2A)
		if err != nil {
			err = fmt.Errorf("proxy conn error: %w", err)
			return
		}
	}()

	err = p.copyBuffer(p.connB, p.connA, &p.bytesA2B)
	if err != nil {
		err = fmt.Errorf("proxy conn error: %w", err)
		return
	}
	return
}

// copied from io.copyBuffer, add written pointer
func (p *Proxier) copyBuffer(dst io.Writer, src io.Reader, written *int64) (err error) {
	buf := proxyBufferPool.Get().([]byte)
	defer proxyBufferPool.Put(buf)

	for {
		nr, er := src.Read(buf)
		if nr > 0 {
			nw, ew := dst.Write(buf[0:nr])
			if nw < 0 || nr < nw {
				nw = 0
				if ew == nil {
					ew = errInvalidWrite
				}
			}
			if written != nil {
				*written += int64(nw)
			}
			if ew != nil {
				err = ew
				break
			}
			if nr != nw {
				err = io.ErrShortWrite
				break
			}
		}
		if er != nil {
			if er != io.EOF {
				err = er
			}
			break
		}
	}
	return err
}
