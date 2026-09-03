package main

import (
	"net"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestListenPortStore(t *testing.T) *listenPortStore {
	t.Helper()
	store, err := openListenPortStore(t.TempDir() + "/listen-ports.db")
	if err != nil {
		t.Fatalf("failed to open listen port store: %v", err)
	}
	t.Cleanup(func() { _ = store.close() })
	return store
}

// findFreePort reserves a port from the OS and releases it again.
func findFreePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("failed to reserve a port: %v", err)
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().(*net.TCPAddr).Port
}

// --- sqlite persistence ---

func TestListenPortStoreRoundTripsPort(t *testing.T) {
	t.Parallel()
	store := newTestListenPortStore(t)
	port := findFreePort(t)

	require.NoError(t, store.saveUsedPort("service-a", port))

	lastUsed, found, err := store.getLastUsedPort("service-a")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, port, lastUsed)
}

func TestListenPortStoreSurvivesReopen(t *testing.T) {
	t.Parallel()
	databasePath := t.TempDir() + "/listen-ports.db"
	store, err := openListenPortStore(databasePath)
	require.NoError(t, err)
	port := findFreePort(t)
	require.NoError(t, store.saveUsedPort("service-a", port))
	require.NoError(t, store.close())

	reopened, err := openListenPortStore(databasePath)
	require.NoError(t, err)
	defer func() { _ = reopened.close() }()

	lastUsed, found, err := reopened.getLastUsedPort("service-a")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, port, lastUsed)
}

func TestListenPortStoreUnknownServiceHasNoPort(t *testing.T) {
	t.Parallel()
	store := newTestListenPortStore(t)

	lastUsed, found, err := store.getLastUsedPort("never-seen")
	require.NoError(t, err)
	assert.False(t, found)
	assert.Equal(t, 0, lastUsed)
}

func TestListenPortStoreOverwritesPreviousPort(t *testing.T) {
	t.Parallel()
	store := newTestListenPortStore(t)

	require.NoError(t, store.saveUsedPort("service-a", 45678))
	require.NoError(t, store.saveUsedPort("service-a", 45679))

	lastUsed, found, err := store.getLastUsedPort("service-a")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, 45679, lastUsed, "the most recently saved port must win")
}

func TestListenPortStoresServicesIndependently(t *testing.T) {
	t.Parallel()
	store := newTestListenPortStore(t)

	require.NoError(t, store.saveUsedPort("service-a", 45678))
	require.NoError(t, store.saveUsedPort("service-b", 45679))

	portA, foundA, err := store.getLastUsedPort("service-a")
	require.NoError(t, err)
	portB, foundB, err := store.getLastUsedPort("service-b")
	require.NoError(t, err)
	assert.True(t, foundA)
	assert.True(t, foundB)
	assert.Equal(t, 45678, portA)
	assert.Equal(t, 45679, portB)
}

func TestListenPortStoreCreatesDatabaseFile(t *testing.T) {
	t.Parallel()
	databasePath := t.TempDir() + "/listen-ports.db"
	store, err := openListenPortStore(databasePath)
	require.NoError(t, err)
	require.NoError(t, store.saveUsedPort("service-a", 45678))
	require.NoError(t, store.close())

	assert.FileExists(t, databasePath)
}

// --- allocation ---

func TestAllocateListenPortPrefersLastUsedPort(t *testing.T) {
	t.Parallel()
	// A freed port can be grabbed by unrelated concurrent listeners between
	// releasing it and the allocator binding it, so give the scenario a few
	// attempts: at least one must observe the reuse.
	for attempt := 0; attempt < 20; attempt++ {
		lastUsedPort := findFreePort(t)

		listener, allocatedPort, err := allocateListenPort(lastUsedPort, true, func(int) bool { return false })
		require.NoError(t, err)
		portWasReused := allocatedPort == lastUsedPort
		require.NoError(t, listener.Close())
		if portWasReused {
			return
		}
	}
	t.Fatal("allocator never reused a free last-used port")
}

