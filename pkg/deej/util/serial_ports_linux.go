package util

import (
	"path/filepath"
	"sort"
	"strings"
)

func listSerialPorts() []SerialPortInfo {

	// friendly names live in /dev/serial/by-id as symlinks, i.e. usb-Arduino_LLC_Arduino_Leonardo-if00 -> ../../ttyACM0
	descriptions := map[string]string{}
	if links, err := filepath.Glob("/dev/serial/by-id/*"); err == nil {
		for _, link := range links {
			if target, err := filepath.EvalSymlinks(link); err == nil {
				description := strings.TrimPrefix(filepath.Base(link), "usb-")
				description = strings.ReplaceAll(description, "_", " ")
				descriptions[target] = description
			}
		}
	}

	ports := []SerialPortInfo{}
	for _, pattern := range []string{"/dev/ttyACM*", "/dev/ttyUSB*"} {
		matches, _ := filepath.Glob(pattern)
		for _, match := range matches {
			ports = append(ports, SerialPortInfo{Name: match, Description: descriptions[match]})
		}
	}

	sort.Slice(ports, func(i, j int) bool { return ports[i].Name < ports[j].Name })

	return ports
}
