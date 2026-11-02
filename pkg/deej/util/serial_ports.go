package util

// SerialPortInfo describes a serial port present on this machine
type SerialPortInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ListSerialPorts returns the serial ports a deej could plausibly be connected to
func ListSerialPorts() []SerialPortInfo {
	return listSerialPorts()
}
