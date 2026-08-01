package main

import (
	"fmt"
	"log"
	"strings"
	"time"
)

func reserveResources(resourceRequirements map[string]int, requestingService string, clientDisconnected <-chan struct{}) bool {
	var resourceList []string
	if len(resourceRequirements) == 0 {
		return true
	}
	for resource, amount := range resourceRequirements {
		resourceList = append(resourceList, fmt.Sprintf("%s: %d", resource, amount))
	}
	log.Printf("[%s] Attempting to reserve %s", requestingService, strings.Join(resourceList, ", "))

	var missingResource *string = nil
	var maxWaitTime time.Duration
	if config.MaxTimeToWaitForServiceToCloseConnectionBeforeGivingUpSeconds == nil {
		maxWaitTime = 120 * time.Second
	} else {
		maxWaitTime = time.Duration(*config.MaxTimeToWaitForServiceToCloseConnectionBeforeGivingUpSeconds) * time.Second
	}
	maxWaitTimeTimer := time.NewTimer(maxWaitTime)
	defer maxWaitTimeTimer.Stop()
	recheckNeeded := true
	var pendingCheckChannels map[string]chan struct{}
	for {
		resourceManager.serviceMutex.Lock()
		missingResource, pendingCheckChannels = findFirstMissingResourceWhenServiceMutexIsLocked(resourceRequirements, requestingService, true, recheckNeeded)
		if missingResource == nil && len(pendingCheckChannels) == 0 {
			// Resources are actually available
			resourceManager.resourceChangeByResourceMutex.Lock()
			for resource := range resourceRequirements {
				delete(resourceManager.resourceChangeByResourceChans[resource], requestingService)
			}
			resourceManager.resourceChangeByResourceMutex.Unlock()
			for resource, amount := range resourceRequirements {
				resourceManager.resourcesInUse[resource] += amount
				resourceManager.resourcesReserved[resource] += amount
			}
			log.Printf("[%s] Reserved %s", requestingService, strings.Join(resourceList, ", "))
			resourceManager.serviceMutex.Unlock()
			return true
		}
		resourceManager.serviceMutex.Unlock()

		// Some resources are backed by a CheckCommand and need a fresh measurement
		// before availability can be decided. Wait for the monitor to run those
		// commands WITHOUT holding serviceMutex: a CheckCommand can be slow (or even
		// hang), and holding the global lock for its duration would block every
		// other operation (service start/stop, /status, process cleanup, ...). The
		// maxWaitTime deadline and client disconnection are honored here too.
		if len(pendingCheckChannels) > 0 {
			if !waitForFirstCheckCommands(pendingCheckChannels, requestingService, maxWaitTime, maxWaitTimeTimer, clientDisconnected) {
				return false
			}
			// The cached resource amounts are now fresh, so use them directly on the
			// next pass instead of forcing another CheckCommand run.
			recheckNeeded = false
			continue
		}

		earliestLastUsedService := findEarliestLastUsedServiceUsingResource(requestingService, *missingResource)
		if earliestLastUsedService != "" {
			log.Printf("[%s] Stopping service to free resources for %s", earliestLastUsedService, requestingService)
			stopService(*findServiceConfigByName(earliestLastUsedService))
			// The stopped service may have changed the resource state (e.g. an exit
			// script), so the cached amount is now stale: force a fresh CheckCommand
			// run on the next pass instead of trusting the cache.
			recheckNeeded = true
			continue
		}
		log.Printf("[%s] Not enough %s to start and no services eligible to stop. Waiting until enough resources are free or a service using a resource can be stopped.", requestingService, *missingResource)

		// Buffered (size 1): broadcastResourceChanges -> sendSignalToChannels sends
		// non-blocking, so a signal arriving in the window between registering
		// this channel and entering the select below must be buffered rather than
		// dropped — otherwise the waiter hangs until maxWaitTime and a service can
		// fail to start after a peer frees resources. See sendSignalToChannels and
		// the "should-not-use-an-outdated-resource-check-result" regression test.
		resourceChangeServiceChannel := make(chan bool, 1)
		resourceManager.resourceChangeByResourceMutex.Lock()
		if _, ok := resourceManager.resourceChangeByResourceChans[*missingResource][requestingService]; ok {
			log.Printf("[%s] ERROR: Resource %s is already being reserved by this service", requestingService, *missingResource)
		}
		resourceManager.resourceChangeByResourceChans[*missingResource][requestingService] = resourceChangeServiceChannel
		resourceManager.resourceChangeByResourceMutex.Unlock()

		select {
		case recheckNeeded = <-resourceChangeServiceChannel:
			resourceManager.resourceChangeByResourceMutex.Lock()
			delete(resourceManager.resourceChangeByResourceChans[*missingResource], requestingService)
			resourceManager.resourceChangeByResourceMutex.Unlock()

			log.Printf("[%s] Received a resource change event for %s, rechecking if the service can be started now", requestingService, *missingResource)

			// resource state changed; loop to re-evaluate
		case <-maxWaitTimeTimer.C:
			resourceManager.resourceChangeByResourceMutex.Lock()
			delete(resourceManager.resourceChangeByResourceChans[*missingResource], requestingService)
			resourceManager.resourceChangeByResourceMutex.Unlock()
			log.Printf("[%s] Failed to find a service to stop in %v, closing client connection", requestingService, maxWaitTime)
			return false
		case <-clientDisconnected:
			resourceManager.resourceChangeByResourceMutex.Lock()
			delete(resourceManager.resourceChangeByResourceChans[*missingResource], requestingService)
			resourceManager.resourceChangeByResourceMutex.Unlock()
			log.Printf("[%s] Client disconnected while waiting for resources, aborting", requestingService)
			return false
		}
	}
}

