package main

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// TestRawCaptureConnectionStopsBuffering verifies that a rawCaptureConnection
// captures bytes into its buffer while active, that the bytes extracted before
// stopping remain valid, and that after stopBuffering further reads are
// forwarded without being captured (and without panicking on a nil buffer).
func TestRawCaptureConnectionStopsBuffering(t *testing.T) {
	t.Parallel()
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	rcc := &rawCaptureConnection{
		Conn:    serverConn,
		buffer:  new(bytes.Buffer),
		capture: true,
	}

	readAll := func(want string) {
		t.Helper()
		buf := make([]byte, 64)
		n, err := rcc.Read(buf)
		if err != nil {
			t.Fatalf("Read failed: %v", err)
		}
		if string(buf[:n]) != want {
			t.Fatalf("Read got %q, want %q", string(buf[:n]), want)
		}
	}

	// While capturing, bytes are stored in the buffer.
	go func() { _, _ = clientConn.Write([]byte("request")) }()
	readAll("request")
	if rcc.buffer.String() != "request" {
		t.Fatalf("buffer = %q, want %q", rcc.buffer.String(), "request")
	}

	// Bytes extracted before stopping stay valid after the buffer is dropped.
	raw := rcc.buffer.Bytes()
	rcc.stopBuffering()
	if string(raw) != "request" {
		t.Fatalf("raw = %q, want %q", string(raw), "request")
	}
	if rcc.buffer != nil {
		t.Fatalf("buffer should be nil after stopBuffering")
	}

	// After stopping, reads still work but are no longer captured.
	go func() { _, _ = clientConn.Write([]byte("stream")) }()
	readAll("stream")
	if rcc.buffer != nil {
		t.Fatalf("buffer must remain nil after stopBuffering")
	}
}

// TestStartClientReadMonitorPreservesRequestBytes pins that request bytes the
// client sends during the startup window (before forwardConnection begins draining
// the reader) must not be lost. startClientReadMonitor hands back a reader that
// yields the exact bytes the client wrote, in order, even though those bytes were
// written before any read was issued and sat buffered through the startup wait.
// A real loopback TCP connection is used (not net.Pipe) so the kernel buffers the
// peer's Write and it can complete before any Read is issued.
func TestStartClientReadMonitorPreservesRequestBytes(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}
	defer listener.Close()

	peerConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	defer peerConn.Close()
	clientConn, err := listener.Accept()
	if err != nil {
		t.Fatalf("Accept failed: %v", err)
	}
	defer clientConn.Close()

	request := []byte("GET / HTTP/1.1\r\nHost: example\r\n\r\nsome-request-body")

	// Write the full request BEFORE starting the monitor / before any consumer
	// reads, to simulate a client-speaks-first protocol whose bytes arrive while
	// the service is still starting.
	if _, err := peerConn.Write(request); err != nil {
		t.Fatalf("peer Write failed: %v", err)
	}

	reader, closeReader, _ := startClientReadMonitor(clientConn)
	defer closeReader()

	// Simulate the startup window: forwardConnection has not started draining yet.
	time.Sleep(100 * time.Millisecond)

	// Now drain exactly len(request) bytes (as forwardConnection would) and assert
	// they are the exact, in-order request bytes — none lost across the wait.
	received := make([]byte, len(request))
	done := make(chan error, 1)
	go func() { _, err := io.ReadFull(reader, received); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ReadFull failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for buffered request bytes to be delivered")
	}
	if !bytes.Equal(received, request) {
		t.Fatalf("received %q, want exact request %q", string(received), string(request))
	}
}

