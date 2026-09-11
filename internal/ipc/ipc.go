package ipc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Transport limits.
const (
	// MaxFrameBytes bounds a single frame so that neither a malformed nor a
	// hostile peer can exhaust the other side's memory.
	MaxFrameBytes = 8 << 20
	// DefaultTimeout bounds one request/response exchange.
	DefaultTimeout = 60 * time.Second
	// DialTimeout bounds connecting to the socket.
	DialTimeout = 2 * time.Second
	// readBuffer is the socket read buffer. Larger frames keep accumulating in a
	// loop rather than growing the buffer.
	readBuffer = 64 << 10
)

// Transport failures. Callers map these onto the structured error codes in
// pkg/relay rather than surfacing a raw syscall error (spec §26).
var (
	ErrUnavailable = errors.New("the Relay daemon is not running")
	ErrTimeout     = errors.New("the Relay daemon did not respond in time")
	ErrLocked      = errors.New("another Relay daemon already owns this Relay home")
)

// Handler processes one decoded frame and returns the value to write back.
//
// The handler receives the frame kind and the raw JSON so it can unmarshal only
// the fields it needs. Returning a nil value writes no reply. A handler reports
// application failures as structured values in the reply, and reserves the
// error return for a genuine transport problem.
type Handler interface {
	Handle(ctx context.Context, kind string, frame []byte) (any, error)
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(ctx context.Context, kind string, frame []byte) (any, error)

// Handle implements Handler.
func (f HandlerFunc) Handle(ctx context.Context, kind string, frame []byte) (any, error) {
	return f(ctx, kind, frame)
}

// Server accepts client connections on the daemon's Unix socket.
type Server struct {
	Listener net.Listener
	Handler  Handler

	// ErrorLog receives non-fatal per-connection errors. One malformed client
	// must not take the daemon down.
	ErrorLog func(error)
}

// Listen binds the daemon socket.
//
// It creates the parent directory, reclaims a socket left behind by a crashed
// daemon, refuses to start when another daemon already answers on the path, and
// restricts the socket to the current user. The Unix socket is the whole trust
// boundary here: no TCP port is opened (spec §14).
func Listen(path string) (*Server, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create socket directory: %w", err)
	}

	// A socket that answers means a live daemon; anything else on the path is
	// debris from a daemon that exited without cleaning up.
	if connection, err := net.DialTimeout("unix", path, 250*time.Millisecond); err == nil {
		connection.Close()
		return nil, fmt.Errorf("a Relay daemon is already listening on %s", path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale socket %s: %w", path, err)
	}

	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		listener.Close()
		return nil, fmt.Errorf("restrict socket %s: %w", path, err)
	}
	return &Server{Listener: listener}, nil
}

// Serve accepts connections until ctx is cancelled or the listener closes.
func (s *Server) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		s.Listener.Close()
	}()

	var clients sync.WaitGroup
	for {
		connection, err := s.Listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				break
			}
			if s.ErrorLog != nil {
				s.ErrorLog(fmt.Errorf("accept: %w", err))
			}
			continue
		}
		clients.Add(1)
		go func() {
			defer clients.Done()
			s.serveConnection(ctx, connection)
		}()
	}
	clients.Wait()
	return nil
}

// Close stops accepting connections.
func (s *Server) Close() error { return s.Listener.Close() }

// serveConnection reads frames until the peer disconnects. A single connection
// may carry several exchanges, which is what lets a tool say hello and then
// invoke without reconnecting.
func (s *Server) serveConnection(ctx context.Context, connection net.Conn) {
	defer connection.Close()
	reader := bufio.NewReaderSize(connection, readBuffer)

	for {
		frame, err := readFrame(reader)
		if err != nil {
			return
		}
		reply, err := s.Handle(ctx, frame)
		if err != nil {
			if s.ErrorLog != nil {
				s.ErrorLog(err)
			}
			return
		}
		if reply == nil {
			continue
		}
		if err := writeFrame(connection, reply); err != nil {
			return
		}
	}
}

// Handle dispatches one raw frame, decoding only its kind first.
func (s *Server) Handle(ctx context.Context, frame []byte) (any, error) {
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(frame, &envelope); err != nil {
		return Malformed(err.Error()), nil
	}
	if envelope.Type == "" {
		return Malformed("frame has no type"), nil
	}
	return s.Handler.Handle(ctx, envelope.Type, frame)
}

