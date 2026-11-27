package copytree

import (
	"sync"
)

// Many small files are slow to copy one after the other: every file is a
// few round trips to a network disk (inode, data, inode of the copy). The
// walk stays in one goroutine – directories are made before what is in
// them, and their times are set at the end – and everything else is handed
// to Workers goroutines.

// pool runs jobs inline (workers ≤ 1) or in workers. The first error stops
// it: later jobs are dropped, do returns the error.
type pool struct {
	jobs chan func([]byte) error
	wg   sync.WaitGroup
	buf  []byte // inline

	mu  sync.Mutex
	err error
}

func newPool(workers int) *pool {
	p := &pool{buf: make([]byte, 1<<20)}
	if workers <= 1 {
		return p
	}
	p.jobs = make(chan func([]byte) error, workers*4)
	for range workers {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			buf := make([]byte, 1<<20)
			for j := range p.jobs {
				if p.failed() != nil {
					continue
				}
				if err := j(buf); err != nil {
					p.fail(err)
				}
			}
		}()
	}
	return p
}

func (p *pool) fail(err error) {
	p.mu.Lock()
	if p.err == nil {
		p.err = err
	}
	p.mu.Unlock()
}

func (p *pool) failed() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// do runs j now or hands it to a worker.
func (p *pool) do(j func([]byte) error) error {
	if p.jobs == nil {
		return j(p.buf)
	}
	if err := p.failed(); err != nil {
		return err
	}
	p.jobs <- j
	return nil
}

// wait: all jobs are done. Safe to call more than once.
func (p *pool) wait() error {
	if p.jobs != nil {
		close(p.jobs)
		p.wg.Wait()
		p.jobs = nil
	}
	return p.failed()
}

// linkTarget: the first copy of a multiply linked file. Later links wait
// for it (it may still be copied by another worker).
type linkTarget struct {
	path string
	done chan struct{}
	err  error
}
