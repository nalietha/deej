package deej

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jacobsa/go-serial/serial"
	"go.uber.org/zap"

	"github.com/omriharel/deej/pkg/deej/util"
)

// SerialIO provides a deej-aware abstraction layer to managing serial I/O.
// it keeps itself connected: finding the board (when the port is "auto"),
// and reconnecting whenever it's unplugged or the connection settings change
type SerialIO struct {
	deej   *Deej
	logger *zap.SugaredLogger

	stopChannel chan bool
	stopOnce    sync.Once
	stopped     int32

	// set to make the current connection drop and be re-established
	reconnectRequested int32

	// set to make the next read line emit move events for every slider
	forceResend int32

	// guards conn, status and connectedWith
	lock          sync.Mutex
	conn          io.ReadWriteCloser
	status        SerialStatus
	connectedWith string

	// only touched by the reading goroutine
	lastKnownNumSliders        int
	currentSliderPercentValues []float32

	sliderMoveConsumers []chan SliderMoveEvent
}

// SerialStatus is a point-in-time view of the connection and the latest pot readings
type SerialStatus struct {
	State      string    `json:"state"`
	Port       string    `json:"port"`
	BaudRate   int       `json:"baud_rate"`
	AutoDetect bool      `json:"auto_detect"`
	Error      string    `json:"error,omitempty"`
	NumSliders int       `json:"num_sliders"`
	Raw        []int     `json:"raw"`
	Values     []float32 `json:"values"`
}

// SliderMoveEvent represents a single slider move captured by deej
type SliderMoveEvent struct {
	SliderID     int
	PercentValue float32
}

// SerialPortProbe is the result of checking a single serial port for a deej
type SerialPortProbe struct {
	util.SerialPortInfo
	IsDeej     bool `json:"is_deej"`
	Connected  bool `json:"connected"`
	NumSliders int  `json:"num_sliders"`
}

const (
	serialStateSearching    = "searching"
	serialStateConnecting   = "connecting"
	serialStateConnected    = "connected"
	serialStateDisconnected = "disconnected"

	retryInterval = 2 * time.Second

	// arduinos that reset on connect (uno, nano) take a moment to boot before they start talking
	probeTimeout = 3500 * time.Millisecond

	// lines needed to be convinced a port is actually a deej
	probeLinesRequired = 3

	// with no data for this long, check whether the device is still around at all
	silenceBeforeLivenessCheck = 2 * time.Second
)

var (
	expectedLinePattern = regexp.MustCompile(`^\d{1,4}(\|\d{1,4})*\r\n$`)

	errNoDeejFound = errors.New("no deej found on any serial port")
	errDeviceGone  = errors.New("serial device disappeared")
)

// NewSerialIO creates a SerialIO instance that uses the provided deej
// instance's connection info to establish communications with the arduino chip
func NewSerialIO(deej *Deej, logger *zap.SugaredLogger) (*SerialIO, error) {
	logger = logger.Named("serial")

	sio := &SerialIO{
		deej:                deej,
		logger:              logger,
		stopChannel:         make(chan bool),
		sliderMoveConsumers: []chan SliderMoveEvent{},
		status:              SerialStatus{State: serialStateDisconnected},
	}

	logger.Debug("Created serial i/o instance")

	// respond to config changes
	sio.setupOnConfigReload()

	return sio, nil
}

// Start begins keeping a connection to our arduino chip alive in the background
func (sio *SerialIO) Start() {
	go sio.superviseConnection()
}

// Stop shuts down our serial connection, if one is active, and stops reconnecting
func (sio *SerialIO) Stop() {
	sio.stopOnce.Do(func() {
		sio.logger.Debug("Shutting down serial connection")
		atomic.StoreInt32(&sio.stopped, 1)
		close(sio.stopChannel)
		sio.closeConn()
	})
}

// Reconnect drops the current connection (if any) and immediately tries again
func (sio *SerialIO) Reconnect() {
	atomic.StoreInt32(&sio.reconnectRequested, 1)
	sio.closeConn()
}

// Status returns a copy of the current connection status and pot readings
func (sio *SerialIO) Status() SerialStatus {
	sio.lock.Lock()
	defer sio.lock.Unlock()

	status := sio.status
	status.Raw = append([]int{}, sio.status.Raw...)
	status.Values = append([]float32{}, sio.status.Values...)

	return status
}

// SubscribeToSliderMoveEvents returns an unbuffered channel that receives
// a sliderMoveEvent struct every time a slider moves
func (sio *SerialIO) SubscribeToSliderMoveEvents() chan SliderMoveEvent {
	ch := make(chan SliderMoveEvent)
	sio.sliderMoveConsumers = append(sio.sliderMoveConsumers, ch)

	return ch
}

