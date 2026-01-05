package main

import (
	"iter"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func monitorResourceAvailability(
	resourceName string,
	checkCommand string,
	checkInterval time.Duration,
	pauseResumeChan chan struct{},
	resourceManager *ResourceManager,
) {
	timer := time.NewTimer(0) // fire immediately to perform the initial check
	for {
		select {
		case <-timer.C:
			checkResourceAvailabilityWithKnownCommand(resourceName, checkCommand, resourceManager)
		case _, ok := <-pauseResumeChan:
			if !ok {
				panic("pauseResumeChan closed unexpectedly, crashing to avoid infinite loop")
			}
			timer.Stop()
			if config.LogLevel == LogLevelDebug {
				log.Printf("[Resource Monitor][%s] An immediate check was requested", resourceName)
			}
			checkResourceAvailabilityWithKnownCommand(resourceName, checkCommand, resourceManager)
		}
		resourceManager.resourceChangeByResourceMutex.Lock()
		if len(resourceManager.resourceChangeByResourceChans[resourceName]) > 0 || len(resourceManager.checkCommandFirstChangeByResourceChans[resourceName]) > 0 {
			timer.Reset(checkInterval)
		}
		resourceManager.resourceChangeByResourceMutex.Unlock()
	}
}

func checkResourceAvailabilityWithKnownCommand(resourceName string, checkCommand string, resourceManager *ResourceManager) {
	if config.LogLevel == LogLevelDebug {
		log.Printf("[Resource Monitor][%s] Running check command \"%s\"", resourceName, checkCommand)
	}
	cmd := exec.Command("sh", "-c", checkCommand)
	output, err := cmd.Output()
	if err != nil {
		log.Printf("[Resource Monitor][%s] Failed to execute check command \"%s\": %v", resourceName, checkCommand, err)
		return
	}

	outputString := string(output)
	outputString = strings.TrimSuffix(outputString, "\n")
	resourceIntValue, err := strconv.Atoi(outputString)
	if err != nil {
		log.Printf("[Resource Monitor][%s] Failed to parse check command \"%s\" output: %v. Output:\n%s", resourceName, checkCommand, err, string(output))
		return
	}
	resourceManager.resourcesAvailableMutex.Lock()
	amountChanged := resourceManager.resourcesAvailable[resourceName] != resourceIntValue
	if amountChanged {
		if config.LogLevel == LogLevelDebug {
			log.Printf("[Resource Monitor][%s] Setting available resource amount to %d", resourceName, resourceIntValue)
		}
		resourceManager.resourcesAvailable[resourceName] = resourceIntValue
	}
	resourceManager.resourcesAvailableMutex.Unlock()

	resourceManager.resourceChangeByResourceMutex.Lock()
	resourceManager.broadcastFirstChangeIfMutexIsLocked(resourceName)
	if amountChanged {
		resourceManager.broadcastResourceChangeWhenResourceChangeByResourceMutexIsLocked(resourceName, false)
	}

	resourceManager.resourceChangeByResourceMutex.Unlock()
}

func UnpauseResourceAvailabilityMonitoring(resourceName string) {
	if config.LogLevel == LogLevelDebug {
		log.Printf("[Resource Monitor][%s] Getting a lock to send unpause monitoring signal", resourceName)
	}
	resourceManager.monitorUnpauseChansMutex.Lock()
	pauseCh := resourceManager.monitorUnpauseChans[resourceName]
	resourceManager.monitorUnpauseChansMutex.Unlock()
	if pauseCh == nil {
		log.Printf("[Resource Monitor][%s] ERROR: Failed to find an unpause channel", resourceName)
		return
	}
	select {
	case pauseCh <- struct{}{}:
		if config.LogLevel == LogLevelDebug {
			log.Printf("[Resource Monitor][%s] Requesting an immediate CommandCheck run", resourceName)
		}
	default:
		log.Printf("[Resource Monitor][%s] Immediate CommandCheck run is already requested", resourceName)
	}
}

func (rm ResourceManager) broadcastResourceChanges(resources iter.Seq[string], recheckNeeded bool) {
	for resource := range resources {
		rm.broadcastResourceChangeWhenResourceChangeByResourceMutexIsLocked(resource, recheckNeeded)
	}
}
func (rm ResourceManager) broadcastFirstChangeIfMutexIsLocked(resourceName string) {
	resourceChangeByResourceChans, ok := rm.checkCommandFirstChangeByResourceChans[resourceName]
	if !ok {
		return //map is not initialized for resources without CheckCommand, so it being missing is ok
	}
	sendSignalToChannels(resourceChangeByResourceChans, resourceName, "checkCommandFirstChangeByResourceChans", struct{}{})
}
func (rm ResourceManager) broadcastResourceChangeWhenResourceChangeByResourceMutexIsLocked(resourceName string, recheckNeeded bool) {
	serviceChannels, ok := rm.resourceChangeByResourceChans[resourceName]
	if !ok {
		log.Printf("[Resource Monitor][%s] ERROR: resourceChangeByResourceChans map is not initialized", resourceName)
		return
	}
	sendSignalToChannels(serviceChannels, resourceName, "resourceChangeByResourceChans", recheckNeeded)
}

func sendSignalToChannels[T any](
	serviceChannels map[string]chan T,
	resourceName string,
	channelName string,
	signalValue T,
) {
	for serviceName, resourceChangeChannel := range serviceChannels {
		if config.LogLevel == LogLevelDebug {
			log.Printf("[Resource Monitor][%s] Sending an update to %s channel for service %q: %v", resourceName, channelName, serviceName, signalValue)
		}

		select {
		case resourceChangeChannel <- signalValue:
		default:
			log.Printf("[Resource Monitor][%s] ERROR: %s channel for service %q is blocked", resourceName, channelName, serviceName)
		}
	}
}