// Client issues sequential requests over a single connection.
//
// Relay's callers are short-lived processes making a handful of calls, so
// pipelining would add correlation bookkeeping without buying anything
// (spec §14).
type Client struct {
	conn    net.Conn
	reader  *bufio.Reader
	Timeout time.Duration
}

// DialClient connects to the daemon socket.
func DialClient(path string) (*Client, error) {
	connection, err := net.DialTimeout("unix", path, DialTimeout)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return &Client{
		conn:    connection,
		reader:  bufio.NewReaderSize(connection, readBuffer),
		Timeout: DefaultTimeout,
	}, nil
}

// Call sends one request frame and decodes the reply into response.
func (c *Client) Call(ctx context.Context, request, response any) error {
	deadline := time.Now().Add(c.Timeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := c.conn.SetDeadline(deadline); err != nil {
		return err
	}

	payload, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode %T: %w", request, err)
	}
	if len(payload)+1 > MaxFrameBytes {
		return fmt.Errorf("request exceeds %d bytes", MaxFrameBytes)
	}
	if _, err := c.conn.Write(append(payload, '\n')); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}

	frame, err := readFrame(c.reader)
	if err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return ErrTimeout
		}
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if response == nil {
		return nil
	}
	if err := json.Unmarshal(frame, response); err != nil {
		return fmt.Errorf("decode reply: %w", err)
	}
	return nil
}

// Close releases the connection.
func (c *Client) Close() error { return c.conn.Close() }

// Call connects, performs one exchange, and disconnects.
func Call(ctx context.Context, path string, request, response any) error {
	client, err := DialClient(path)
	if err != nil {
		return err
	}
	defer client.Close()
	return client.Call(ctx, request, response)
}

// CallWithTimeout is Call with an explicit exchange bound.
//
// Client.Call only ever shortens its deadline against the context, so a caller
// that must legitimately wait longer than DefaultTimeout needs to say so here.
// That caller is the OAuth2 device login: it waits on a human, and the bound is
// the code's own expiry rather than an arbitrary transport timeout (spec §54).
// A non-positive timeout leaves DefaultTimeout in place.
func CallWithTimeout(ctx context.Context, path string, timeout time.Duration, request, response any) error {
	client, err := DialClient(path)
	if err != nil {
		return err
	}
	defer client.Close()
	if timeout > 0 {
		client.Timeout = timeout
	}
	return client.Call(ctx, request, response)
}

// Lock is a held single-instance lock.
type Lock struct {
	file *os.File
}

// Acquire takes the daemon's exclusive lock, returning ErrLocked when another
// daemon already holds it (spec §3.3).
func Acquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}
	if err := flock(file); err != nil {
		file.Close()
		return nil, err
	}
	return &Lock{file: file}, nil
}

// Release drops the lock.
func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := funlock(l.file)
	if closeErr := l.file.Close(); err == nil {
		err = closeErr
	}
	return err
}

// readFrame reads one newline-delimited JSON frame.
func readFrame(reader *bufio.Reader) ([]byte, error) {
	var buffer bytes.Buffer
	for {
		chunk, err := reader.ReadSlice('\n')
		buffer.Write(chunk)
		if buffer.Len() > MaxFrameBytes {
			return nil, fmt.Errorf("frame exceeds %d bytes", MaxFrameBytes)
		}
		switch {
		case err == nil:
			frame := bytes.TrimSpace(buffer.Bytes())
			if len(frame) == 0 {
				return nil, errors.New("empty frame")
			}
			return frame, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return nil, err
		}
	}
}

// writeFrame writes one newline-delimited JSON frame.
func writeFrame(writer io.Writer, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode reply: %w", err)
	}
	if _, err := writer.Write(append(encoded, '\n')); err != nil {
		return err
	}
	return nil
}

// Malformed builds the structured reply for a frame that could not be decoded.
// It lives here, rather than in the daemon, so a malformed frame still gets a
// well-formed answer.
func Malformed(message string) map[string]any {
	return map[string]any{
		"success": false,
		"error": map[string]any{
			"code":      "INVALID_INPUT",
			"message":   "malformed IPC frame: " + message,
			"retryable": false,
		},
	}
}
