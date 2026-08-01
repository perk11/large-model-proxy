package main

import (
	"flag"
	"fmt"
	"log"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type RunningService struct {
	manageMutex           *channelMutex
	cmd                   *exec.Cmd
	isWaitingForResources bool
	isReady               bool
	lastUsed              *time.Time
	idleTimer             *time.Timer
	exitWaitGroup         *sync.WaitGroup
	resourcesReleased     *bool
	resourcesReserved     bool
	stdoutWriter          *serviceLoggingWriter
	stderrWriter          *serviceLoggingWriter
}
type ServiceConnectionStats struct {
	proxied int
	waiting int
}
type ResourceManager struct {
	serviceMutex      *sync.Mutex    //reads and writes to resourcesInUse, resourcesReserved, runningServices. Never lock while connectionStatsMutex is locked
	resourcesInUse    map[string]int // used by services that are currently starting or running
	runningServices   map[string]*RunningService
	resourcesReserved map[string]int // used by services that are currently starting but have not yet passed the health check

	resourcesAvailableMutex *sync.Mutex
	resourcesAvailable      map[string]int // if CheckCommand is used, the result returned by CheckCommand. Otherwise, unused

	connectionStatsMutex *sync.Mutex //never lock serviceMutex when this is locked
	connectionStats      map[string]ServiceConnectionStats

	monitorUnpauseChansMutex *sync.Mutex
	monitorUnpauseChans      map[string]chan struct{} // writing on this channel makes monitor run the command immediately

	resourceChangeByResourceMutex          *sync.Mutex //Also covers checkCommandFirstChangeByResourceChans
	checkCommandFirstChangeByResourceChans map[string]map[string]chan struct{}
	resourceChangeByResourceChans          map[string]map[string]chan bool // value sent down the channel is the next "firstCheckNeeded" for the waiter: true = cached amount may be stale, re-run CheckCommand; false = amount was just updated by the monitor, trust the cache
}

// maybeGetRunningService looks up a running service under serviceMutex. During
// shutdown it TryLocks instead of blocking (the stop loop holds serviceMutex),
// reporting the service as gone if the lock cannot be acquired rather than
// reading the map unsynchronized. The shutdown path bypasses this via
// stopRunningService.
func (rm ResourceManager) maybeGetRunningService(name string) (*RunningService, bool) {
	if interrupted.Load() {
		if rm.serviceMutex.TryLock() {
			defer rm.serviceMutex.Unlock()
		} else {
			// Shutting down and the lock is held by the stop loop: do not read
			// the map without the lock.
			return nil, false
		}
	} else {
		rm.serviceMutex.Lock()
		defer rm.serviceMutex.Unlock()
	}
	rs, ok := rm.runningServices[name]
	return rs, ok
}

func (rm ResourceManager) incrementConnection(name string, proxiedConnectionsCountChange int, waitingConnectionsCountChange int) {
	rm.connectionStatsMutex.Lock()
	connectionStats := rm.connectionStats[name]
	connectionStats.proxied += proxiedConnectionsCountChange
	connectionStats.waiting += waitingConnectionsCountChange
	if connectionStats.proxied < 0 || connectionStats.waiting < 0 {
		log.Printf("[%s] ERROR: Negative connection count. Proxied: %d. Waiting: %d. Clamping to 0.", name, connectionStats.proxied, connectionStats.waiting)
		if connectionStats.proxied < 0 {
			connectionStats.proxied = 0
		}
		if connectionStats.waiting < 0 {
			connectionStats.waiting = 0
		}
	}
	rm.connectionStats[name] = connectionStats
	rm.connectionStatsMutex.Unlock()

	if connectionStats.proxied == 0 && connectionStats.waiting == 0 {
		if config.LogLevel == LogLevelDebug {
			log.Printf("[%s] All connections closed, sending resourceChange event", name)
		}
		if serviceConfig, ok := serviceConfigByName[name]; ok && serviceConfig != nil {
			rm.broadcastResourceChanges(maps.Keys(serviceConfig.ResourceRequirements), true)
		}
	}
}

var (
	config              Config
	serviceConfigByName map[string]*ServiceConfig
	resourceManager     ResourceManager
	interrupted         atomic.Bool
)

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	exit := make(chan os.Signal, 1)
	signal.Notify(exit, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)

	configFilePath := flag.String("c", "", "path to the config file. If not specified, will look for config.json or config.jsonc in the current directory")
	flag.Parse()

	if (*configFilePath) == "" {
		if _, err := os.Stat("config.json"); err == nil {
			*configFilePath = "config.json"
		} else if _, err := os.Stat("config.jsonc"); err == nil {
			*configFilePath = "config.jsonc"
		} else {
			FprintfError("Could not find config file. Please specify the path to the config file using the -c flag or create a config.jsonc file in the current directory\n")
			os.Exit(1)
		}
	}
	var err error
	config, err = loadConfig(*configFilePath)
	if err != nil {
		log.Printf("Error loading %s:\n", *configFilePath)
		FprintfError("%v\n", err)
		os.Exit(1)
	}
	resourceManager = ResourceManager{
		resourcesInUse:                         make(map[string]int, len(config.ResourcesAvailable)),
		resourcesReserved:                      make(map[string]int, len(config.ResourcesAvailable)),
		resourcesAvailable:                     make(map[string]int, len(config.ResourcesAvailable)),
		runningServices:                        make(map[string]*RunningService),
		serviceMutex:                           &sync.Mutex{},
		resourcesAvailableMutex:                &sync.Mutex{},
		resourceChangeByResourceMutex:          &sync.Mutex{},
		monitorUnpauseChansMutex:               &sync.Mutex{},
		connectionStatsMutex:                   &sync.Mutex{},
		connectionStats:                        make(map[string]ServiceConnectionStats, len(config.Services)),
		monitorUnpauseChans:                    make(map[string]chan struct{}),
		checkCommandFirstChangeByResourceChans: make(map[string]map[string]chan struct{}),
		resourceChangeByResourceChans:          make(map[string]map[string]chan bool, len(config.ResourcesAvailable)),
	}

	serviceConfigByName = make(map[string]*ServiceConfig, len(config.Services))
	for serviceIndex := range config.Services {
		serviceName := config.Services[serviceIndex].Name
		serviceConfigByName[serviceName] = &config.Services[serviceIndex]
		resourceManager.connectionStats[serviceName] = ServiceConnectionStats{proxied: 0, waiting: 0}
	}

	for name, resource := range config.ResourcesAvailable {
		resourceManager.resourcesAvailable[name] = 0
		resourceManager.resourcesInUse[name] = 0
		resourceManager.resourcesReserved[name] = 0
		resourceManager.resourceChangeByResourceChans[name] = make(map[string]chan bool)

		if resource.CheckCommand != "" {
			resourceManager.monitorUnpauseChans[name] = make(chan struct{}, 1) // capacity 1: see sendSignalToChannels — UnpauseResourceAvailabilityMonitoring sends non-blocking, so a signal arriving before the monitor re-enters its receive must be buffered, not dropped
			resourceManager.checkCommandFirstChangeByResourceChans[name] = make(map[string]chan struct{})
			go monitorResourceAvailability(
				name,
				resource.CheckCommand,
				time.Duration(resource.CheckWhenNotEnoughIntervalMilliseconds)*time.Millisecond,
				resourceManager.monitorUnpauseChans[name],
				&resourceManager,
			)
		}
	}
	for _, service := range config.Services {
		if service.ListenPort != "" {
			go startProxy(service)
		}
	}
	if config.OpenAiApi.ListenPort != "" {
		go startOpenAiApi(config.OpenAiApi, config.Services)
	}
	if config.ManagementApi.ListenPort != "" {
		go startManagementApi(config.ManagementApi, config.Services)
	}

	for {
		receivedSignal := <-exit
		log.Printf("Received %s signal, terminating all processes", signalToString(receivedSignal))
		interrupted.Store(true)
		// Stop services directly from the held map: stopService -> maybeGetRunningService
		// would re-TryLock serviceMutex (non-reentrant) and abort every stop. The map is
		// frozen during shutdown (cleanUp is skipped), so ranging it under the lock is safe.
		// no need to unlock as os.Exit will be called
		resourceManager.serviceMutex.Lock()
		for name, runningService := range resourceManager.runningServices {
			stopRunningService(*findServiceConfigByName(name), runningService)
		}
		log.Printf("Done, exiting")
		os.Exit(0)
	}
}
func findServiceConfigByName(serviceName string) *ServiceConfig {
	if service, ok := serviceConfigByName[serviceName]; ok {
		return service
	}
	panic(fmt.Sprintf("Failed to find service config for service %s", serviceName))
}