func TestAllocateListenPortAvoidsOccupiedLastUsedPort(t *testing.T) {
	t.Parallel()
	// Keep the last-used port occupied so it cannot be reused
	blocker, err := net.Listen("tcp", ":0")
	require.NoError(t, err)
	defer func() { _ = blocker.Close() }()
	occupiedPort := blocker.Addr().(*net.TCPAddr).Port

	listener, allocatedPort, err := allocateListenPort(occupiedPort, true, func(int) bool { return false })
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()

	assert.NotEqual(t, occupiedPort, allocatedPort)
	assert.Greater(t, allocatedPort, 0)
}

func TestAllocateListenPortAvoidsTakenCandidates(t *testing.T) {
	t.Parallel()
	var candidates []int
	var firstCandidate int
	// Reject the first candidate the OS offers: the allocator must close it
	// and ask again instead of returning a port marked as taken.
	isPortTaken := func(port int) bool {
		candidates = append(candidates, port)
		if len(candidates) == 1 {
			firstCandidate = port
			return true
		}
		return port == firstCandidate
	}

	listener, allocatedPort, err := allocateListenPort(0, false, isPortTaken)
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()

	assert.NotEmpty(t, candidates, "the taken-port check must be consulted")
	assert.NotEqual(t, firstCandidate, allocatedPort, "a port reported as taken must never be returned")
}

func TestAllocateListenPortReturnsHeldListener(t *testing.T) {
	t.Parallel()
	listener, allocatedPort, err := allocateListenPort(0, false, func(int) bool { return false })
	require.NoError(t, err)
	defer func() { _ = listener.Close() }()

	// The returned listener must already hold the port, so a second bind must fail
	second, secondErr := net.Listen("tcp", ":"+strconv.Itoa(allocatedPort))
	if secondErr == nil {
		_ = second.Close()
		t.Fatalf("expected port %d to be held by the returned listener", allocatedPort)
	}
}

// --- config resolution ---

func TestResolveAutoListenPortsAssignsNumericPort(t *testing.T) {
	t.Parallel()
	store := newTestListenPortStore(t)
	cfg := &Config{
		Services: []ServiceConfig{
			{Name: "auto-service", ListenPort: autoListenPort, Command: "/bin/echo"},
			{Name: "static-service", ListenPort: "8099", Command: "/bin/echo"},
		},
	}

	listeners, err := resolveAutoListenPorts(cfg, store)
	require.NoError(t, err)
	defer closeListeners(listeners)

	assert.NotEqual(t, autoListenPort, cfg.Services[0].ListenPort)
	portNumber, convertErr := strconv.Atoi(cfg.Services[0].ListenPort)
	require.NoError(t, convertErr, "the resolved listen port must be numeric")
	assert.Greater(t, portNumber, 0)
	assert.Equal(t, "8099", cfg.Services[1].ListenPort, "static ports must not be touched")

	listener, found := listeners["auto-service"]
	require.True(t, found, "a bound listener must be returned for the auto service")
	assert.Equal(t, portNumber, listener.Addr().(*net.TCPAddr).Port)

	savedPort, found, err := store.getLastUsedPort("auto-service")
	require.NoError(t, err)
	assert.True(t, found, "the allocated port must be persisted")
	assert.Equal(t, portNumber, savedPort)
}

func TestResolveAutoListenPortsAvoidsConfiguredPorts(t *testing.T) {
	t.Parallel()
	store := newTestListenPortStore(t)
	cfg := &Config{
		OpenAiApi:     OpenAiApi{ListenPort: "8100"},
		ManagementApi: ManagementApi{ListenPort: "8101"},
		Services: []ServiceConfig{
			{Name: "auto-service", ListenPort: autoListenPort, Command: "/bin/echo"},
			{Name: "static-service", ListenPort: "8099", Command: "/bin/echo"},
		},
	}

	listeners, err := resolveAutoListenPorts(cfg, store)
	require.NoError(t, err)
	defer closeListeners(listeners)

	resolvedPort := cfg.Services[0].ListenPort
	assert.NotEqual(t, "8099", resolvedPort)
	assert.NotEqual(t, "8100", resolvedPort)
	assert.NotEqual(t, "8101", resolvedPort)
}