// ProbePorts lists serial ports and checks each one for a deej. The port we're connected to is reported as-is
func (sio *SerialIO) ProbePorts() []SerialPortProbe {
	_, baudRate := sio.deej.config.Connection()
	status := sio.Status()

	ports := util.ListSerialPorts()
	results := make([]SerialPortProbe, len(ports))

	var wg sync.WaitGroup
	for idx, port := range ports {
		results[idx].SerialPortInfo = port

		// can't open the port we're already holding - but we know what's on it
		if status.State == serialStateConnected && port.Name == status.Port {
			results[idx].IsDeej = true
			results[idx].Connected = true
			results[idx].NumSliders = status.NumSliders
			continue
		}

		wg.Add(1)
		go func(idx int, name string) {
			defer wg.Done()
			results[idx].NumSliders, results[idx].IsDeej = sio.probePort(name, baudRate)
		}(idx, port.Name)
	}

	wg.Wait()

	return results
}

func (sio *SerialIO) superviseConnection() {
	lastFailure := ""

	for !sio.isStopped() {
		atomic.StoreInt32(&sio.reconnectRequested, 0)

		err := sio.connectAndRead()
		if sio.isStopped() {
			return
		}

		reconnectNow := atomic.LoadInt32(&sio.reconnectRequested) == 1

		if err != nil && !reconnectNow {
			sio.setState(serialStateDisconnected, "", err)

			// only bother the user once per kind of failure, until things work again
			if failure := err.Error(); failure != lastFailure {
				lastFailure = failure
				sio.notifyFailure(err)
			}
		} else {
			lastFailure = ""
		}

		if reconnectNow {
			continue
		}

		select {
		case <-sio.stopChannel:
			return
		case <-time.After(retryInterval):
		}
	}
}

// connectAndRead finds and opens the port, then reads from it until the connection drops
func (sio *SerialIO) connectAndRead() error {
	configuredPort, baudRate := sio.deej.config.Connection()
	autoDetect := configuredPort == autoCOMPort

	sio.lock.Lock()
	sio.status.AutoDetect = autoDetect
	sio.status.BaudRate = baudRate
	sio.connectedWith = connectionSpec(configuredPort, baudRate)
	sio.lock.Unlock()

	portName := configuredPort
	if autoDetect {
		sio.setState(serialStateSearching, "", nil)

		var err error
		if portName, err = sio.detectDeejPort(baudRate); err != nil {
			return err
		}
	}

	sio.setState(serialStateConnecting, portName, nil)

	conn, err := serial.Open(sio.connectionOptions(portName, baudRate))
	if err != nil {
		sio.logger.Warnw("Failed to open serial connection", "port", portName, "error", err)
		return fmt.Errorf("open serial connection: %w", err)
	}

	sio.lock.Lock()
	sio.conn = conn
	sio.lock.Unlock()

	defer sio.closeConn()

	namedLogger := sio.logger.Named(strings.ToLower(portName))
	namedLogger.Infow("Connected", "port", portName, "baudRate", baudRate, "autoDetected", autoDetect)

	sio.lastKnownNumSliders = 0
	sio.setState(serialStateConnected, portName, nil)
	sio.deej.notifier.Notify("deej connected", fmt.Sprintf("Listening on %s", portName))

	err = sio.readLines(conn, portName, func(line string) { sio.handleLine(namedLogger, line) })

	if !sio.isStopped() && atomic.LoadInt32(&sio.reconnectRequested) == 0 {
		namedLogger.Infow("Connection lost", "error", err)
		sio.deej.notifier.Notify("deej disconnected", "Reconnecting automatically as soon as it's back.")
	}

	return err
}

// readLines feeds complete lines to onLine until the port errors out, disappears, or we're told to let go
func (sio *SerialIO) readLines(conn io.Reader, portName string, onLine func(string)) error {
	buf := make([]byte, 512)
	pending := []byte{}
	lastData := time.Now()
	lastLivenessCheck := time.Now()

	for !sio.isStopped() && atomic.LoadInt32(&sio.reconnectRequested) == 0 {
		n, err := conn.Read(buf)

		if n > 0 {
			lastData = time.Now()
			pending = append(pending, buf[:n]...)

			for {
				newline := bytes.IndexByte(pending, '\n')
				if newline < 0 {
					break
				}

				line := string(pending[:newline+1])
				pending = pending[newline+1:]

				if sio.deej.Verbose() {
					sio.logger.Debugw("Read new line", "line", line)
				}

				onLine(line)
			}

			// never let garbage without newlines pile up
			if len(pending) > 4096 {
				pending = pending[:0]
			}
		}

		// reads time out periodically and come back empty (possibly with io.EOF) - that's fine
		if err != nil && err != io.EOF {
			return fmt.Errorf("read from serial: %w", err)
		}

		if n == 0 {

			// a vanished device node means it was unplugged (linux keeps the fd quietly readable)
			if time.Since(lastData) > silenceBeforeLivenessCheck && time.Since(lastLivenessCheck) > time.Second {
				lastLivenessCheck = time.Now()

				if util.Linux() {
					if _, statErr := os.Stat(portName); statErr != nil {
						return errDeviceGone
					}
				}
			}

			// windows reads return immediately when there's nothing to read, don't spin
			time.Sleep(5 * time.Millisecond)
		}
	}

	return nil
}