// TestBufferedPipeBackpressureBlocksWrite verifies that once the buffered backlog
// reaches the cap, Write blocks (re-applying backpressure) until a Read frees
// space — closing the unbounded-buffer memory hole during startup.
func TestBufferedPipeBackpressureBlocksWrite(t *testing.T) {
	t.Parallel()
	const cap = 8
	pipe := newBufferedPipe(cap)

	// Fill the buffer to the cap (allowed into an empty buffer) plus a bit more
	// via subsequent writes that each individually fit but collectively exceed.
	if _, err := pipe.Write([]byte("01234567")); err != nil { // exactly cap, empty buffer => allowed
		t.Fatalf("initial write failed: %v", err)
	}

	writeDone := make(chan int, 1)
	go func() {
		n, err := pipe.Write([]byte("overflow"))
		if err != nil {
			writeDone <- -1
			return
		}
		writeDone <- n
	}()

	// While the buffer is full, the blocked Write must not complete.
	select {
	case n := <-writeDone:
		t.Fatalf("Write completed while buffer was full (n=%d); backpressure is missing", n)
	case <-time.After(50 * time.Millisecond):
	}

	// Drain some bytes; the blocked Write should now complete.
	buf := make([]byte, cap)
	if _, err := io.ReadFull(pipe, buf); err != nil {
		t.Fatalf("ReadFull failed: %v", err)
	}
	select {
	case n := <-writeDone:
		if n != len("overflow") {
			t.Fatalf("blocked Write returned n=%d, want %d", n, len("overflow"))
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("blocked Write did not complete after the buffer was drained")
	}
}

// TestBufferedPipeZeroLimitIsUnbounded verifies that limit == 0 (the operator
// escape hatch) disables backpressure: a write far larger than 0 completes without
// a reader ever draining the pipe.
func TestBufferedPipeZeroLimitIsUnbounded(t *testing.T) {
	t.Parallel()
	pipe := newBufferedPipe(0)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := pipe.Write(make([]byte, 1<<20)); err != nil { // 1 MiB, no reader
			t.Errorf("unbounded Write failed: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("limit=0 Write blocked; expected unbounded/no-backpressure behavior")
	}
}

// TestChannelMutexLockOrCancel verifies the channel-backed mutex semantics that
// startServiceIfNotAlreadyRunningAndConnect relies on: TryLock succeeds on a
// fresh/unlocked mutex and fails while held; LockOrCancel blocks while held,
// acquires the instant the token is returned, and returns false (without
// acquiring) the instant the cancel channel closes.
func TestChannelMutexLockOrCancel(t *testing.T) {
	t.Parallel()
	m := newChannelMutex()

	// Fresh mutex is unlocked; re-lock fails until Unlock.
	if !m.TryLock() {
		t.Fatalf("TryLock on fresh mutex should succeed")
	}
	if m.TryLock() {
		t.Fatalf("TryLock on held mutex should fail")
	}
	m.Unlock()
	if !m.TryLock() {
		t.Fatalf("TryLock after Unlock should succeed")
	}
	m.Unlock()

	// LockOrCancel blocks while held, then acquires on release.
	m.Lock()
	acquired := make(chan bool, 1)
	go func() { acquired <- m.LockOrCancel(make(chan struct{})) }() // never-canceling cancel chan
	select {
	case <-acquired:
		t.Fatalf("LockOrCancel acquired while mutex was held")
	case <-time.After(50 * time.Millisecond):
	}
	m.Unlock()
	select {
	case got := <-acquired:
		if !got {
			t.Fatalf("LockOrCancel returned false after release; want true")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("LockOrCancel did not acquire after the token was returned")
	}
	m.Unlock() // release what the goroutine acquired

	// LockOrCancel returns false when cancel closes before the token is available.
	m.Lock()
	cancel := make(chan struct{})
	gotCancel := make(chan bool, 1)
	go func() { gotCancel <- m.LockOrCancel(cancel) }()
	select {
	case <-gotCancel:
		t.Fatalf("LockOrCancel returned before cancel while mutex was held")
	case <-time.After(50 * time.Millisecond):
	}
	close(cancel)
	select {
	case got := <-gotCancel:
		if got {
			t.Fatalf("LockOrCancel returned true on cancel; want false")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("LockOrCancel did not return after cancel closed")
	}
	m.Unlock()
}

// TestStartClientReadMonitorReturnsEOFAfterCleanClose pins the behavior that
// broke startup-timeout-cleanup / client-close-full: after the client closes
// its side cleanly (a normal EOF), io.Copy in the monitor returns a nil error,
// so the reader MUST still surface io.EOF once its buffered bytes are drained.
func TestStartClientReadMonitorReturnsEOFAfterCleanClose(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}
	defer listener.Close()

	peerConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	defer peerConn.Close()
	clientConn, err := listener.Accept()
	if err != nil {
		t.Fatalf("Accept failed: %v", err)
	}
	defer clientConn.Close()

	request := []byte("ping")
	if _, err := peerConn.Write(request); err != nil {
		t.Fatalf("peer Write failed: %v", err)
	}
	// Clean close from the client side (sends a FIN, no error). io.Copy in the
	// monitor returns nil here, which is exactly the case the EOF handling must
	// not drop.
	if err := peerConn.Close(); err != nil {
		t.Fatalf("peer Close failed: %v", err)
	}

	reader, closeReader, _ := startClientReadMonitor(clientConn)
	defer closeReader()

	// First the buffered bytes must be delivered ...
	received := make([]byte, len(request))
	if _, err := io.ReadFull(reader, received); err != nil {
		t.Fatalf("ReadFull of buffered bytes failed: %v", err)
	}
	if !bytes.Equal(received, request) {
		t.Fatalf("received %q, want %q", string(received), string(request))
	}
	// ... then the reader MUST return io.EOF (not block) so that forwardConnection
	// finishes and the connection's bookkeeping is released.
	readErrChan := make(chan error, 1)
	one := make([]byte, 1)
	go func() { _, err := reader.Read(one); readErrChan <- err }()
	select {
	case err := <-readErrChan:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("expected io.EOF after clean client close, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out: reader did not return io.EOF after a clean client close (forwardConnection would hang and leak the connection)")
	}
}

// A client that sends SOME request bytes and THEN
// disconnects must be observed promptly by the monitor, even though bytes were
// sent and nobody is consuming the read end yet (the situation during service
// startup, before forwardConnection begins). Under the OLD synchronous io.Pipe
// the monitor would stall inside pipeWriter.Write (blocked waiting for a reader
// that does not exist until startup finishes), stop issuing Read on the client
// connection, and thus never observe the close — the disconnected channel would
// not close until the startup timeout / maxWait. With the unbounded buffered
// pipe, Write returns immediately so the monitor keeps reading the client and
// observes the close promptly. (We do not consume the reader, to mirror the
// pre-forwardConnection startup window.)
func TestStartClientReadMonitorDetectsDisconnectAfterBytes(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}
	defer listener.Close()

	peerConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	clientConn, err := listener.Accept()
	if err != nil {
		t.Fatalf("Accept failed: %v", err)
	}

	reader, closeReader, disconnected := startClientReadMonitor(clientConn)
	defer closeReader()
	defer clientConn.Close()
	_ = reader // intentionally not consumed: mirrors the pre-forwardConnection startup window

	// Send some request bytes first — this is exactly the condition that triggers
	// the issue (the old io.Pipe would then stall inside pipeWriter.Write).
	if _, err := peerConn.Write([]byte("GET / HTTP/1.1\r\nHost: example\r\n\r\n")); err != nil {
		t.Fatalf("peer Write failed: %v", err)
	}
	// Give the monitor a moment to read the bytes and settle into its next Read
	// on the client connection.
	time.Sleep(100 * time.Millisecond)

	// The client disconnects after sending bytes. The monitor must observe this
	// promptly because it keeps issuing Read on clientConnection.
	if err := peerConn.Close(); err != nil {
		t.Fatalf("peer Close failed: %v", err)
	}

	select {
	case <-disconnected:
		// disconnect detected promptly — pass
	case <-time.After(2 * time.Second):
		t.Fatalf("client disconnect after sending bytes was not detected within 2s (Issue B regression)")
	}
}
