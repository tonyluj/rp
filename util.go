package main

import (
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

func proxyConn(dst, src io.ReadWriteCloser) {
	defer func(dst io.ReadWriteCloser) {
		err := dst.Close()
		if err != nil {
			return
		}
	}(dst)

	go func() {
		defer func(src io.ReadWriteCloser) {
			err := src.Close()
			if err != nil {
				return
			}
		}(src)

		_, err := copyBuffer(src, dst)
		if err != nil {
			return
		}
	}()

	_, err := copyBuffer(dst, src)
	if err != nil {
		return
	}
}

func copyBuffer(dst io.WriteCloser, src io.ReadCloser) (written int64, err error) {
	buf := proxyBufferPool.Get().([]byte)
	defer proxyBufferPool.Put(buf)

	return io.CopyBuffer(dst, src, buf)
}