func (sio *SerialIO) detectDeejPort(baudRate int) (string, error) {
	ports := util.ListSerialPorts()
	if len(ports) == 0 {
		return "", errNoDeejFound
	}

	sio.logger.Debugw("Probing serial ports for a deej", "ports", ports)

	found := make(chan string, len(ports))

	var wg sync.WaitGroup
	for _, port := range ports {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			if _, ok := sio.probePort(name, baudRate); ok {
				found <- name
			}
		}(port.Name)
	}

	go func() {
		wg.Wait()
		close(found)
	}()

	name, ok := <-found
	if !ok {
		return "", errNoDeejFound
	}

	// let the other probes release their ports before we open ours
	wg.Wait()

	sio.logger.Infow("Found deej", "port", name)

	return name, nil
}

// probePort briefly listens on a port and reports whether it speaks deej's protocol
func (sio *SerialIO) probePort(portName string, baudRate int) (int, bool) {
	conn, err := serial.Open(sio.connectionOptions(portName, baudRate))
	if err != nil {
		return 0, false
	}

	defer conn.Close()

	validLines := 0
	numSliders := 0
	deadline := time.Now().Add(probeTimeout)

	buf := make([]byte, 256)
	pending := []byte{}

	for time.Now().Before(deadline) && validLines < probeLinesRequired {
		n, err := conn.Read(buf)
		if err != nil && err != io.EOF {
			return 0, false
		}

		if n == 0 {
			time.Sleep(5 * time.Millisecond)
			continue
		}

		pending = append(pending, buf[:n]...)
		for {
			newline := bytes.IndexByte(pending, '\n')
			if newline < 0 {
				break
			}

			line := string(pending[:newline+1])
			pending = pending[newline+1:]

			if expectedLinePattern.MatchString(line) {
				validLines++
				numSliders = strings.Count(line, "|") + 1
			}
		}

		if len(pending) > 4096 {
			pending = pending[:0]
		}
	}

	return numSliders, validLines >= probeLinesRequired
}

func (sio *SerialIO) connectionOptions(portName string, baudRate int) serial.OpenOptions {
	options := serial.OpenOptions{
		PortName: portName,
		BaudRate: uint(baudRate),
		DataBits: 8,
		StopBits: 1,

		// windows: a minimum read size of 0 prevents a rare bug where serial reads get congested,
		// resulting in significant lag. reads return immediately when there's nothing to read
		MinimumReadSize: 0,
	}

	// linux: time reads out every 200ms, so we can notice being stopped or unplugged instead of blocking forever
	if util.Linux() {
		options.InterCharacterTimeout = 200
	}

	return options
}

func (sio *SerialIO) setupOnConfigReload() {
	configReloadedChannel := sio.deej.config.SubscribeToChanges()

	const resendDelay = 50 * time.Millisecond

	go func() {
		for range configReloadedChannel {

			// make any config reload re-send all slider values to ensure process volumes are being re-set
			// (this is what makes switching profiles apply immediately). this needs to happen after a small
			// delay, because the session map will also re-acquire sessions whenever the config is reloaded,
			// and we don't want it to receive these move events while the map is still cleared
			go func() {
				<-time.After(resendDelay)
				atomic.StoreInt32(&sio.forceResend, 1)
			}()

			// if connection params have changed, renew the connection
			sio.lock.Lock()
			changed := sio.connectedWith != connectionSpec(sio.deej.config.Connection())
			sio.lock.Unlock()

			if changed {
				sio.logger.Info("Detected change in connection parameters, renewing connection")
				sio.Reconnect()
			}
		}
	}()
}

func (sio *SerialIO) closeConn() {
	sio.lock.Lock()
	defer sio.lock.Unlock()

	if sio.conn == nil {
		return
	}

	if err := sio.conn.Close(); err != nil {
		sio.logger.Warnw("Failed to close serial connection", "error", err)
	} else {
		sio.logger.Debug("Serial connection closed")
	}

	sio.conn = nil
}

