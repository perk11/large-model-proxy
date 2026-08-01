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
		// hadListeners reports whether the just-run check had any registered
		// waiter to notify (captured under the broadcast lock, see
		// checkResourceAvailabilityWithKnownCommand). The monitor keeps polling
		// (re-arms the timer) only while at least one waiter exists.
		var hadListeners bool
		select {
		case <-timer.C:
			hadListeners = checkResourceAvailabilityWithKnownCommand(resourceName, checkCommand, resourceManager)
		case _, ok := <-pauseResumeChan:
			if !ok {
				panic("pauseResumeChan closed unexpectedly, crashing to avoid infinite loop")
			}
			// Canonical non-blocking drain: if Stop reports the timer already
			// fired (or was stopped), a value may be sitting in timer.C; pull
			// it out so the timer.Reset below does not surface a stale tick as
			// a spurious immediate re-check. timer.C is only read by this
			// goroutine, so the drain is race-free.
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			if config.LogLevel == LogLevelDebug {
				log.Printf("[Resource Monitor][%s] An immediate check was requested", resourceName)
			}
			hadListeners = checkResourceAvailabilityWithKnownCommand(resourceName, checkCommand, resourceManager)
		}
		if hadListeners {
			timer.Reset(checkInterval)
		}
	}
}

// checkResourceAvailabilityWithKnownCommand runs the resource's CheckCommand,
// caches the result, broadcasts it to any registered waiters, and reports
// whether at least one waiter was registered (so the monitor knows to keep
// polling). hadListeners is captured WHILE holding the broadcast lock — i.e.
// before any waiter can deregister (a waiter only wakes and deregisters after
// this lock is released). Reading the waiter set in a later, separate critical
// section races with a waiter's deregister→re-register window and can
// transiently observe an empty set, which would stop the monitor from polling
// and stall a CheckCommand resource's reported amount (and any waiter on it)
// until the waiter's maxWait deadline.
func checkResourceAvailabilityWithKnownCommand(resourceName string, checkCommand string, resourceManager *ResourceManager) (hadListeners bool) {
	if config.LogLevel == LogLevelDebug {
		log.Printf("[Resource Monitor][%s] Running check command \"%s\"", resourceName, checkCommand)
	}
	cmd := exec.Command("sh", "-c", checkCommand)
	output, err := cmd.Output()
	if err != nil {
		log.Printf("[Resource Monitor][%s] Failed to execute check command \"%s\": %v", resourceName, checkCommand, err)
		// Keep retrying while waiters exist even though this run failed.
		return resourceManager.resourceHasWaiters(resourceName)
	}

	outputString := string(output)
	outputString = strings.TrimSuffix(outputString, "\n")
	resourceIntValue, err := strconv.Atoi(outputString)
	if err != nil {
		log.Printf("[Resource Monitor][%s] Failed to parse check command \"%s\" output: %v. Output:\n%s", resourceName, checkCommand, err, string(output))
		return resourceManager.resourceHasWaiters(resourceName)
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
	hadListeners = len(resourceManager.resourceChangeByResourceChans[resourceName]) > 0 || len(resourceManager.checkCommandFirstChangeByResourceChans[resourceName]) > 0
	resourceManager.resourceChangeByResourceMutex.Unlock()
	return hadListeners
}

// resourceHasWaiters reports whether any service is currently registered for a
// change notification on this resource. Call without holding
// resourceChangeByResourceMutex.
func (rm ResourceManager) resourceHasWaiters(resourceName string) bool {
	rm.resourceChangeByResourceMutex.Lock()
	defer rm.resourceChangeByResourceMutex.Unlock()
	return len(rm.resourceChangeByResourceChans[resourceName]) > 0 || len(rm.checkCommandFirstChangeByResourceChans[resourceName]) > 0
}

// UnpauseResourceAvailabilityMonitoring pokes the per-resource monitor so it
// runs its CheckCommand immediately instead of waiting for the next interval.
// The send is non-blocking (select/default), so the target channel —
// monitorUnpauseChans — must stay buffered (capacity 1); see sendSignalToChannels
// for the rationale. At most one outstanding poke is ever meaningful, since the
// monitor drains and re-runs the command, so capacity 1 is sufficient.
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
	resourceManager.resourceChangeByResourceMutex.Lock()
	for resource := range resources {
		rm.broadcastResourceChangeWhenResourceChangeByResourceMutexIsLocked(resource, recheckNeeded)
	}
	resourceManager.resourceChangeByResourceMutex.Unlock()
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

// sendSignalToChannels fans a single signal out to every registered listener
// using a NON-BLOCKING send (select with a default branch): a listener that
// is not ready to receive is skipped instead of blocking the broadcaster. The
// resource monitor must never stall because one waiter is slow or gone.
//
// Load-bearing invariant: every channel passed in here MUST be buffered
// (capacity >= 1). A waiter registers its channel, then does a little work
// (e.g. dropping its previous "first check" registration) before entering the
// select that receives from it. A broadcast landing in that window has to be
// buffered; with an unbuffered channel it is silently dropped (logged as
// "... channel is blocked") and the waiter then hangs until its maxWaitTime
// deadline. That was the root cause of a service never starting after a peer
// freed resources — see the "should-not-use-an-outdated-resource-check-result"
// regression test and commit f6188c2. All channels used this way are created
// with capacity 1: the two sendSignalToChannels target families
// (resourceChangeByResourceChans, checkCommandFirstChangeByResourceChans) and
// monitorUnpauseChans, which UnpauseResourceAvailabilityMonitoring sends to with
// the same non-blocking pattern.
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
