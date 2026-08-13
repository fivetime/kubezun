package zun

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
)

// InteractiveSession is where to attach to a command running in a capsule.
type InteractiveSession struct {
	// ProxyURL is a websocket url, already carrying the token that admits
	// exactly one session. It names the node holding the capsule: the runtime
	// serves the stream on that node's loopback, so only a proxy running
	// there can reach it.
	ProxyURL string `json:"proxy_url"`
	ExecID   string `json:"exec_id"`
}

// ExecInteractive asks for a command with a terminal and gets back somewhere
// to attach, rather than output.
//
// The other Exec runs the command to completion and answers with everything at
// once. That cannot be a terminal: there is nothing to type into, and a shell
// with nothing on stdin simply waits until it is cut off.
func (a *CapsuleAPI) ExecInteractive(ctx context.Context, podUID string, index int, cmd []string) (*InteractiveSession, error) {
	if podUID == "" {
		return nil, fmt.Errorf("a pod UID is required to find its capsule")
	}
	if len(cmd) == 0 {
		return nil, fmt.Errorf("a command is required")
	}
	name := "kubezun-" + podUID

	capsule, err := a.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	if index < 0 || index >= len(capsule.Containers) {
		return nil, errdefs.NotFoundf(
			"the capsule has %d containers, so there is none at position %d",
			len(capsule.Containers), index)
	}

	q := url.Values{}
	q.Set("container", capsule.Containers[index].UUID)
	q.Set("command", shellQuote(cmd))
	q.Set("run", "false")
	q.Set("interactive", "true")

	var out InteractiveSession
	if _, err := a.sc.Post(ctx,
		a.url(name, "execute")+"?"+q.Encode(), nil, &out,
		&gophercloud.RequestOpts{OkCodes: []int{200, 202}}); err != nil {
		return nil, translate(err)
	}
	if out.ProxyURL == "" {
		return nil, fmt.Errorf("no session was returned for an interactive exec")
	}
	return &out, nil
}

// The channel each frame belongs to, in the protocol the runtime's streaming
// server speaks. One byte in front of the data; that is the whole framing.
const (
	ChannelStdin  byte = 0
	ChannelStdout byte = 1
	ChannelStderr byte = 2
	ChannelError  byte = 3
	ChannelResize byte = 4
)

// TerminalSize is what travels on the resize channel, as the runtime expects
// to read it.
type TerminalSize struct {
	Width  uint16 `json:"Width"`
	Height uint16 `json:"Height"`
}

// EncodeResize renders a terminal size as a resize frame.
func EncodeResize(width, height uint16) ([]byte, error) {
	body, err := json.Marshal(TerminalSize{Width: width, Height: height})
	if err != nil {
		return nil, err
	}
	return append([]byte{ChannelResize}, body...), nil
}

// ExecStatus is what the error channel carries when the command ends: an
// ordinary Kubernetes Status.
type ExecStatus struct {
	Status  string `json:"status"`
	Message string `json:"message"`
	Reason  string `json:"reason"`
	Details *struct {
		Causes []struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"causes"`
	} `json:"details"`
}

// ExitCodeOf reads the command's exit status out of an error-channel frame.
//
// Success arrives as {"status":"Success"} and everything else names the code
// in a cause. A caller has to see it: kubectl reports what a command exited
// with, and losing it makes a failure look like success.
func ExitCodeOf(frame []byte) (int, error) {
	var st ExecStatus
	if err := json.Unmarshal(frame, &st); err != nil {
		return 0, fmt.Errorf("unreadable exec status %q: %w", string(frame), err)
	}
	if st.Status == "Success" {
		return 0, nil
	}
	if st.Details != nil {
		for _, c := range st.Details.Causes {
			if c.Reason == "ExitCode" {
				var code int
				if _, err := fmt.Sscanf(c.Message, "%d", &code); err == nil {
					return code, nil
				}
			}
		}
	}
	// A failure with no code named: report something non-zero rather than
	// letting it read as success.
	if st.Message != "" {
		return 1, fmt.Errorf("%s", st.Message)
	}
	return 1, nil
}
