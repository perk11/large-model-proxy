package main

import (
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
	checkTimerResetChan chan struct{},
	pauseResumeChan chan bool,
	resourceManager *ResourceManager,
) {
	// Use a timer-driven loop so we can reset the sleep interval when requested.
	timer := time.NewTimer(0) // fire immediately to perform the initial check
	paused := true
	for {
		select {
		case <-timer.C:
			if !paused {
				checkResourceAvailabilityWithKnownCommand(resourceName, checkCommand, resourceManager)
				// Signal completion to any waiter that the check finished and resource amount was updated
				if ch, ok := resourceManager.monitorCheckDoneChans[resourceName]; ok && ch != nil {
					select {
					case ch <- struct{}{}:
					default:
					}
				}
				// Also signal that this resource may have changed so waiters can re-check
				if ch, ok := resourceManager.resourceChangeChans[resourceName]; ok && ch != nil {
					select {
					case ch <- struct{}{}:
					default:
					}
				}
			}
			// After a check (or if paused), schedule the next interval.
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			if paused {
				// Keep timer stopped while paused.
				continue
			}
			timer.Reset(checkInterval)
		case <-checkTimerResetChan:
			if paused {
				log.Printf("[Resource Monitor][%s] ERROR: check timer reset requested while not paused", resourceName)
				continue
			}
			if !timer.Stop() {
				// Drain if it already fired to avoid immediate trigger race.
				select {
				case <-timer.C:
				default:
				}
			}
			// Reset to zero to run the check right away; after the check, we will schedule the next at checkInterval.
			timer.Reset(0)
		case shouldBePaused := <-pauseResumeChan:
			if shouldBePaused { // pause
				paused = true
				// stop and drain timer so it doesn't fire while paused
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			} else { // resume
				if paused {
					paused = false
					// On resume, trigger immediate check
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					timer.Reset(0)
				}
			}
		}
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
	} else {
		outputString := string(output)
		outputString = strings.TrimSuffix(outputString, "\n")
		resourceIntValue, err := strconv.Atoi(outputString)
		if err == nil {
			if *resourceManager.resourcesAvailable[resourceName] != resourceIntValue { //Don't need a lock for reading since this is the only place which writes
				if config.LogLevel == LogLevelDebug {
					log.Printf("[Resource Monitor][%s] Setting available resource amount to %d", resourceName, resourceIntValue)
				}
				resourceManager.serviceMutex.Lock() //Avoid other places reading while we write
				*resourceManager.resourcesAvailable[resourceName] = resourceIntValue
				resourceManager.serviceMutex.Unlock()
			}
		} else {
			log.Printf("[Resource Monitor][%s] Failed to parse check command \"%s\" output: %v. Output:\n%s", resourceName, checkCommand, err, string(output))
		}

	}
}

// PauseResourceAvailabilityMonitoring pauses running the check for the given resource.
// The monitor loop keeps its own timer and state; this function just signals pause.
func PauseResourceAvailabilityMonitoring(resourceName string) {
	resourceManager.serviceMutex.Lock()
	pauseCh := resourceManager.monitorPauseChans[resourceName]
	resourceManager.serviceMutex.Unlock()
	if pauseCh == nil {
		log.Printf("[Resource Monitor][%s] ERROR: Failed to find a pause channel", resourceName)
		return
	}
	select {
	case pauseCh <- true:
		log.Printf("[Resource Monitor][%s] Monitoring paused", resourceName)
	default:
		// if a signal is already pending, that's fine
	}
}

// ResumeResourceAvailabilityMonitoring resumes checks for the given resource.
// The monitor will reset its timer to run a check immediately upon resume.
func ResumeResourceAvailabilityMonitoring(resourceName string) {
	resourceManager.serviceMutex.Lock()
	pauseCh := resourceManager.monitorPauseChans[resourceName]
	resourceManager.serviceMutex.Unlock()
	if pauseCh == nil {
		log.Printf("[Resource Monitor][%s] ERROR: Failed to find a pause channel", resourceName)
		return
	}
	select {
	case pauseCh <- false:
		log.Printf("[Resource Monitor][%s] Monitoring resumed", resourceName)
	default:
		// if a signal is already pending, that's fine
	}
}

func CheckResourceAvailabilityIfNecessary(resourceName string, resourceManager *ResourceManager, config *Config) {
	resourceRecord, ok := config.ResourcesAvailable[resourceName]
	if !ok {
		log.Printf("[Resource Monitor][%s] ERROR: Failed to find resource record in the resources available map", resourceName)
		return
	}
	if resourceRecord.CheckCommand == "" {
		return
	}
	checkResourceImmediatelyAndWaitForCompletion(resourceName)
}

// checkResourceImmediatelyAndWaitForCompletion triggers an immediate resource availability check
// for the given resource and blocks until the check command finishes and the
// resource amount is updated by the monitor.
func checkResourceImmediatelyAndWaitForCompletion(resourceName string) {
	// Lookup channels under lock to avoid races on the maps
	resourceManager.serviceMutex.Lock()
	resetCh := resourceManager.monitorResetChans[resourceName]
	doneCh := resourceManager.monitorCheckDoneChans[resourceName]
	resourceManager.serviceMutex.Unlock()
	if resetCh == nil || doneCh == nil {
		log.Printf("[Resource Monitor][%s] ERROR: missing reset or done channel", resourceName)
		return
	}
	// Drain any stale completion notifications so we wait for the next check
	for {
		select {
		case <-doneCh:
			// keep draining
		default:
			goto drained
		}
	}

drained:
	// Request an immediate check; if a reset is already pending, that's fine
	select {
	case resetCh <- struct{}{}:
	default:
	}
	// Wait until the monitor completes the check and updates the value
	<-doneCh
}
