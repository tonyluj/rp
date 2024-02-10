package main

import (
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

func proxyConn(dst, src io.ReadWriteCloser) (err error) {
	go func() {
		_, err = copyBuffer(src, dst)
		if err != nil {
			err = fmt.Errorf("proxy conn error: %w", err)
			return
		}
	}()

	_, err = copyBuffer(dst, src)
	if err != nil {
		err = fmt.Errorf("proxy conn error: %w", err)
		return
	}
	return
}

func copyBuffer(dst io.WriteCloser, src io.ReadCloser) (written int64, err error) {
	buf := proxyBufferPool.Get().([]byte)
	defer proxyBufferPool.Put(buf)

	return io.CopyBuffer(dst, src, buf)
}