func TestResolveAutoListenPortsReusesPersistedPort(t *testing.T) {
	t.Parallel()
	// Retry to tolerate unrelated listeners grabbing the seeded port between
	// releasing it and the proxy binding it.
	for attempt := 0; attempt < 20; attempt++ {
		store := newTestListenPortStore(t)
		persistedPort := findFreePort(t)
		require.NoError(t, store.saveUsedPort("auto-service", persistedPort))
		cfg := &Config{
			Services: []ServiceConfig{
				{Name: "auto-service", ListenPort: autoListenPort, Command: "/bin/echo"},
			},
		}

		listeners, err := resolveAutoListenPorts(cfg, store)
		require.NoError(t, err)
		resolvedPort := cfg.Services[0].ListenPort
		resolvedListenerPort := listeners["auto-service"].Addr().(*net.TCPAddr).Port
		closeListeners(listeners)

		if resolvedPort == strconv.Itoa(persistedPort) && resolvedListenerPort == persistedPort {
			return
		}
	}
	t.Fatal("resolver never reused a free persisted port")
}

func TestResolveAutoListenPortsReplacesUnavailablePersistedPort(t *testing.T) {
	t.Parallel()
	store := newTestListenPortStore(t)
	blocker, err := net.Listen("tcp", ":0")
	require.NoError(t, err)
	defer func() { _ = blocker.Close() }()
	unavailablePort := blocker.Addr().(*net.TCPAddr).Port
	require.NoError(t, store.saveUsedPort("auto-service", unavailablePort))
	cfg := &Config{
		Services: []ServiceConfig{
			{Name: "auto-service", ListenPort: autoListenPort, Command: "/bin/echo"},
		},
	}

	listeners, err := resolveAutoListenPorts(cfg, store)
	require.NoError(t, err)
	defer closeListeners(listeners)

	resolvedPortNumber, convertErr := strconv.Atoi(cfg.Services[0].ListenPort)
	require.NoError(t, convertErr)
	assert.NotEqual(t, unavailablePort, resolvedPortNumber)

	savedPort, found, err := store.getLastUsedPort("auto-service")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, resolvedPortNumber, savedPort, "the store must record the replacement port")
}

func TestResolveAutoListenPortsAssignsDistinctPorts(t *testing.T) {
	t.Parallel()
	store := newTestListenPortStore(t)
	cfg := &Config{
		Services: []ServiceConfig{
			{Name: "auto-one", ListenPort: autoListenPort, Command: "/bin/echo"},
			{Name: "auto-two", ListenPort: autoListenPort, Command: "/bin/echo"},
		},
	}

	listeners, err := resolveAutoListenPorts(cfg, store)
	require.NoError(t, err)
	defer closeListeners(listeners)

	assert.NotEqual(t, cfg.Services[0].ListenPort, cfg.Services[1].ListenPort, "two auto services must never share a port")
	assert.Len(t, listeners, 2)
}

func TestResolveAutoListenPortsWithoutAutoPorts(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		Services: []ServiceConfig{
			{Name: "static-service", ListenPort: "8099", Command: "/bin/echo"},
		},
	}

	listeners, err := resolveAutoListenPorts(cfg, nil)
	require.NoError(t, err)
	assert.Empty(t, listeners)
	assert.Equal(t, "8099", cfg.Services[0].ListenPort)
}

func TestResolveAutoListenPortsWithoutStoreFails(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		Services: []ServiceConfig{
			{Name: "auto-service", ListenPort: autoListenPort, Command: "/bin/echo"},
		},
	}

	_, err := resolveAutoListenPorts(cfg, nil)
	assert.Error(t, err, "auto ports without a persistence store must be a configuration error")
}
