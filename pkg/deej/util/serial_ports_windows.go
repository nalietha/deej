package util

import (
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/windows/registry"
)

func listSerialPorts() []SerialPortInfo {
	ports := []SerialPortInfo{}

	// windows keeps a live map of serial device -> COM port name here
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `HARDWARE\DEVICEMAP\SERIALCOMM`, registry.QUERY_VALUE)
	if err != nil {
		return ports
	}

	defer key.Close()

	devices, err := key.ReadValueNames(0)
	if err != nil {
		return ports
	}

	for _, device := range devices {
		name, _, err := key.GetStringValue(device)
		if err != nil {
			continue
		}

		ports = append(ports, SerialPortInfo{Name: name, Description: describeSerialDevice(device)})
	}

	// sort COM2 before COM10
	sort.Slice(ports, func(i, j int) bool {
		a, _ := strconv.Atoi(strings.TrimPrefix(strings.ToUpper(ports[i].Name), "COM"))
		b, _ := strconv.Atoi(strings.TrimPrefix(strings.ToUpper(ports[j].Name), "COM"))
		return a < b
	})

	return ports
}

// turns driver device names into something a human recognizes
func describeSerialDevice(device string) string {
	lower := strings.ToLower(device)

	switch {
	case strings.Contains(lower, "usbser"):
		return "USB serial (Arduino / native USB)"
	case strings.Contains(lower, "ch341"), strings.Contains(lower, "wchusb"):
		return "CH340 USB serial"
	case strings.Contains(lower, "vcp"):
		return "FTDI USB serial"
	case strings.Contains(lower, "silab"):
		return "CP210x USB serial"
	}

	return strings.TrimPrefix(device, `\Device\`)
}
