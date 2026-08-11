package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/virtual-kubelet/virtual-kubelet/log"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"

	"github.com/fivetime/kubezun/pkg/zun"
)

// handshakeTimeout bounds opening the session. The proxy is on the node
// holding the capsule, so this is a hop across the management network; a node
// that cannot answer in this long is not going to serve a terminal.
const handshakeTimeout = 30 * time.Second

// runInteractive gives the caller a terminal in a capsule's container.
//
// Four streams go in and one websocket comes out. The protocol the runtime
// speaks puts the channel in the first byte of every frame and the data after
// it, so this is multiplexing and nothing more -- no translation, no buffering
// decisions, no protocol of our own.
//
// The one thing worth stating: stderr does not appear. A terminal has a single
// stream by definition, and the runtime refuses to allocate one while also
// being asked to separate the two -- which is why the session is opened with
// stderr off, and why anything written to the container's stderr arrives on
// stdout, exactly as it does in a real terminal.
func (p *Provider) runInteractive(ctx context.Context, podUID string, index int, cmd []string, attach api.AttachIO) error {
	session, err := p.capsules.ExecInteractive(ctx, podUID, index, cmd)
	if err != nil {
		return err
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: handshakeTimeout,
		// The proxy speaks websockify's own subprotocol; the runtime's channel
		// framing rides inside the bytes either way. Offering the runtime's
		// name here would be refused by the proxy, which is not the thing
		// being spoken to.
		Subprotocols: []string{"binary"},
	}
	conn, resp, err := dialer.DialContext(ctx, session.ProxyURL, nil)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("attaching to the session: %w (%s)", err, resp.Status)
		}
		return fmt.Errorf("attaching to the session: %w", err)
	}
	defer conn.Close()

	// One writer. A websocket connection permits exactly one writer at a
	// time, and stdin and resize both write.
	var writeMu sync.Mutex
	write := func(frame []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteMessage(websocket.BinaryMessage, frame)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go p.pumpStdin(ctx, attach, write)
	go p.pumpResize(ctx, attach, write)

	return p.pumpOutput(ctx, conn, attach)
}

// pumpStdin carries what the user types into the session.
func (p *Provider) pumpStdin(ctx context.Context, attach api.AttachIO, write func([]byte) error) {
	in := attach.Stdin()
	if in == nil {
		return
	}
	buf := make([]byte, 4096)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			if werr := write(append([]byte{zun.ChannelStdin}, buf[:n]...)); werr != nil {
				return
			}
		}
		if err != nil {
			// End of input is not an error: it is the user pressing ctrl-D,
			// and the command decides what that means.
			return
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// pumpResize tells the session when the window changed.
//
// Without this a terminal is stuck at whatever size it was born with: a full
// screen editor draws over itself, and line editing wraps in the wrong place.
// It is the reason the fifth channel exists.
func (p *Provider) pumpResize(ctx context.Context, attach api.AttachIO, write func([]byte) error) {
	sizes := attach.Resize()
	if sizes == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case size, ok := <-sizes:
			if !ok {
				return
			}
			frame, err := zun.EncodeResize(size.Width, size.Height)
			if err != nil {
				continue
			}
			if err := write(frame); err != nil {
				return
			}
		}
	}
}

// pumpOutput carries the session's output back, and returns what the command
// exited with.
func (p *Provider) pumpOutput(ctx context.Context, conn *websocket.Conn, attach api.AttachIO) error {
	var exitErr error
	for {
		_, frame, err := conn.ReadMessage()
		if err != nil {
			if isNormalClose(err) || ctx.Err() != nil {
				return exitErr
			}
			// A session cut off mid-command has to be reported: it is
			// indistinguishable, from the caller's side, from a command that
			// finished quietly.
			if exitErr != nil {
				return exitErr
			}
			return fmt.Errorf("the session ended unexpectedly: %w", err)
		}
		if len(frame) == 0 {
			continue
		}

		channel, data := frame[0], frame[1:]
		switch channel {
		case zun.ChannelStdout:
			if out := attach.Stdout(); out != nil && len(data) > 0 {
				if _, err := out.Write(data); err != nil {
					return err
				}
			}
		case zun.ChannelStderr:
			// Only reached when the session was not a terminal; with one, the
			// runtime merges the two.
			if errOut := attach.Stderr(); errOut != nil && len(data) > 0 {
				if _, err := errOut.Write(data); err != nil {
					return err
				}
			}
		case zun.ChannelError:
			if len(data) == 0 {
				continue
			}
			code, err := zun.ExitCodeOf(data)
			if err != nil {
				log.G(ctx).WithError(err).Debug("unreadable exec status")
				continue
			}
			if code != 0 {
				// Held rather than returned: the command has exited but its
				// last output may still be in flight, and returning here
				// would cut it off.
				exitErr = fmt.Errorf("command terminated with exit code %d", code)
			}
		}
	}
}

func isNormalClose(err error) bool {
	if errors.Is(err, io.EOF) {
		return true
	}
	return websocket.IsCloseError(err,
		websocket.CloseNormalClosure, websocket.CloseGoingAway,
		websocket.CloseAbnormalClosure)
}
