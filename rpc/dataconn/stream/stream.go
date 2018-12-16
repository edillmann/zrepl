package stream

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/zrepl/zrepl/logger"
	"github.com/zrepl/zrepl/rpc/dataconn/base2bufpool"
	"github.com/zrepl/zrepl/rpc/dataconn/frameconn2"
)

type Logger = logger.Logger

type contextKey int

const (
	contextKeyLogger contextKey = 1 + iota
)

func WithLogger(ctx context.Context, log Logger) context.Context {
	return context.WithValue(ctx, contextKeyLogger, log)
}

func getLog(ctx context.Context) Logger {
	log, ok := ctx.Value(contextKeyLogger).(Logger)
	if !ok {
		log = logger.NewNullLogger()
	}
	return log
}

// The following frameconn.Frame.Type are reserved for Streamer.
const (
	SourceEOF uint32 = ^uint32(0)
	SourceErr uint32 = ^uint32(1)
)

// if sendStream returns an error, that error will be sent as a trailer to the client
// ok will return nil, though.
func WriteStream(ctx context.Context, c *frameconn.Conn, stream io.Reader, stype uint32) error {

	if stype == 0 {
		panic("")
	}

	bufpool := base2bufpool.New(19, 19)
	type read struct {
		buf base2bufpool.Buffer
		err error
	}
	reads := make(chan read, 1)
	go func() {
		for {
			buffer := bufpool.Get(1 << 19)
			bufferBytes := buffer.Bytes()
			n, err := io.ReadFull(stream, bufferBytes)
			buffer.Shrink(uint(n))
			if err == io.ErrUnexpectedEOF {
				err = io.EOF
			}
			reads <- read{buffer, err}
			if err != nil {
				close(reads)
				return
			}
		}
	}()

	for read := range reads {
		buf := read.buf
		if read.err != nil && read.err != io.EOF {
			buf.Free()
			errReader := strings.NewReader(read.err.Error())
			err := WriteStream(ctx, c, errReader, SourceErr)
			if err != nil {
				return err
			}
			return nil
		}
		// next line is the hot path...
		writeErr := c.WriteFrame(buf.Bytes(), stype)
		buf.Free()
		if writeErr != nil {
			return writeErr
		}
		if read.err == io.EOF {
			if err := c.WriteFrame([]byte{}, SourceEOF); err != nil {
				return err
			}
			break
		}
	}

	return nil
}

// ReadStream will close c if an error reading  from c or writing to receiver occurs
func ReadStream(ctx context.Context, c *frameconn.Conn, receiver io.Writer, stype uint32) (err error) {

	type read struct {
		f   frameconn.Frame
		err error
	}
	reads := make(chan read, 1)
	go func() {
		for {
			var r read
			r.f, r.err = c.ReadFrame()
			reads <- r
			if r.err != nil || r.f.Header.Type == SourceEOF || r.f.Header.Type == SourceErr {
				close(reads)
				return
			}
		}
	}()

	var f frameconn.Frame
	for read := range reads {
		f = read.f
		if read.err != nil {
			return read.err
		}
		if f.Header.Type != stype {
			break
		}

		n, err := receiver.Write(f.Buffer.Bytes())
		if err != nil {
			f.Buffer.Free()
			return err // FIXME wrap as writer error
		}
		if n != len(f.Buffer.Bytes()) {
			f.Buffer.Free()
			return io.ErrShortWrite
		}
		f.Buffer.Free()
	}

	if f.Header.Type == SourceEOF {
		return nil
	}

	if f.Header.Type == SourceErr {
		return fmt.Errorf("stream error: %q", string(f.Buffer.Bytes())) // FIXME
	}

	return fmt.Errorf("received unexpected frame type: %v", f.Header.Type)
}