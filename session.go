package klipper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/prusa3d/prusa-connect/sdk/connectorapi"
	"github.com/prusa3d/prusa-connect/sdk/fault"
)

// The shipping liveness numbers. Moonraker sends a reading every few hundred
// milliseconds while anything changes, and answers a ping at once, so a minute
// of silence is a connection that has gone away.
const (
	readDeadline = 60 * time.Second
	pingInterval = 20 * time.Second
	pingTimeout  = 10 * time.Second
)

// ErrSessionEnded reports a call made on a session whose connection has gone.
var ErrSessionEnded = errors.New("klipper: the session has ended")

// liveness is how long a connection may say nothing, how often it is asked to
// say something, and how long that asking may take. The zero value takes the
// shipping numbers.
type liveness struct {
	readDeadline time.Duration
	pingInterval time.Duration
	pingTimeout  time.Duration
}

func (l liveness) orShipping() liveness {
	if l.readDeadline <= 0 {
		l.readDeadline = readDeadline
	}

	if l.pingInterval <= 0 {
		l.pingInterval = pingInterval
	}

	if l.pingTimeout <= 0 {
		l.pingTimeout = pingTimeout
	}

	return l
}

// Session is one connected Klipper machine.
//
// It has two lives. During the identity exchange the goroutine inside Connect
// reads the connection itself. After begin, one reader goroutine owns every
// read, and it is the only writer to the patch channel and therefore the only
// thing that closes it.
type Session struct {
	conn     *websocket.Conn
	settings settings
	log      *slog.Logger

	// files is the gcodes folder of this machine: listing, changes, uploads.
	files *files

	// reported is what printer.info answered.
	reported printerInfo
	// asked is the objects the subscription covers, which is also what a
	// status query asks for.
	asked []string

	identity connectorapi.Identity
	caps     connectorapi.Set
	// probe is what this machine answered about itself during the handshake.
	probe connectorapi.Set

	reader reader

	// powerWanted is the power device the endpoint named, and power is the
	// one this session switches. Empty power is a machine with none.
	powerWanted string
	power       string

	// first is the reading the subscription answered with, emitted by the
	// reader goroutine before its loop.
	first []connectorapi.StatePatch

	patches chan connectorapi.StatePatch
	// done closes when the reader has gone.
	done chan struct{}

	nextID atomic.Uint64

	writing sync.Mutex

	pending sync.Map

	closing sync.Once

	dropped atomic.Uint64
	refused atomic.Uint64
}

var (
	_ connectorapi.Session      = (*Session)(nil)
	_ connectorapi.CountsLink   = (*Session)(nil)
	_ connectorapi.ReportsProbe = (*Session)(nil)
	_ connectorapi.ReportsPeer  = (*Session)(nil)
	_ connectorapi.Filesystem   = (*Session)(nil)
	_ connectorapi.FileManager  = (*Session)(nil)
	_ connectorapi.Transfers    = (*Session)(nil)
)

// newSession wraps a connection that is open and has said nothing yet.
func newSession(conn *websocket.Conn, s settings, log *slog.Logger) *Session {
	session := &Session{
		conn:     conn,
		settings: s,
		log:      log,
		patches:  make(chan connectorapi.StatePatch, patchBacklog),
		done:     make(chan struct{}),
	}
	session.reader.storage = s.storage

	return session
}