func findEarliestLastUsedServiceUsingResource(requestingService string, missingResource string) string {
	earliestTime := time.Now()
	var earliestLastUsedService string

	resourceManager.serviceMutex.Lock()
	defer resourceManager.serviceMutex.Unlock()

	for serviceName := range resourceManager.runningServices {
		if serviceName == requestingService {
			continue
		}
		serviceConfig := findServiceConfigByName(serviceName)
		if serviceConfig.ResourceRequirements[missingResource] == 0 {
			continue
		}
		runningService := resourceManager.runningServices[serviceName]
		if !canBeStopped(serviceName, runningService) {
			continue
		}
		lastUsed := runningService.lastUsed
		if lastUsed != nil {
			timeDifference := lastUsed.Sub(earliestTime)
			if timeDifference < 0 {
				earliestLastUsedService = serviceName
				earliestTime = *lastUsed
			}
		}
	}

	return earliestLastUsedService
}

func findFirstMissingResourceWhenServiceMutexIsLocked(
	resourceRequirements map[string]int,
	requestingService string,
	outputError bool,
	firstCheckNeeded bool,
) (missingResource *string, pendingCheckChannels map[string]chan struct{}) {
	// pendingCheckChannels collects the "first change" channels registered for
	// resources backed by a CheckCommand when a fresh measurement is needed. They
	// are returned to the caller, which waits on them WITHOUT holding
	// serviceMutex (see reserveResources / waitForFirstCheckCommands).
	pendingCheckChannels = make(map[string]chan struct{})

	// First evaluate the statically-tracked resources. If any of them is the
	// bottleneck, return it WITHOUT registering any CheckCommand first-change
	// channels — registering those (and Unpause-ing the monitor) would force a
	// wasteful CheckCommand run for a service that cannot start anyway because of
	// a static shortage.
	for resource, requiredAmount := range resourceRequirements {
		resourceConfig := config.ResourcesAvailable[resource]
		if resourceConfig.CheckCommand != "" {
			continue
		}
		inUseAmount, ok := resourceManager.resourcesInUse[resource]
		if !ok {
			log.Printf(
				"[%s] ERROR: Resource \"%s\" is missing from the list of the resources in use. This shouldn't be happening",
				requestingService,
				resource,
			)
			inUseAmount = 0
		}
		totalAvailableAmount := config.ResourcesAvailable[resource].Amount
		if requiredAmount > totalAvailableAmount-inUseAmount {
			handleNotEnoughResource(requestingService, outputError, false, resource, 0, requiredAmount)
			return &resource, nil
		}
	}

	// All static resources are satisfied; now handle CheckCommand-backed resources.
	for resource, requiredAmount := range resourceRequirements {
		resourceConfig := config.ResourcesAvailable[resource]
		if resourceConfig.CheckCommand == "" {
			continue
		}
		if firstCheckNeeded {
			newChannel := make(chan struct{}, 1) // capacity 1: see sendSignalToChannels — the monitor broadcasts the first CheckCommand result non-blocking, so a result arriving between registration and waitForFirstCheckCommands' receive must be buffered, not dropped
			pendingCheckChannels[resource] = newChannel
			resourceManager.resourceChangeByResourceMutex.Lock()
			resourceManager.checkCommandFirstChangeByResourceChans[resource][requestingService] = newChannel
			resourceManager.resourceChangeByResourceMutex.Unlock()
			UnpauseResourceAvailabilityMonitoring(resource)
		} else {
			currentlyAvailableAmount, enoughOfResource :=
				isEnoughResourceForServiceWithCheckCommandThatRan(
					resource,
					requestingService,
					requiredAmount,
				)
			if !enoughOfResource {
				handleNotEnoughResource(requestingService, outputError, true, resource, currentlyAvailableAmount, requiredAmount)
				return &resource, nil
			}
		}
	}
	return nil, pendingCheckChannels
}

