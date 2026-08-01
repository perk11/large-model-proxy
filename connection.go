package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
)

func startProxy(serviceConfig ServiceConfig) {
	listener, err := net.Listen("tcp", ":"+serviceConfig.ListenPort)
	log.Printf("[%s] Listening on port %s", serviceConfig.Name, serviceConfig.ListenPort)
	if err != nil {
		log.Fatalf("[%s] Fatal error: cannot listen on port %s: %v", serviceConfig.Name, serviceConfig.ListenPort, err)
	}
	defer func(listener net.Listener) {
		_ = listener.Close()
	}(listener)

	for {
		if interrupted.Load() {
			return
		}
		clientConnection, err := listener.Accept()
		if err != nil {
			log.Printf("[%s] Error accepting connection: %v", serviceConfig.Name, err)
			continue
		}
		log.Printf("[%s] New client connection received %s", serviceConfig.Name, humanReadableConnection(clientConnection))
		go handleConnection(clientConnection, serviceConfig, []byte{})
	}
}
func humanReadableConnection(conn net.Conn) string {
	if conn == nil {
		return "nil"
	}
	return fmt.Sprintf("%s->%s", conn.LocalAddr().String(), conn.RemoteAddr().String())
}

// bufferedPipe is an in-memory io.Pipe-like reader/writer pair backed by a
// BOUNDED buffer. Once the accumulated backlog reaches the configured cap, Write
// blocks (true backpressure) until a Read frees space, rather than growing the
// buffer without limit. This re-engages TCP flow control on the client during
// service startup (or any window where the consumer is slow to drain), bounding
// the proxy's RAM in the face of a large upload or a hostile peer.
//
// Unlike io.Pipe, the monitor can still keep issuing Read on the client
// connection (and thereby detect a client disconnect/EOF) while a bounded backlog
// is outstanding — that is the whole reason bufferedPipe exists rather than a
// plain io.Pipe, whose Write would stall the moment the read end is unconsumed
// and so block the monitor until startup completes, defeating prompt client-disconnect detection. The
// tradeoff is now BOUNDED rather than unbounded: request bytes are still buffered
// and preserved for the consumer, but only up to the cap. A limit of 0
// restores the old unbounded/no-backpressure behavior as an operator escape hatch.
type bufferedPipe struct {
	mu         sync.Mutex
	buf        bytes.Buffer
	limit      uint64 // max bytes buffered before Write blocks; 0 => unbounded
	writerDone bool   // set by closeWrite; once set, Reads return io.EOF after the buffer drains
	closed     bool   // read end closed by the consumer via closeRead
	cond       *sync.Cond
}

func newBufferedPipe(limit uint64) *bufferedPipe {
	bp := &bufferedPipe{limit: limit}
	bp.cond = sync.NewCond(&bp.mu)
	return bp
}

// Write appends to the internal buffer, applying backpressure once the accumulated
// backlog reaches the cap. A write into an empty buffer is always allowed (even if
// larger than the cap) so a single large request body still flows; the cap bounds
// the buffered backlog, not any individual message. It blocks until there is room.
func (b *bufferedPipe) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Apply backpressure once the accumulated buffered data reaches the cap. A
	// write into an empty buffer is always allowed (even if larger than the cap)
	// so a single large request body still flows — the cap bounds the buffered
	// backlog, not any individual message. limit == 0 means unbounded.
	for b.limit > 0 && b.buf.Len() > 0 && uint64(b.buf.Len())+uint64(len(p)) > b.limit {
		if b.closed {
			return 0, io.ErrClosedPipe
		}
		b.cond.Wait()
	}
	if b.closed {
		return 0, io.ErrClosedPipe
	}
	n, _ := b.buf.Write(p) // bytes.Buffer.Write never returns an error
	b.cond.Broadcast()
	return n, nil
}

// closeWrite marks the writer as done. Subsequent Reads, after draining any
// buffered data, return io.EOF regardless of err (so partially-buffered request
// bytes are still delivered before EOF). This MUST be tracked with a dedicated
// flag rather than err != nil: io.Copy returns a nil error on a clean EOF, so
// gating on err would leave Read blocked forever after a normal client close
// (which would stall forwardConnection and leak the proxied-connection count).
func (b *bufferedPipe) closeWrite(_ error) {
	b.mu.Lock()
	b.writerDone = true
	b.cond.Broadcast()
	b.mu.Unlock()
}