// handshake performs the identity exchange and the live probe, and returns the
// capability set the machine's own answer narrowed the offer to.
func (s *Session) handshake(ctx context.Context, offered connectorapi.Set) (connectorapi.Set, error) {
	if err := s.callSync(ctx, methodPrinterInfo, nil, &s.reported); err != nil {
		return nil, err
	}

	// Klipper that is shut down still answers, and a person needs to see the
	// error and restart it. Klipper that is starting or not connected to
	// Moonraker answers nothing useful yet, so the host dials again later.
	switch s.reported.State {
	case "ready", "shutdown", "error":
	default:
		return nil, fmt.Errorf("%w: Klipper reports %q: %s",
			ErrKlippyNotReady, s.reported.State, strings.TrimSpace(s.reported.StateMessage))
	}

	var listed objectList
	if err := s.callSync(ctx, methodObjectsList, nil, &listed); err != nil {
		return nil, err
	}

	extruders := extrudersOf(listed.Objects)
	chamber := chamberOf(listed.Objects)

	s.asked = append(present(subscribed, listed.Objects), furtherExtruders(extruders)...)
	if chamber != "" {
		s.asked = append(s.asked, chamber)
	}

	dumped, err := s.readConfig(ctx, listed.Objects)
	if err != nil {
		return nil, err
	}

	s.probeRoot(ctx)

	powered, err := s.probePower(ctx)
	if err != nil {
		return nil, err
	}

	var opening statusReply
	if err = s.callSync(ctx, methodObjectsSubscribe, objectsParam(s.asked), &opening); err != nil {
		return nil, err
	}

	s.reader.heads.count = len(extruders)
	s.reader.chamber = chamber

	s.first = append(toolPatches(dumped, extruders), buildVolumePatches(dumped)...)
	s.first = appendText(s.first, hostnamePath, s.reported.Hostname)
	s.first = append(s.first, powered...)
	s.first = append(s.first, s.reader.patchesFrom(opening.Status, time.Now())...)

	s.probe = probe(machine{
		objects:   listed.Objects,
		heaters:   heaterBounds(dumped, heaterExtruder, heaterBed, objectChamberHeater),
		extruders: len(extruders),
		chamber:   chamber,
		power:     s.power,
	}, offered)

	return offered.Intersect(s.probe), nil
}

// chamberOf is the chamber object a machine has: a heater it can set, or a
// sensor it can only read. Empty for none.
func chamberOf(objects []string) string {
	for _, candidate := range []string{objectChamberHeater, objectChamberSensor} {
		if slices.Contains(objects, candidate) {
			return candidate
		}
	}

	return ""
}

// readConfig asks the machine for its own configuration. A machine that
// reports no configfile object answers an empty configuration.
func (s *Session) readConfig(ctx context.Context, available []string) (configDump, error) {
	if !slices.Contains(available, objectConfigFile) {
		return configDump{}, nil
	}

	var reply statusReply

	params := map[string]any{"objects": map[string]any{objectConfigFile: []string{"settings"}}}
	if err := s.callSync(ctx, methodObjectsQuery, params, &reply); err != nil {
		return configDump{}, err
	}

	var dumped configDump
	if err := decodeResult(methodObjectsQuery, reply.Status[objectConfigFile], &dumped); err != nil {
		return configDump{}, err
	}

	return dumped, nil
}

// furtherExtruders is every extruder but the first, which subscribed already
// names.
func furtherExtruders(extruders []string) []string {
	return slices.DeleteFunc(slices.Clone(extruders), func(object string) bool { return object == objectExtruder })
}

// present is the objects this driver wants that the machine reports.
func present(wanted, available []string) []string {
	out := make([]string, 0, len(wanted))

	for _, object := range wanted {
		if slices.Contains(available, object) {
			out = append(out, object)
		}
	}

	return out
}

// begin fixes what the session reports and starts the goroutines that own the
// connection from here on.
func (s *Session) begin(identity connectorapi.Identity, caps connectorapi.Set) {
	s.identity = identity
	s.caps = caps

	go s.read()
	go s.keepalive()
}

// Identity is what this machine is called.
func (s *Session) Identity() connectorapi.Identity { return s.identity }

// Capabilities is the resolved set and its epoch. The epoch never changes: a
// machine whose configuration changed restarts Klipper, which ends the session,
// and the next session probes again.
func (s *Session) Capabilities() (connectorapi.Set, uint64) { return s.caps.Clone(), 1 }

// Probe is what this machine answered about itself.
func (s *Session) Probe() connectorapi.Set { return s.probe.Clone() }

// State is the state tree's update channel.
func (s *Session) State() <-chan connectorapi.StatePatch { return s.patches }

// Dropped is how many state patches this session discarded.
func (s *Session) Dropped() uint64 { return s.dropped.Load() }

// Refused is how many frames the machine sent that this driver could not read.
func (s *Session) Refused() uint64 { return s.refused.Load() }

// Peer is the address of the machine's end of the connection.
func (s *Session) Peer() netip.Addr {
	addr, ok := s.conn.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return netip.Addr{}
	}

	peer, _ := netip.AddrFromSlice(addr.IP)

	return peer.Unmap()
}

// Close releases the session. It is safe to call more than once. Closing the
// connection ends the reader, and the reader closes the patch channel.
func (s *Session) Close() error {
	s.hangUp()

	return nil
}