// waitForFirstCheckCommands waits for the resource monitor to run the CheckCommand
// for every resource in pendingCheckChannels, WITHOUT holding serviceMutex. This
// keeps the global lock free while a (potentially slow) CheckCommand executes, and
// honors the overall reservation deadline (maxWaitTimeTimer) and client
// disconnection. It always cleans up its channel registrations before returning.
func waitForFirstCheckCommands(
	pendingCheckChannels map[string]chan struct{},
	requestingService string,
	maxWaitTime time.Duration,
	maxWaitTimeTimer *time.Timer,
	clientDisconnected <-chan struct{},
) bool {
	for _, changeChan := range pendingCheckChannels {
		select {
		case <-changeChan:
			// Monitor ran the CheckCommand for this resource.
		case <-maxWaitTimeTimer.C:
			cleanupFirstChangeChannels(pendingCheckChannels, requestingService)
			log.Printf("[%s] Failed to confirm enough resources in %v, closing client connection", requestingService, maxWaitTime)
			return false
		case <-clientDisconnected:
			cleanupFirstChangeChannels(pendingCheckChannels, requestingService)
			log.Printf("[%s] Client disconnected while waiting for resource checks, aborting", requestingService)
			return false
		}
	}
	// All requested checks ran; drop the registrations so the monitor stops
	// signalling them. The monitor's timer remains armed for one more interval
	// from the last check, which covers the brief window before this service
	// either reserves or registers for resource-change notifications.
	cleanupFirstChangeChannels(pendingCheckChannels, requestingService)
	return true
}

// cleanupFirstChangeChannels removes this service's "first change" channel
// registrations so the monitor stops signalling them.
func cleanupFirstChangeChannels(pendingCheckChannels map[string]chan struct{}, requestingService string) {
	resourceManager.resourceChangeByResourceMutex.Lock()
	for resource := range pendingCheckChannels {
		delete(resourceManager.checkCommandFirstChangeByResourceChans[resource], requestingService)
	}
	resourceManager.resourceChangeByResourceMutex.Unlock()
}