// closeRead closes the read end: in-flight and future Writes return io.ErrClosedPipe
// so the producing goroutine stops.
func (b *bufferedPipe) closeRead() {
	b.mu.Lock()
	b.closed = true
	b.cond.Broadcast()
	b.mu.Unlock()
}

func (b *bufferedPipe) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.buf.Len() == 0 {
		if b.writerDone {
			return 0, io.EOF // writer done and nothing left buffered
		}
		if b.closed {
			return 0, io.ErrClosedPipe
		}
		b.cond.Wait()
	}
	n, _ := b.buf.Read(p)
	b.cond.Broadcast() // wake any Write blocked on backpressure now that room freed up
	return n, nil
}

// defaultClientRequestBufferLimitBytes bounds how many unread client→service
// request bytes the proxy will buffer in RAM during service startup (or whenever
// the service is slow to consume). Beyond this the monitor's Write blocks,
// re-applying TCP backpressure on the client instead of buffering unbounded data.
const defaultClientRequestBufferLimitBytes = 16 << 20 // 16 MiB

// startClientReadMonitor takes ownership of reading from clientConnection for the
// whole lifetime of a proxied connection. All bytes read from the client are
// available through the returned reader, so no request data is lost. The
// returned channel is closed as soon as the client's read side reports an error
// (i.e. the client disconnected), which lets waiting code abort promptly instead
// of holding a stale "waiting" connection count. The returned close func closes
// the reader end of the pipe and must be invoked when the caller is done; it
// unblocks the monitor goroutine if it is stuck writing to a full pipe.
//
// The reader/writer pair is a bufferedPipe rather than an io.Pipe: io.Pipe's Write
// blocks until a Read consumes the data, but the read end is only consumed by
// forwardConnection, which does not start until after service startup. With an
// io.Pipe, a client that sends any request bytes during startup (the common case
// for client-speaks-first protocols) would stall the monitor inside Write, stop
// reading the client connection, and thus fail to detect the client's disconnect
// until maxWait/startup timeout — defeating prompt client-disconnect detection. A bufferedPipe keeps
// reading the client and observes the disconnect promptly, while still preserving
// the buffered request bytes for the consumer. Unlike the old
// unbounded version, its buffer is now capped (default 16 MiB,
// ClientRequestBufferLimitBytes; 0 restores unbounded behavior), so Write only
// blocks once the backlog reaches that cap rather than growing RAM without limit.
func startClientReadMonitor(clientConnection net.Conn) (clientReader io.Reader, closeReader func(), disconnected <-chan struct{}) {
	limit := uint64(defaultClientRequestBufferLimitBytes)
	if config.ClientRequestBufferLimitBytes != nil {
		limit = uint64(*config.ClientRequestBufferLimitBytes) // 0 => unbounded (operator escape hatch)
	}
	pipe := newBufferedPipe(limit)
	disconnectedChan := make(chan struct{})
	go func() {
		_, err := io.Copy(pipe, clientConnection) // pipe.Write blocks only past the cap, so we keep reading the client → detect disconnect
		pipe.closeWrite(err)
		close(disconnectedChan)
	}()
	var once sync.Once
	return pipe, func() {
		once.Do(func() { pipe.closeRead() })
	}, disconnectedChan
}

func handleConnection(clientConnection net.Conn, serviceConfig ServiceConfig, dataToSendToServiceBeforeForwardingFromClient []byte) {
	if interrupted.Load() {
		_ = clientConnection.Close()
		return
	}
	clientReader, closeClientReader, clientDisconnected := startClientReadMonitor(clientConnection)
	defer closeClientReader()

	resourceManager.incrementConnection(serviceConfig.Name, 0, 1)
	serviceConnection := startServiceIfNotAlreadyRunningAndConnect(serviceConfig, clientDisconnected)

	if serviceConnection == nil {
		resourceManager.incrementConnection(serviceConfig.Name, 0, -1)
		closeConnectionAndHandleError(
			clientConnection,
			serviceConfig,
			"client",
			"failed to establish a connection to the service",
		)
		return
	}

	log.Printf("[%s] Opened service connection %s", serviceConfig.Name, humanReadableConnection(serviceConnection))
	trackServiceLastUsed(serviceConfig, true)
	resourceManager.incrementConnection(serviceConfig.Name, 1, -1)
	defer resourceManager.incrementConnection(serviceConfig.Name, -1, 0)

	if len(dataToSendToServiceBeforeForwardingFromClient) > 0 {
		if _, err := serviceConnection.Write(dataToSendToServiceBeforeForwardingFromClient); err != nil {
			log.Printf("[%s] Error writing bytes read from client to service: %v", serviceConfig.Name, err)
			closeConnectionAndHandleError(
				clientConnection,
				serviceConfig,
				"client",
				"internal error",
			)
			closeConnectionAndHandleError(
				serviceConnection,
				serviceConfig,
				"service",
				"internal error",
			)
			return
		}
	}

	//forwardConnection will handle closing the connections at this point
	forwardConnection(clientReader, clientConnection, serviceConnection, serviceConfig.Name)

	trackServiceLastUsed(serviceConfig, false)
}

