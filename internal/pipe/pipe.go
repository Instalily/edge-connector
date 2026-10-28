// Package pipe joins two connections.
package pipe

import (
	"io"
	"sync"
)

type closeWriter interface {
	CloseWrite() error
}

// Join copies bytes both ways between a and b until both directions finish,
// then closes both. When one side finishes sending, the other side's write
// half is closed so protocols that half-close still work. A copy error closes
// both sides at once. Join returns the bytes copied from a to b and from b
// to a.
func Join(a, b io.ReadWriteCloser) (aToB, bToA int64) {
	var wg sync.WaitGroup
	cp := func(dst, src io.ReadWriteCloser, n *int64) {
		defer wg.Done()
		var err error
		*n, err = io.Copy(dst, src)
		if err != nil {
			a.Close()
			b.Close()
			return
		}
		halfClose(dst)
	}
	wg.Add(2)
	go cp(b, a, &aToB)
	go cp(a, b, &bToA)
	wg.Wait()
	a.Close()
	b.Close()
	return aToB, bToA
}

// halfClose ends c's write half. A yamux stream's Close already does only
// that: it sends FIN and leaves the stream readable.
func halfClose(c io.ReadWriteCloser) {
	if cw, ok := c.(closeWriter); ok {
		cw.CloseWrite()
		return
	}
	c.Close()
}
