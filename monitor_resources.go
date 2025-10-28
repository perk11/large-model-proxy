package main

import (
	"log"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func monitorResourceAvailability(resourceName string, checkCommand string, checkInterval time.Duration, resourceManager *ResourceManager) {
	for {
		log.Printf("[Resource Monitor][%s] Running check command \"%s\"", resourceName, checkCommand)
		cmd := exec.Command("sh", "-c", checkCommand)
		output, err := cmd.Output()
		if err != nil {
			log.Printf("[Resource Monitor][%s] Failed to start check command \"%s\": %v", resourceName, checkCommand, err)
		} else {
			outputString := string(output)
			outputString = strings.TrimSuffix(outputString, "\n")
			resourceIntValue, err := strconv.Atoi(outputString)
			if err == nil {
				log.Printf("[Resource Monitor][%s] Setting available resource amount to %d", resourceName, resourceIntValue)
				resourceManager.serviceMutex.Lock()
				resourceManager.resourcesAvailable[resourceName] = resourceIntValue
				resourceManager.serviceMutex.Unlock()
			} else {
				log.Printf("[Resource Monitor][%s] Failed to parse check command \"%s\" output: %v. Output:\n%s", resourceName, checkCommand, err, string(output))
			}

		}
		time.Sleep(checkInterval)
	}
}
