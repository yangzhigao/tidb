// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package gp

import (
	"sync"
	"sync/atomic"
	"time"
)

// Pool is a struct to represent goroutine pool.
type Pool struct {
	head        goroutine
	tail        *goroutine
	count       int
	idleTimeout time.Duration
	sync.Mutex
}

// goroutine is actually a background goroutine, with a channel binded for communication.
type goroutine struct {
	ch     chan func()
	pool   *Pool
	next   *goroutine
	status int32
}

const (
	statusIdle  int32 = 0
	statusInUse int32 = 1
	statusDying int32 = 2 // Intermediate state used to avoid race: Idle => Dying => Dead
	statusDead  int32 = 3
)

// New returns a new *Pool object.
func New(idleTimeout time.Duration) *Pool {
	pool := &Pool{
		idleTimeout: idleTimeout,
	}
	pool.tail = &pool.head
	return pool
}

// Go works like go func(), but goroutines are pooled for reusing.
// This strategy can avoid runtime.morestack, because pooled goroutine is already enlarged.
func (pool *Pool) Go(f func()) {
	var g *goroutine
	for {
		g = pool.get()
		if atomic.CompareAndSwapInt32(&g.status, statusIdle, statusInUse) {
			break
		}
		// Status already changed from statusIdle => statusDying, delete this goroutine.
		if atomic.LoadInt32(&g.status) == statusDying {
			g.status = statusDead
		}
	}

	g.ch <- f
	// When the goroutine finish f(), it will be put back to pool automatically,
	// so it doesn't need to call pool.put() here.
}

func (pool *Pool) get() *goroutine {
	pool.Lock()
	head := &pool.head
	if head.next == nil {
		pool.Unlock()
		return pool.alloc()
	}

	ret := head.next
	head.next = ret.next
	if ret == pool.tail {
		pool.tail = head
	}
	pool.count--
	pool.Unlock()
	ret.next = nil
	return ret
}

func (pool *Pool) put(p *goroutine) {
	p.next = nil
	pool.Lock()
	pool.tail.next = p
	pool.tail = p
	pool.count++
	p.status = statusIdle
	pool.Unlock()
}

func (pool *Pool) alloc() *goroutine {
	g := &goroutine{
		ch:   make(chan func()),
		pool: pool,
	}
	go func(g *goroutine) {
		timer := time.NewTimer(pool.idleTimeout)
		for {
			select {
			case <-timer.C:
				// Check to avoid a corner case that the goroutine is take out from pool,
				// and get this signal at the same time.
				succ := atomic.CompareAndSwapInt32(&g.status, statusIdle, statusDying)
				if succ {
					return
				}
			case work := <-g.ch:
				work()
				// Put g back to the pool.
				// This is the normal usage for a resource pool:
				//
				//     obj := pool.get()
				//     use(obj)
				//     pool.put(obj)
				//
				// But when goroutine is used as a resource, we can't pool.put() immediately,
				// because the resource(goroutine) maybe still in use.
				// So, put back resource is done here,  when the goroutine finish its work.
				pool.put(g)
			}
			timer.Reset(pool.idleTimeout)
		}
	}(g)
	return g
}
