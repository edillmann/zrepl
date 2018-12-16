package base2bufpool

import (
	"fmt"
	"math/bits"
	"sync"
)

type pool struct {
	mtx sync.Mutex
	bufs [][]byte
	shift uint
}

func (p *pool) Put(buf []byte) {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	if len(buf) != 1 << p.shift {
		panic(fmt.Sprintf("implementation error: %v %v", len(buf), 1 << p.shift))
	}
	if len(p.bufs) > 10 {
		return
	}
	p.bufs = append(p.bufs, buf)
}

func (p *pool) Get() []byte {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	if len(p.bufs) > 0 {
		ret :=  p.bufs[len(p.bufs)-1]
		p.bufs = p.bufs[0:len(p.bufs)-1]
		return ret
	}
	return make([]byte, 1<<p.shift)
}

type Pool struct {
	minShift uint
	maxShift uint
	pools    []pool
}

type Buffer struct {
	// always power of 2, from Pool.pools
	shiftBuf []byte
	// presentedLen
	payloadLen uint
	// backref too pool for Free
	pool *Pool
}

func (b Buffer) Bytes() []byte {
	return b.shiftBuf[0:b.payloadLen]
}

func (b *Buffer) Shrink(newPayloadLen uint) {
	if newPayloadLen > b.payloadLen {
		panic(fmt.Sprintf("shrink is actually an expand, invalid: %v %v", newPayloadLen, b.payloadLen))
	}
	b.payloadLen = newPayloadLen
}

func (b Buffer) Free() {
	if b.pool != nil {
		b.pool.put(b)	
	}
}

func New(minShift, maxShift uint) *Pool {
	pools := make([]pool, maxShift-minShift+1)
	for i := uint(0); i < uint(len(pools)); i++ {
		i := i // the closure below must copy i
		pools[i] = pool {
			shift: minShift+i,
			bufs: make([][]byte, 0, 10),
		}
	}
	return &Pool{
		minShift: minShift,
		maxShift: maxShift,
		pools:    pools,
	}
}

func fittingShift(x uint) uint {
	if x == 0 {
		return 0
	}
	blen := uint(bits.Len(x))
	if 1<<(blen-1) == x {
		return blen-1
	}
	return blen
}

func (p *Pool) Get(minSize uint) Buffer {
	shift := fittingShift(minSize)
	if shift == 0 {
		return Buffer{[]byte{}, 0, nil}
	}
	if shift < p.minShift || shift > p.maxShift {
		return Buffer{make([]byte, 1<<shift), minSize, nil}
	}
	return Buffer{p.pools[shift-p.minShift].Get(), minSize, p}
}

func (p *Pool) put(buffer Buffer) {
	if buffer.pool != p {
		panic("putting buffer to pool where it didn't originate from")
	}
	buf := buffer.shiftBuf
	if bits.OnesCount(uint(len(buf))) > 1 {
		panic(fmt.Sprintf("putting buffer that is not power of two len: %v", len(buf)))
	}
	if len(buf) == 0 {
		return
	}
	shift := fittingShift(uint(len(buf)))
	if shift < p.minShift || shift > p.maxShift {
		return // drop it
	}
	p.pools[shift-p.minShift].Put(buf)
}
