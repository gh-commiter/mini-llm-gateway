package main

import (
	"context"
	"time"
)

// The automatic upstream push and pull on `minigw serve`: one goroutine that looks every few seconds for a link the owner set to auto that is
// dirty, quiet for the debounce and past its back-off, or a link with reading on whose pull is due (every pull_every, 15 minutes by default),
// and pushes or pulls it, one link at a time (core.Store.AutoStep: push and pull in turn). It contacts only upstreams the owner added, links
// the owner set to auto and links whose reach the owner accepted, and stops (cancelling a run in flight) when the gateway shuts down. The
// outbox and the pull times are in the database, so a restart loses nothing. The Mac app has the same loop in Swift, which asks the helper through POST /upstream-auto.
func (s *server) startUpstreamAuto() {
	stop, done := make(chan struct{}), make(chan struct{})
	s.autoStop, s.autoDone = stop, done
	st := s.core
	go func() {
		defer close(done)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			select {
			case <-stop:
				cancel()
			case <-ctx.Done():
			}
		}()
		sleep := func(d time.Duration) bool {
			select {
			case <-time.After(d):
				return true
			case <-ctx.Done():
				return false
			}
		}
		for {
			wait := time.Duration(s.autoTick.Load())
			if wait <= 0 {
				wait = 10 * time.Second
			}
			select {
			case <-stop:
				return
			case <-time.After(wait):
			}
			for { // drain: one link per call, the next at once while links are due
				run, err := st.AutoStep(ctx, upstreamHost{s}, sleep)
				if err != nil {
					s.log.Printf("automatic upstream push or pull: %v", err)
					break
				}
				if !run.Ran || ctx.Err() != nil {
					break
				}
			}
		}
	}()
}

func (s *server) stopUpstreamAuto() {
	if s.autoStop == nil {
		return
	}
	close(s.autoStop)
	<-s.autoDone
	s.autoStop = nil
}
