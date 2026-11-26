package campaign

import (
	"fmt"
	"io"
	"sync/atomic"
	"time"
)

const progressInterval = 5 * time.Second

type progressTracker struct {
	out        io.Writer
	phase      string
	total      int
	completed  atomic.Int64
	killed     atomic.Int64
	survived   atomic.Int64
	nonViable  atomic.Int64
	incomplete atomic.Int64
	done       chan struct{}
	finished   chan struct{}
}

func startProgress(out io.Writer, phase string, total int) *progressTracker {
	if out == nil || total <= 0 {
		return nil
	}
	p := &progressTracker{
		out:      out,
		phase:    phase,
		total:    total,
		done:     make(chan struct{}),
		finished: make(chan struct{}),
	}
	go p.loop()
	return p
}

func (p *progressTracker) record(status string) {
	if p == nil {
		return
	}
	switch status {
	case "killed":
		p.killed.Add(1)
	case "non-viable":
		p.nonViable.Add(1)
	case "input-gap", "assertion-gap", "source-survivor", "untested-change", "weakened-test":
		p.survived.Add(1)
	case "":
	default:
		p.incomplete.Add(1)
	}
	p.completed.Add(1)
}

func (p *progressTracker) stop() {
	if p == nil {
		return
	}
	close(p.done)
	<-p.finished
}

func (p *progressTracker) loop() {
	defer close(p.finished)
	ticker := time.NewTicker(progressInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-ticker.C:
			fmt.Fprintf(p.out, "tmt: %s: %d/%d completed (%d killed, %d survived, %d non-viable, %d pending/incomplete)\n",
				p.phase,
				p.completed.Load(),
				p.total,
				p.killed.Load(),
				p.survived.Load(),
				p.nonViable.Load(),
				p.incomplete.Load(),
			)
		}
	}
}