// hangUp drops the connection.
func (s *Session) hangUp() {
	s.closing.Do(func() { _ = s.conn.Close() })
}

// Do sends one command to the machine and reports what it made of it. The
// arguments were validated against Capabilities before this was reached.
func (s *Session) Do(ctx context.Context, cmd connectorapi.Command) (connectorapi.Result, error) {
	if !s.caps.Has(cmd.Verb) {
		return connectorapi.Result{}, connectorapi.CapabilityNotSupported.
			New("the printer does not have that capability").
			With(fault.StringParam("verb", cmd.Verb.String()))
	}

	call, err := s.callFor(cmd)
	if err != nil {
		return connectorapi.Result{}, err
	}

	result, refusal, err := s.call(ctx, call.method, call.params, call.timeout)
	if err != nil {
		return connectorapi.Result{}, err
	}

	if refusal != nil {
		return refused(refusal.Message), nil
	}

	return connectorapi.Result{OK: true, Ignored: call.ignored, Raw: result}, nil
}

// refused is a machine's refusal of a command, with its own words.
func refused(reason string) connectorapi.Result {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return connectorapi.Result{OK: false, FaultCode: connectorapi.CommandRefusedWithoutReason.Code().String()}
	}

	return connectorapi.Result{OK: false, FaultCode: connectorapi.CommandRefused.Code().String(), MachineReason: reason}
}

// outgoing is one command as Moonraker reads it.
type outgoing struct {
	method  string
	params  any
	timeout time.Duration
	ignored []string
}

// callFor is the call one command becomes: a Moonraker method of its own, a
// status query, or a G-code script.
func (s *Session) callFor(cmd connectorapi.Command) (outgoing, error) {
	switch cmd.Verb {
	case VerbSendStateInfo:
		return outgoing{method: methodObjectsQuery, params: objectsParam(s.asked), timeout: s.settings.timeout}, nil
	case VerbPower:
		action := "on"
		if cmd.Args[argPower] == "false" {
			action = "off"
		}

		return outgoing{
			method: methodPowerPost, params: map[string]any{"device": s.power, "action": action},
			timeout: s.settings.gcodeTimeout,
		}, nil
	case VerbStartPrint:
		filename, err := s.files.relative(cmd.Args[argPath])
		if err != nil {
			return outgoing{}, err
		}

		return outgoing{
			method: methodPrintStart, params: map[string]any{"filename": filename}, timeout: s.settings.timeout,
		}, nil
	}

	if named, ok := methodFor(cmd.Verb); ok {
		return outgoing{method: named, timeout: s.settings.timeout}, nil
	}

	script, ignored, err := gcodeFor(cmd, s.reader.objectName)
	if err != nil {
		return outgoing{}, err
	}

	// Moonraker answers a script when Klipper has run it, so homing or a bed
	// mesh holds the call for as long as the machine takes.
	return outgoing{
		method: methodGcodeScript, params: map[string]any{"script": script},
		timeout: s.settings.gcodeTimeout, ignored: ignored,
	}, nil
}

// call sends one request and waits for the machine's answer. refusal is the
// machine's own error, which is an answer and not a failure of this connection.
func (s *Session) call(
	ctx context.Context, method string, params any, timeout time.Duration,
) (result json.RawMessage, refusal *rpcError, err error) {
	// A caller who has already given up must not move the machine.
	if err = ctx.Err(); err != nil {
		return nil, nil, fmt.Errorf("klipper: %s: %w", method, err)
	}

	if timeout <= 0 {
		timeout = s.settings.timeout
	}

	id := s.nextID.Add(1)
	replies := make(chan rpcFrame, 1)

	s.pending.Store(id, replies)
	defer s.pending.Delete(id)

	if err = s.write(rpcRequest{JSONRPC: jsonrpcVersion, Method: method, Params: params, ID: id}); err != nil {
		return nil, nil, err
	}

	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	select {
	case frame := <-replies:
		return frame.Result, frame.Error, nil
	case <-s.done:
		return nil, nil, fmt.Errorf("%w: %s was never answered", ErrSessionEnded, method)
	case <-callCtx.Done():
		return nil, nil, fmt.Errorf("klipper: %s: %w", method, callCtx.Err())
	}
}