func closeConnectionAndHandleError(connection net.Conn, serviceConfig ServiceConfig, connectionType string, reason string) {
	log.Printf(
		"[%s] Closing %s connection %s: %s",
		serviceConfig.Name,
		connectionType,
		humanReadableConnection(connection),
		reason,
	)
	err := connection.Close()
	if err != nil {
		log.Printf(
			"[%s] Failed to close %s connection %s: %v",
			serviceConfig.Name,
			connectionType,
			humanReadableConnection(connection),
			err,
		)
	}
}

// channelMutex is a mutex backed by a buffered channel of capacity 1. Unlike
// sync.Mutex it supports cancellation via a select (e.g. a client disconnect)
// without spawning a goroutine or polling: LockOrCancel blocks on the token
// channel and is woken the instant the token is returned, or bails out the
// instant the cancel channel closes.
type channelMutex struct {
	token chan struct{}
}

func newChannelMutex() *channelMutex {
	m := &channelMutex{token: make(chan struct{}, 1)}
	m.token <- struct{}{} // start unlocked
	return m
}

func (m *channelMutex) TryLock() bool {
	select {
	case <-m.token:
		return true
	default:
		return false
	}
}

func (m *channelMutex) Lock() {
	<-m.token
}

// LockOrCancel blocks until the lock is acquired or cancel is closed.
// Returns true if acquired, false if cancelled (lock not held).
func (m *channelMutex) LockOrCancel(cancel <-chan struct{}) bool {
	select {
	case <-m.token:
		return true
	case <-cancel:
		return false
	}
}

func (m *channelMutex) Unlock() {
	m.token <- struct{}{}
}
func forwardConnection(clientReader io.Reader, clientConnection, serviceConnection net.Conn, serviceName string) {
	var wg sync.WaitGroup
	wg.Add(2)
	var EOFOnWriteFromServerToClient *bool

	go func() {
		defer wg.Done()
		copyAndHandleErrors(
			serviceConnection,
			clientReader,
			fmt.Sprintf("[%s] (service (%s) to client (%s))", serviceName, humanReadableConnection(serviceConnection), humanReadableConnection(clientConnection)),
		)

		if EOFOnWriteFromServerToClient == nil {
			EOFOnWriteFromServerToClient = new(bool)
			*EOFOnWriteFromServerToClient = true
		}
		// Once done copying client->service, close service side.
		err := serviceConnection.Close()
		if err != nil && !errors.Is(err, net.ErrClosed) {
			log.Printf("[%s] Error closing service to client connection: %v", serviceName, err)
		}
	}()
	go func() {
		defer wg.Done()
		copyAndHandleErrors(
			clientConnection,
			serviceConnection,
			fmt.Sprintf("[%s] (client (%s) to service (%s))", serviceName, humanReadableConnection(clientConnection), humanReadableConnection(serviceConnection)),
		)
		if EOFOnWriteFromServerToClient == nil {
			EOFOnWriteFromServerToClient = new(bool)
			*EOFOnWriteFromServerToClient = false
		}
		err := clientConnection.Close()
		if err != nil && !errors.Is(err, net.ErrClosed) {
			log.Printf("[%s] Error closing client to service connection: %v", serviceName, err)
		}
	}()
	wg.Wait()
	var reason string
	if *EOFOnWriteFromServerToClient {
		reason = "EOF on write from server to client"
	} else {
		reason = "EOF on write from client to server"
	}
	log.Printf(
		"[%s] Closed service and client connection %s, %s: %s",
		serviceName,
		humanReadableConnection(serviceConnection),
		humanReadableConnection(clientConnection),
		reason,
	)
}
func copyAndHandleErrors(dst io.Writer, src io.Reader, logPrefix string) {
	_, err := io.Copy(dst, src)
	//ErrClosed is not logged since it happens routinely when connection is closed without sending/receiving EOF
	if err != nil && !errors.Is(err, net.ErrClosed) {
		log.Printf("%s error during data transfer: %v", logPrefix, err)
	}
}
