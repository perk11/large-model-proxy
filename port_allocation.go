package main

import (
	"database/sql"
	"fmt"
	"log"
	"net"
	"strconv"

	_ "modernc.org/sqlite"
)

// autoListenPort is the magic ListenPort value that asks the proxy to pick a
// free port automatically. Every service using it gets a port of its own;
// context-routing groups (several services sharing one port) require explicit
// numeric ports.
const autoListenPort = "auto"

// defaultAutoListenPortDatabasePath is where chosen ports are persisted when
// AutoListenPortDatabasePath is not configured.
const defaultAutoListenPortDatabasePath = "auto-listen-ports.db"

// listenPortStore persists the last automatically selected listen port of
// every service in a small SQLite database, so restarts keep serving services
// on stable ports whenever possible.
type listenPortStore struct {
	db *sql.DB
}

func openListenPortStore(databasePath string) (*listenPortStore, error) {
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open listen port database %s: %w", databasePath, err)
	}
	// SQLite handles one writer at a time; a single connection avoids
	// "database is locked" errors without needing WAL tuning for what is
	// a handful of startup writes.
	db.SetMaxOpenConns(1)
	_, err = db.Exec(
		"CREATE TABLE IF NOT EXISTS service_listen_ports (" +
			"service_name TEXT PRIMARY KEY," +
			"port INTEGER NOT NULL," +
			"updated_at TEXT NOT NULL DEFAULT (datetime('now')))",
	)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to initialize listen port database %s: %w", databasePath, err)
	}
	return &listenPortStore{db: db}, nil
}

// getLastUsedPort returns the port this service was last assigned, if any.
func (s *listenPortStore) getLastUsedPort(serviceName string) (int, bool, error) {
	var port int
	err := s.db.QueryRow(
		"SELECT port FROM service_listen_ports WHERE service_name = ?", serviceName,
	).Scan(&port)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("failed to look up last used port for service %s: %w", serviceName, err)
	}
	return port, true, nil
}

// saveUsedPort records the port assigned to a service, replacing any previous
// value.
func (s *listenPortStore) saveUsedPort(serviceName string, port int) error {
	_, err := s.db.Exec(
		"INSERT INTO service_listen_ports (service_name, port, updated_at) VALUES (?, ?, datetime('now')) "+
			"ON CONFLICT(service_name) DO UPDATE SET port = excluded.port, updated_at = excluded.updated_at",
		serviceName, port,
	)
	if err != nil {
		return fmt.Errorf("failed to save port %d for service %s: %w", port, serviceName, err)
	}
	return nil
}

func (s *listenPortStore) close() error {
	return s.db.Close()
}

// allocateListenPort binds a listen port for one service. If the service used
// a port before and that port is still free, it is reused so clients keep
// working across restarts; otherwise a fresh port is requested from the OS.
// isPortTaken is consulted for every candidate and lets the caller keep
// candidates away from ports that are already spoken for (statically
// configured ports, ports assigned moments ago, ...). The returned listener
// already holds the port, closing the race between allocation and use.
func allocateListenPort(lastUsedPort int, hasLastUsedPort bool, isPortTaken func(port int) bool) (net.Listener, int, error) {
	if hasLastUsedPort && lastUsedPort > 0 {
		listener, err := net.Listen("tcp", ":"+strconv.Itoa(lastUsedPort))
		if err == nil && !isPortTaken(lastUsedPort) {
			return listener, lastUsedPort, nil
		}
		if listener != nil {
			_ = listener.Close()
		}
		log.Printf("Last used listen port %d is not available, selecting a new one", lastUsedPort)
	}

	// Ask the OS for a free port. The OS does not know about ports this proxy
	// will only bind later (statically configured ones), so candidates
	// reported as taken are released and retried.
	for attempt := 0; attempt < 100; attempt++ {
		listener, err := net.Listen("tcp", ":0")
		if err != nil {
			return nil, 0, fmt.Errorf("failed to request a free port from the OS: %w", err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		if !isPortTaken(port) {
			return listener, port, nil
		}
		_ = listener.Close()
	}
	return nil, 0, fmt.Errorf("could not find a free listen port that does not conflict with the configuration after 100 attempts")
}

func closeListeners(listeners map[string]net.Listener) {
	for _, listener := range listeners {
		_ = listener.Close()
	}
}

// resolveAutoListenPorts replaces the "auto" ListenPort of every service with
// a concrete port number, mutating config in place, and returns the already
// bound listener for each of those services. Ports are persisted through the
// store so that a service keeps its port across restarts whenever possible.
func resolveAutoListenPorts(config *Config, store *listenPortStore) (map[string]net.Listener, error) {
	listeners := make(map[string]net.Listener)

	takenPorts := make(map[int]bool)
	for _, port := range []string{config.OpenAiApi.ListenPort, config.ManagementApi.ListenPort} {
		if portNumber, err := strconv.Atoi(port); err == nil {
			takenPorts[portNumber] = true
		}
	}

	for serviceIndex := range config.Services {
		if config.Services[serviceIndex].ListenPort != autoListenPort {
			if portNumber, err := strconv.Atoi(config.Services[serviceIndex].ListenPort); err == nil {
				takenPorts[portNumber] = true
			}
			continue
		}
		if store == nil {
			return nil, fmt.Errorf("service %s uses ListenPort \"auto\" but no listen port database is available; configure AutoListenPortDatabasePath", config.Services[serviceIndex].Name)
		}

		serviceName := config.Services[serviceIndex].Name
		lastUsedPort, hasLastUsedPort, err := store.getLastUsedPort(serviceName)
		if err != nil {
			return nil, err
		}

		isPortTaken := func(port int) bool { return takenPorts[port] }
		listener, port, err := allocateListenPort(lastUsedPort, hasLastUsedPort, isPortTaken)
		if err != nil {
			closeListeners(listeners)
			return nil, fmt.Errorf("failed to allocate a listen port for service %s: %w", serviceName, err)
		}

		if err := store.saveUsedPort(serviceName, port); err != nil {
			_ = listener.Close()
			closeListeners(listeners)
			return nil, err
		}
		takenPorts[port] = true
		listeners[serviceName] = listener
		config.Services[serviceIndex].ListenPort = strconv.Itoa(port)
		log.Printf("[%s] Automatically selected listen port %d", serviceName, port)
	}
	return listeners, nil
}