// callInto sends one request, and decodes the answer into into. A refusal is
// returned as rpcError.
func (s *Session) callInto(ctx context.Context, method string, params, into any) (*rpcError, error) {
	result, refusal, err := s.call(ctx, method, params, s.settings.timeout)
	if err != nil || refusal != nil {
		return refusal, err
	}

	if into == nil {
		return nil, nil
	}

	return nil, decodeResult(method, result, into)
}

// callSync sends one request and reads the connection itself until the answer
// arrives. It is the handshake's call, used before any goroutine owns reads.
func (s *Session) callSync(ctx context.Context, method string, params any, into any) error {
	id := s.nextID.Add(1)

	if err := s.write(rpcRequest{JSONRPC: jsonrpcVersion, Method: method, Params: params, ID: id}); err != nil {
		return err
	}

	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > s.settings.timeout {
		deadline = time.Now().Add(s.settings.timeout)
	}

	if err := s.conn.SetReadDeadline(deadline); err != nil {
		return fmt.Errorf("%w: arm the read deadline: %w", ErrUnreachable, err)
	}

	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("klipper: %s: %w", method, err)
		}

		var frame rpcFrame
		if err := s.conn.ReadJSON(&frame); err != nil {
			return fmt.Errorf("%w: %s: %w", ErrUnreachable, method, err)
		}

		if frame.Method == notifyKlippyDisconnected {
			return fmt.Errorf("%w: Klipper disconnected during the handshake", ErrKlippyNotReady)
		}

		if frame.ID == nil || *frame.ID != id {
			continue
		}

		if frame.Error != nil {
			return fmt.Errorf("%w: %s: %s", ErrRefused, method, frame.Error.Message)
		}

		if into == nil {
			return nil
		}

		return decodeResult(method, frame.Result, into)
	}
}

// write puts one request on the wire.
func (s *Session) write(request rpcRequest) error {
	s.writing.Lock()
	defer s.writing.Unlock()

	if err := s.conn.SetWriteDeadline(time.Now().Add(s.settings.timeout)); err != nil {
		return fmt.Errorf("%w: arm the write deadline: %w", ErrUnreachable, err)
	}

	if err := s.conn.WriteJSON(request); err != nil {
		return fmt.Errorf("%w: send %s: %w", ErrUnreachable, request.Method, err)
	}

	return nil
}

// read owns every read once the session has begun. It is the only writer to the
// patch channel, which is what lets it be the only closer.
func (s *Session) read() {
	defer close(s.patches)
	defer close(s.done)
	defer s.hangUp()

	s.conn.SetPongHandler(func(string) error { return s.rearm() })

	if err := s.rearm(); err != nil {
		return
	}

	for _, patch := range s.first {
		s.report(patch)
	}

	s.first = nil

	for {
		_, data, err := s.conn.ReadMessage()
		if err != nil {
			s.log.Debug("the machine's connection ended", "error", err.Error())

			return
		}

		if err = s.rearm(); err != nil {
			return
		}

		s.dispatch(data)
	}
}

// rearm pushes the read deadline out to the full silence this driver tolerates.
func (s *Session) rearm() error {
	if err := s.conn.SetReadDeadline(time.Now().Add(s.settings.live.readDeadline)); err != nil {
		return fmt.Errorf("%w: arm the read deadline: %w", ErrUnreachable, err)
	}

	return nil
}

// dispatch acts on one frame: an answer to a call, a notification, or
// something this driver has no reading of.
func (s *Session) dispatch(data []byte) {
	var frame rpcFrame
	if err := json.Unmarshal(data, &frame); err != nil {
		s.refused.Add(1)

		return
	}

	if frame.ID != nil {
		s.answer(*frame.ID, frame)

		return
	}

	switch frame.Method {
	case notifyKlippyDisconnected:
		// Moonraker drops every subscription. Ending the session makes the
		// host dial again, and the next handshake subscribes afresh.
		s.log.Info("Klipper disconnected from Moonraker; reconnecting")
		s.hangUp()
	case notifyKlippyShutdown:
		s.update(json.RawMessage(`[{"webhooks":{"state":"shutdown"}}]`))
	case notifyStatusUpdate:
		s.update(frame.Params)
	case notifyFilelistChanged:
		s.files.changed(frame.Params)
	case notifyPowerChanged:
		s.powerChanged(frame.Params)
	}
}

