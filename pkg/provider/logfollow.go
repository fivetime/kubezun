package provider

import (
	"bytes"
	"context"
	"io"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/log"

	"github.com/fivetime/kubezun/pkg/zun"
)

// followInterval is how often a followed log is asked for more.
//
// It is the delay a line waits before a reader sees it. A second is short
// enough to read like a stream and long enough that a "kubectl logs -f" left
// open overnight is not a request per second per viewer against the Zun API.
const followInterval = time.Second

// followLogs turns Zun's read-the-whole-log call into something that reads
// like a stream.
//
// Zun has no streaming endpoint: it answers with the log as it stands and
// closes. Asking again gets everything again, which is why following used to
// be refused rather than faked -- a reader would have seen every line
// repeated at each poll.
//
// What makes it exact is the format the runtime writes: every line carries an
// RFC3339Nano timestamp. Timestamps are always requested from Zun, whatever
// the caller asked for, and the last one emitted is remembered; the next poll
// emits only lines after it, and strips the timestamps again if the caller did
// not want them. Repetition is therefore not "unlikely" but impossible: a line
// is emitted when its timestamp sorts after the last emitted one, and the
// runtime writes them in order.
func (p *Provider) followLogs(ctx context.Context, capsules *zun.CapsuleAPI, capsuleUUID string, index int, opts zun.LogOptions) io.ReadCloser {
	reader, writer := io.Pipe()

	// Timestamps are the cursor. Whether the caller wanted them is a
	// presentation decision applied on the way out.
	wanted := opts.Timestamps
	poll := opts
	poll.Timestamps = true

	go func() {
		defer writer.Close()

		var last string
		for {
			raw, err := capsules.LogsForPod(ctx, capsuleUUID, index, poll)
			if err != nil {
				// A capsule that goes away ends the stream rather than failing
				// it: the reader has had everything there was.
				log.G(ctx).WithError(err).Debug("stopped following logs")
				return
			}

			fresh, newest := linesAfter(raw, last)
			if newest != "" {
				last = newest
			}
			if len(fresh) > 0 {
				if _, err := writer.Write(render(fresh, wanted)); err != nil {
					return // the reader hung up
				}
			}
			// Only the first read honours tail and since; after that the
			// cursor decides, and re-applying them would hide lines.
			poll.Tail, poll.Since = 0, time.Time{}

			select {
			case <-ctx.Done():
				return
			case <-time.After(followInterval):
			}
		}
	}()

	return reader
}

// linesAfter returns the lines that come after the cursor, and the new cursor.
//
// The cursor is the whole timestamp field of the last line emitted, compared
// as a string: RFC3339Nano at a fixed precision sorts the same either way, and
// a string comparison cannot fail to parse a line the runtime wrote in a way
// this did not expect.
func linesAfter(raw []byte, cursor string) (lines [][]byte, newest string) {
	newest = cursor
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		stamp := timestampOf(line)
		if stamp == "" {
			// No timestamp to place it by. Emitting it would risk repeating it
			// forever; the alternative is dropping one malformed line.
			continue
		}
		if cursor != "" && stamp <= cursor {
			continue
		}
		lines = append(lines, line)
		if stamp > newest {
			newest = stamp
		}
	}
	return lines, newest
}

func timestampOf(line []byte) string {
	i := bytes.IndexByte(line, ' ')
	if i <= 0 {
		return ""
	}
	stamp := string(line[:i])
	if _, err := time.Parse(time.RFC3339Nano, stamp); err != nil {
		return ""
	}
	return stamp
}

// render joins lines back together, dropping the timestamps a caller did not
// ask for.
func render(lines [][]byte, keepTimestamps bool) []byte {
	var out bytes.Buffer
	for _, line := range lines {
		if !keepTimestamps {
			if i := bytes.IndexByte(line, ' '); i > 0 {
				line = line[i+1:]
			}
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	return out.Bytes()
}