func handleNotEnoughResource(requestingService string, outputError bool, currentlyAvailableAmountIsMeasured bool, resource string, currentlyAvailableAmount int, requiredAmount int) {
	resourceManager.resourceChangeByResourceMutex.Lock()
	//This might be not necessary if there is no CheckCommand
	for _, resourceToCheck := range resourceManager.checkCommandFirstChangeByResourceChans {
		delete(resourceToCheck, requestingService)
	}
	resourceManager.resourceChangeByResourceMutex.Unlock()
	if outputError {
		if currentlyAvailableAmountIsMeasured {
			log.Printf(
				"[%s] Not enough %s to start. Total: %d, Available: %d, Reserved by starting services: %d, Required: %d",
				requestingService,
				resource,
				config.ResourcesAvailable[resource].Amount,
				currentlyAvailableAmount,
				resourceManager.resourcesReserved[resource],
				requiredAmount,
			)
		} else {
			log.Printf(
				"[%s] Not enough %s to start. Total: %d, Reserved by running services: %d, Required: %d",
				requestingService,
				resource,
				config.ResourcesAvailable[resource].Amount,
				resourceManager.resourcesInUse[resource],
				requiredAmount,
			)
		}
	}
}

func isEnoughResourceForServiceWithCheckCommandThatRan(resource string, requestingService string, requiredAmount int) (int, bool) {
	// Use resources reserved instead of used for the calculation as we only need
	// to account for the services that are being started, the ones that already started
	// are accounted for by the check command.
	resourceManager.resourcesAvailableMutex.Lock()
	currentlyAvailableAmount, currentlyAvailableAmountIsMeasured := resourceManager.resourcesAvailable[resource]
	resourceManager.resourcesAvailableMutex.Unlock()
	if !currentlyAvailableAmountIsMeasured {
		log.Printf(
			"[%s] ERROR: Resource \"%s\" is missing from the list of the available resources. This shouldn't be happening",
			requestingService,
			resource,
		)
		currentlyAvailableAmount = 0
	}
	var ok bool
	reservedAmount, ok := resourceManager.resourcesReserved[resource]
	if !ok {
		log.Printf(
			"[%s] ERROR: Resource \"%s\" is missing from the list of the reserved resources. This shouldn't be happening",
			requestingService,
			resource,
		)
		reservedAmount = 0
	}
	enoughOfResource := requiredAmount <= currentlyAvailableAmount-reservedAmount
	return currentlyAvailableAmount, enoughOfResource
}

func trackServiceLastUsed(serviceConfig ServiceConfig, runningServiceMustExist bool) {
	runningService, ok := resourceManager.maybeGetRunningService(serviceConfig.Name)
	if !ok {
		if runningServiceMustExist {
			log.Printf("[%s] Warning: Tried to track service usage, but couldn't find it in the list of running services, it was probably stopped", serviceConfig.Name)
		}
		return
	}
	resourceManager.serviceMutex.Lock()
	now := time.Now()
	runningService.lastUsed = &now
	if runningService.idleTimer != nil {
		runningService.idleTimer.Reset(getIdleTimeout(serviceConfig))
	}
	resourceManager.serviceMutex.Unlock()
}

func canBeStopped(serviceName string, runningService *RunningService) bool {
	if !runningService.manageMutex.TryLock() {
		return false
	}

	defer runningService.manageMutex.Unlock()
	resourceManager.connectionStatsMutex.Lock()
	defer resourceManager.connectionStatsMutex.Unlock()
	connectionStats := resourceManager.connectionStats[serviceName]
	return connectionStats.proxied == 0
}

func releaseResourcesWhenServiceMutexIsLocked(used map[string]int) {
	for resource, amount := range used {
		resourceManager.resourcesInUse[resource] -= amount
	}
}
func releaseReservedResources(reserved map[string]int) {
	resourceManager.serviceMutex.Lock()
	releaseReservedResourcesWhenServiceMutexIsLocked(reserved)
	resourceManager.serviceMutex.Unlock()
}
func releaseReservedResourcesWhenServiceMutexIsLocked(reserved map[string]int) {
	for resource, amount := range reserved {
		resourceManager.resourcesReserved[resource] -= amount
	}
}
