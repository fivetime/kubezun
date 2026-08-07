package zun

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
)

// LogOptions selects which part of a container's output to read.
type LogOptions struct {
	// Container names which container of the capsule to read. A capsule with
	// more than one requires it.
	Container string
	// Tail is the number of lines from the end; zero means all of them.
	Tail int
	// Since drops lines older than this. The zero time means no limit.
	Since time.Time
	// Timestamps prefixes every line with when it was written.
	Timestamps bool
}

// Logs reads a container's output.
//
// Zun answers with the whole log as a JSON string rather than a stream, so
// this returns bytes. Following a log (kubectl logs -f) therefore has nothing
// to attach to and is refused by the caller rather than faked by polling,
// which would duplicate lines at every boundary.
func (a *CapsuleAPI) Logs(ctx context.Context, id string, opts LogOptions) ([]byte, error) {
	q := url.Values{}
	if opts.Container != "" {
		q.Set("container", opts.Container)
	}
	if opts.Tail > 0 {
		q.Set("tail", strconv.Itoa(opts.Tail))
	}
	if !opts.Since.IsZero() {
		q.Set("since", strconv.FormatInt(opts.Since.Unix(), 10))
	}
	if opts.Timestamps {
		q.Set("timestamps", "True")
	}

	u := a.url(id, "logs")
	if len(q) > 0 {
		u += "?" + q.Encode()
	}

	// Zun sends a bare JSON string, which gophercloud's JSON decoding handles
	// but its struct-shaped helpers do not.
	var body string
	if _, err := a.client.ServiceClient().Get(ctx, u, &body,
		&gophercloud.RequestOpts{OkCodes: []int{200}}); err != nil {
		return nil, translate(err)
	}
	return []byte(body), nil
}

// LogsForPod reads the output of the index'th container of the capsule backing
// a pod.
//
// The container is named by position rather than by the pod's name for it: Zun
// invents its own container names ("capsule-<uuid>-phi-12") and discards the
// one the template gave, so the pod's name matches nothing on that side. Order
// is preserved from the template, which is the same invariant the status
// mapping already relies on.
func (a *CapsuleAPI) LogsForPod(ctx context.Context, podUID string, index int, opts LogOptions) ([]byte, error) {
	if podUID == "" {
		return nil, fmt.Errorf("a pod UID is required to find its capsule")
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
	opts.Container = capsule.Containers[index].UUID
	return a.Logs(ctx, name, opts)
}

// ExecResult is what a command left behind.
type ExecResult struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exit_code"`
}

// Exec runs a command in one container of a capsule and waits for it.
//
// Zun runs it to completion and answers with everything at once, so there is
// no stdin to write to and no output before the command ends. An interactive
// session needs the runtime's streaming endpoint, which Zun does not expose.
func (a *CapsuleAPI) Exec(ctx context.Context, podUID string, index int, cmd []string) (*ExecResult, error) {
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
	// Sent as one string: Zun splits it the way a shell would. Quoting each
	// argument keeps a value with spaces in it a single argument.
	q.Set("command", shellQuote(cmd))
	q.Set("run", "True")

	var out ExecResult
	if _, err := a.client.ServiceClient().Post(ctx,
		a.url(name, "execute")+"?"+q.Encode(), nil, &out,
		&gophercloud.RequestOpts{OkCodes: []int{200}}); err != nil {
		return nil, translate(err)
	}
	return &out, nil
}

// shellQuote joins a command into the single string Zun's endpoint takes,
// quoting each argument so one containing a space stays one argument.
func shellQuote(cmd []string) string {
	quoted := make([]string, 0, len(cmd))
	for _, arg := range cmd {
		quoted = append(quoted, "'"+strings.ReplaceAll(arg, "'", `'"'"'`)+"'")
	}
	return strings.Join(quoted, " ")
}