// answer hands one reply to the call that is waiting for it.
func (s *Session) answer(id uint64, frame rpcFrame) {
	waiting, found := s.pending.Load(id)
	if !found {
		return
	}

	replies, ok := waiting.(chan rpcFrame)
	if !ok {
		return
	}

	select {
	case replies <- frame:
	default:
	}
}

// update turns one subscription notification into patches.
func (s *Session) update(params json.RawMessage) {
	var carried []json.RawMessage
	if json.Unmarshal(params, &carried) != nil || len(carried) == 0 {
		s.refused.Add(1)

		return
	}

	var status map[string]json.RawMessage
	if json.Unmarshal(carried[0], &status) != nil {
		s.refused.Add(1)

		return
	}

	for _, patch := range s.reader.patchesFrom(status, time.Now()) {
		s.report(patch)
	}
}

// report queues one patch for the host without waiting. When the host falls
// behind, the oldest patch goes.
func (s *Session) report(patch connectorapi.StatePatch) {
	select {
	case s.patches <- patch:
		return
	case <-s.done:
		return
	default:
	}

	select {
	case <-s.patches:
		s.dropped.Add(1)
	default:
	}

	select {
	case s.patches <- patch:
	case <-s.done:
	default:
		s.dropped.Add(1)
	}
}

// keepalive pings the machine until the connection ends, so a machine that lost
// power does not hold a session that looks connected.
func (s *Session) keepalive() {
	ticker := time.NewTicker(s.settings.live.pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			if err := s.ping(); err != nil {
				s.log.Debug("the machine would not take a ping, so its connection is gone", "error", err.Error())
				s.hangUp()

				return
			}
		}
	}
}

// ping writes one control frame.
func (s *Session) ping() error {
	s.writing.Lock()
	defer s.writing.Unlock()

	if err := s.conn.WriteControl(
		websocket.PingMessage, nil, time.Now().Add(s.settings.live.pingTimeout),
	); err != nil {
		return fmt.Errorf("%w: send a ping: %w", ErrUnreachable, err)
	}

	return nil
}

// powerPath is where the power device's state lands: true while it is on.
const powerPath = "/addon_power"

// probePower picks the power device this session switches, and answers its
// state as patches. A Moonraker with no [power] section has none, which is
// not an error.
func (s *Session) probePower(ctx context.Context) ([]connectorapi.StatePatch, error) {
	var listed powerList

	err := s.callSync(ctx, methodPowerDevices, nil, &listed)
	if errors.Is(err, ErrRefused) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	chosen, ok := pickPower(listed.Devices, s.powerWanted)
	if !ok {
		if s.powerWanted != "" {
			s.log.Warn("the endpoint names a power device Moonraker does not have", "power_device", s.powerWanted)
		}

		return nil, nil
	}

	s.power = chosen.Device

	return powerPatch(chosen.Status), nil
}

// pickPower is the named device, or the one called "printer", or the only one.
func pickPower(devices []powerDevice, wanted string) (powerDevice, bool) {
	for _, name := range []string{wanted, "printer"} {
		if name == "" {
			continue
		}

		for _, device := range devices {
			if device.Device == name {
				return device, true
			}
		}

		if name == wanted {
			return powerDevice{}, false
		}
	}

	if len(devices) == 1 {
		return devices[0], true
	}

	return powerDevice{}, false
}

// powerPatch is a device status as the boolean the tree carries. A status that
// is neither on nor off, such as "error", removes the reading.
func powerPatch(status string) []connectorapi.StatePatch {
	switch status {
	case "on":
		return []connectorapi.StatePatch{{Path: powerPath, Value: json.RawMessage("true")}}
	case "off":
		return []connectorapi.StatePatch{{Path: powerPath, Value: json.RawMessage("false")}}
	default:
		return []connectorapi.StatePatch{{Path: powerPath, Value: json.RawMessage("null")}}
	}
}

// powerChanged reads one notify_power_changed.
func (s *Session) powerChanged(params json.RawMessage) {
	var changed []powerDevice
	if json.Unmarshal(params, &changed) != nil {
		s.refused.Add(1)

		return
	}

	for _, device := range changed {
		if device.Device == s.power && s.power != "" {
			for _, patch := range powerPatch(device.Status) {
				s.report(patch)
			}
		}
	}
}
