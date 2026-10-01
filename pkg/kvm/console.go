package kvm

import (
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"sync"
	"time"
)

// Console bundles a control Session, the video stream and a Framebuffer into
// the thing most callers want: "connect to the iDRAC console, give me pixels,
// take my keystrokes". It is what the CLI and the VNC bridge use.
type Console struct {
	Session *Session
	Video   *VideoStream
	FB      *Framebuffer
	Logger  Logger

	cancel context.CancelFunc
	wg     sync.WaitGroup
	errMu  sync.Mutex
	errs   []error
	done   chan struct{}
}

// OpenConsole connects, authenticates, opens the video channel and starts the
// control and video loops. Cancel ctx or call Close to tear everything down.
func OpenConsole(ctx context.Context, cfg Config) (*Console, error) {
	if cfg.Logger == nil {
		cfg.Logger = nopLogger{}
	}
	c := &Console{Logger: cfg.Logger, FB: NewFramebuffer(), done: make(chan struct{})}
	c.Session = NewSession(cfg)
	if err := c.Session.Connect(ctx); err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		if err := c.Session.Run(runCtx); err != nil && runCtx.Err() == nil {
			c.record(fmt.Errorf("control channel: %w", err))
		}
		cancel()
	}()
	if c.Session.NeedsVideoChannel() {
		rw, err := c.Session.OpenVideo(ctx)
		if err != nil {
			c.Close()
			return nil, fmt.Errorf("video channel: %w", err)
		}
		c.Video = NewVideoStream(rw, c.FB, cfg.Logger)
		c.Video.SetTrace(cfg.TraceVideo)
		c.Video.Control = c.Session
		c.Video.OnConnectStatus = func(connected bool) {
			if connected {
				c.Session.VideoConnected()
			}
		}
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			if err := c.Video.Run(runCtx); err != nil && runCtx.Err() == nil && !errors.Is(err, io.EOF) {
				c.record(fmt.Errorf("video channel: %w", err))
			}
			cancel()
		}()
	} else {
		// Video rides the control socket (login flag 0x01): feed those packets
		// through the same decoder.
		pr, pw := io.Pipe()
		c.Session.OnVideoPacket = func(p *VideoPacket) {
			f, err := Marshal(p)
			if err == nil {
				pw.Write(f.Bytes())
			}
		}
		c.Video = NewVideoStream(struct {
			io.Reader
			io.Writer
		}{pr, io.Discard}, c.FB, cfg.Logger)
		c.Video.Control = c.Session
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			c.Video.Run(runCtx)
		}()
	}
	go func() {
		c.wg.Wait()
		close(c.done)
	}()
	return c, nil
}

func (c *Console) record(err error) {
	c.errMu.Lock()
	c.errs = append(c.errs, err)
	c.errMu.Unlock()
	c.Logger.Printf("console: %v", err)
}

// Err returns the first asynchronous error, if any.
func (c *Console) Err() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	if len(c.errs) == 0 {
		return nil
	}
	return c.errs[0]
}

// Done is closed when both channels have stopped.
func (c *Console) Done() <-chan struct{} { return c.done }

// WaitRunning blocks until the session reaches RUNNING (video enabled) or
// fails.
func (c *Console) WaitRunning(ctx context.Context) error {
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		switch c.Session.State() {
		case StateRunning:
			return nil
		case StateConnectionFailed, StateLoginFailed, StateClosed, StateClosing:
			if err := c.Err(); err != nil {
				return err
			}
			return fmt.Errorf("console session ended (%v)", c.Session.State())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.done:
			if err := c.Err(); err != nil {
				return err
			}
			return errors.New("console session ended")
		case <-t.C:
		}
	}
}

// WaitFrames waits until at least n complete frames have been decoded (or a
// quiet period of settle passes after the first frame) and returns a
// snapshot. Use it for screenshots: the first frame after connect is the full
// screen, later ones are deltas.
func (c *Console) WaitFrames(ctx context.Context, n uint64, settle time.Duration) (*image.RGBA, error) {
	if err := c.WaitRunning(ctx); err != nil {
		return nil, err
	}
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	var firstAt time.Time
	var last uint64
	for {
		frames := c.FB.Frames()
		if frames > 0 && firstAt.IsZero() {
			firstAt = time.Now()
		}
		if frames != last {
			last = frames
			firstAt = time.Now()
		}
		if frames >= n || (frames > 0 && settle > 0 && time.Since(firstAt) > settle) {
			return c.FB.Snapshot(), nil
		}
		select {
		case <-ctx.Done():
			if frames > 0 {
				return c.FB.Snapshot(), nil
			}
			return nil, fmt.Errorf("no video frame received: %w", ctx.Err())
		case <-c.done:
			if err := c.Err(); err != nil {
				return nil, err
			}
			return nil, errors.New("console session ended before a frame arrived")
		case <-t.C:
		}
	}
}

// Close stops both channels and closes the sockets.
func (c *Console) Close() error {
	if c.cancel != nil {
		c.cancel()
	}
	err := c.Session.Close()
	c.wg.Wait()
	return err
}