func (sio *SerialIO) setState(state string, port string, err error) {
	sio.lock.Lock()
	defer sio.lock.Unlock()

	sio.status.State = state
	sio.status.Port = port
	sio.status.Error = ""

	if err != nil {
		sio.status.Error = err.Error()
	}

	if state != serialStateConnected {
		sio.status.NumSliders = 0
		sio.status.Raw = nil
		sio.status.Values = nil
	}
}

func (sio *SerialIO) notifyFailure(err error) {
	port, _ := sio.deej.config.Connection()

	switch {
	case errors.Is(err, errNoDeejFound):
		sio.deej.notifier.Notify("Looking for your deej...",
			"No deej found on any serial port yet. Plug it in - deej will connect by itself.")

	case errors.Is(err, os.ErrPermission):
		hint := "Make sure to close any serial monitor or other deej instance."
		if util.Linux() {
			hint = "Close any serial monitor, and make sure you're in the 'dialout' group."
		}

		sio.deej.notifier.Notify(fmt.Sprintf("Can't connect to %s!", port), "This serial port is busy. "+hint)

	case errors.Is(err, os.ErrNotExist):
		sio.deej.notifier.Notify(fmt.Sprintf("Can't connect to %s!",
			port), "This serial port doesn't exist. Set the port to \"auto\" in the control panel to find it automatically.")
	}
}

func (sio *SerialIO) isStopped() bool {
	return atomic.LoadInt32(&sio.stopped) == 1
}

func connectionSpec(port string, baudRate int) string {
	return fmt.Sprintf("%s@%d", strings.ToLower(port), baudRate)
}

func (sio *SerialIO) handleLine(logger *zap.SugaredLogger, line string) {

	// this function receives an unsanitized line which is guaranteed to end with LF,
	// but most lines will end with CRLF. it may also have garbage instead of
	// deej-formatted values, so we must check for that! just ignore bad ones
	if !expectedLinePattern.MatchString(line) {
		return
	}

	// trim the suffix
	line = strings.TrimSuffix(line, "\r\n")

	// split on pipe (|), this gives a slice of numerical strings between "0" and "1023"
	splitLine := strings.Split(line, "|")
	numSliders := len(splitLine)

	// update our slider count, if needed (or asked to) - this will send slider move events for all
	if numSliders != sio.lastKnownNumSliders || atomic.SwapInt32(&sio.forceResend, 0) == 1 {
		if numSliders != sio.lastKnownNumSliders {
			logger.Infow("Detected sliders", "amount", numSliders)
		}

		sio.lastKnownNumSliders = numSliders
		sio.currentSliderPercentValues = make([]float32, numSliders)

		// reset everything to be an impossible value to force the slider move event later
		for idx := range sio.currentSliderPercentValues {
			sio.currentSliderPercentValues[idx] = -1.0
		}
	}

	noiseReductionLevel := sio.deej.config.NoiseReduction()
	rawValues := make([]int, numSliders)

	// for each slider:
	moveEvents := []SliderMoveEvent{}
	for sliderIdx, stringValue := range splitLine {

		// convert string values to integers ("1023" -> 1023)
		number, _ := strconv.Atoi(stringValue)

		// turns out the first line could come out dirty sometimes (i.e. "4558|925|41|643|220")
		// so let's check the first number for correctness just in case
		if sliderIdx == 0 && number > 1023 {
			sio.logger.Debugw("Got malformed line from serial, ignoring", "line", line)
			return
		}

		rawValues[sliderIdx] = number

		// calibrate, invert and curve the raw value into a volume scalar between 0.0 and 1.0, per this pot's settings
		normalizedScalar := sio.deej.config.Pot(sliderIdx).apply(number)

		// check if it changes the desired state (could just be a jumpy raw slider value)
		if util.SignificantlyDifferent(sio.currentSliderPercentValues[sliderIdx], normalizedScalar, noiseReductionLevel) {

			// if it does, update the saved value and create a move event
			sio.currentSliderPercentValues[sliderIdx] = normalizedScalar

			moveEvents = append(moveEvents, SliderMoveEvent{
				SliderID:     sliderIdx,
				PercentValue: normalizedScalar,
			})

			if sio.deej.Verbose() {
				logger.Debugw("Slider moved", "event", moveEvents[len(moveEvents)-1])
			}
		}
	}

	sio.lock.Lock()
	sio.status.NumSliders = numSliders
	sio.status.Raw = rawValues
	sio.status.Values = append([]float32{}, sio.currentSliderPercentValues...)
	sio.lock.Unlock()

	// deliver move events if there are any, towards all potential consumers
	if len(moveEvents) > 0 {
		for _, consumer := range sio.sliderMoveConsumers {
			for _, moveEvent := range moveEvents {
				consumer <- moveEvent
			}
		}
	}
}
