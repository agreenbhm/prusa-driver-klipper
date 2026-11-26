// Package klipper is a Prusa Connect driver for Klipper printers, reached
// through Moonraker, the API server that runs beside Klipper on a Voron, a
// Rat Rig, a converted Ender and most other Klipper machines.
//
// The driver is outbound: the server dials the address in the endpoint list.
// One websocket carries the identity exchange, the status subscription, every
// command and every file operation, as JSON-RPC 2.0. Uploads and thumbnails go
// over plain HTTP to the same address.
//
// What the machine lists under printer.objects.list narrows what is offered. A
// machine with no pause_resume section is not offered pause, and one with no
// bed_mesh section is not offered bed leveling. The heater bounds are the
// machine's own.
//
// Klipper reports no vendor, model or serial number. The endpoint options give
// them:
//
//	vendor    who made the machine. Empty reports "Klipper"
//	model     the model name. Empty reports "Klipper"
//	serial    the serial number. Empty reports none
//	unit_uid  the stable identity. Empty derives one from the address
//	api_key   the Moonraker API key, for a Moonraker that requires one
//
// Set unit_uid where an address can change. The printer id is derived from it.
package klipper

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/prusa3d/prusa-connect/sdk/connectorapi"
	"github.com/prusa3d/prusa-connect/sdk/extension"
)

// Type is the value of a connector's `type` in configuration that selects this
// driver.
const Type connectorapi.TypeName = "klipper"

// Version is this driver's own release.
const Version = "0.1.0"

// defaultPort is where Moonraker listens by default.
const defaultPort = "7125"

// websocketPath is where Moonraker serves its JSON-RPC API.
const websocketPath = "/websocket"

// apiKeyHeader is the header Moonraker reads an API key from, on the websocket
// upgrade and on every HTTP request.
const apiKeyHeader = "X-Api-Key"

const (
	// defaultTimeout bounds one call to the machine.
	defaultTimeout = 10 * time.Second

	// defaultStorage is the name the gcodes folder is exposed under. Both
	// PrusaSlicer connectors send files to "usb", so a Klipper machine takes
	// them with no change on the slicer side.
	defaultStorage = "usb"

	// patchBacklog bounds how many state patches may wait for the host.
	patchBacklog = 256

	// maxFrameBytes bounds one websocket frame. The configuration dump and a
	// large folder listing are the largest things Moonraker sends.
	maxFrameBytes = 1 << 23
)

// The errors this driver reports. Callers inspect them with errors.Is.
var (
	// ErrInvalidConfig reports a config block or an address this driver cannot
	// act on.
	ErrInvalidConfig = errors.New("klipper: invalid configuration")
	// ErrUnreachable reports a machine that did not answer.
	ErrUnreachable = errors.New("klipper: the machine did not answer")
	// ErrRefused reports a machine that answered a call with a JSON-RPC error
	// during the identity exchange, so no session was built.
	ErrRefused = errors.New("klipper: the machine refused a call")
	// ErrKlippyNotReady reports a Moonraker whose Klipper is not running, or
	// is starting. The host dials again later.
	ErrKlippyNotReady = errors.New("klipper: Klipper is not ready")
)

//nolint:gochecknoinits // registration has to happen before main; the host reaches it through a blank import.
func init() { connectorapi.Register(Type, doc(), New, dialect()) }

// dialect describes the machines of this driver to a page that adds one.
func dialect() connectorapi.Dialect {
	return connectorapi.Dialect{
		Brands: []string{"Klipper", "Voron", "Rat Rig", "Creality", "Sovol", "Elegoo", "Qidi"},
		Needs:  connectorapi.NeedsAddress,
		Models: []connectorapi.DeviceModel{{
			Model:       defaultModel,
			DisplayName: "Klipper printer",
			Vendor:      defaultVendor,
			Artwork:     connectorapi.Artwork{MediaType: connectorapi.ArtworkSVG, Bytes: artwork},
		}},
		States: states(),
	}
}

// artwork is the drawing of a Klipper machine.
//
//go:embed artwork.svg
var artwork []byte

// doc is what the published extension reference says about this driver.
func doc() extension.Doc {
	return extension.Doc{
		Summary: "A Klipper printer reached through Moonraker: telemetry, files, uploads, prints and controls.",
		Description: "The host dials the Moonraker address the endpoint list holds, asks what the machine " +
			"is, and subscribes to the Klipper objects it reads telemetry from. The gcodes folder is " +
			"exposed as one storage, so files can be listed, uploaded, deleted and printed. What the " +
			"machine reports under printer.objects.list narrows what is offered: a machine with no " +
			"pause_resume section is not offered pause, and one with no LOAD_FILAMENT macro is not " +
			"offered load. The endpoint options vendor, model, serial, unit_uid and api_key say what " +
			"Klipper itself does not report. Commands are not queued by this driver: Moonraker runs " +
			"them in the order they arrive.",
		Version:   Version,
		Contract:  connectorapi.Contract,
		Stability: extension.StabilityExperimental,
		Options: []extension.Option{
			{
				Key:     "timeout",
				Type:    extension.OptionDuration,
				Default: "10s",
				Summary: "How long one call to the machine may take before it is abandoned.",
			},
			{
				Key:     "gcode_timeout",
				Type:    extension.OptionDuration,
				Default: "5m",
				Summary: "How long a G-code command may run, such as homing or bed leveling.",
			},
			{
				Key:     "storage_name",
				Type:    extension.OptionString,
				Default: defaultStorage,
				Summary: "The storage name the gcodes folder is shown under. Both PrusaSlicer connectors send to usb.",
			},
			{
				Key:     "api_key",
				Type:    extension.OptionSecret,
				Summary: "The Moonraker API key for every machine of this connector. An endpoint option api_key overrides it.",
			},
		},
	}
}

// Config is what an operator writes under a connector's `config` block.
type Config struct {
	// Timeout bounds one call to the machine.
	Timeout string `json:"timeout,omitempty"`
	// GcodeTimeout bounds one G-code command, which can take minutes.
	GcodeTimeout string `json:"gcode_timeout,omitempty"`
	// StorageName is the storage the gcodes folder is exposed as.
	StorageName string `json:"storage_name,omitempty"`
	// APIKey is the Moonraker API key, for a Moonraker that requires one.
	APIKey string `json:"api_key,omitempty"` //nolint:gosec // a config field, not a hard-coded secret.
}

// settings is the parsed configuration a driver hands to each session.
type settings struct {
	timeout      time.Duration
	gcodeTimeout time.Duration
	storage      string
	apiKey       string
	live         liveness
}

// Driver is the connectorapi.Outbound this package registers. It has no
// per-machine state. That is on the Session built for each machine.
type Driver struct {
	settings settings
	dialer   *websocket.Dialer
	http     *http.Client
	log      *slog.Logger
}

var (
	_ connectorapi.Outbound    = (*Driver)(nil)
	_ connectorapi.TakesLogger = (*Driver)(nil)
)

// New builds a Driver from its spec. It is the connectorapi.Factory for Type.
func New(spec connectorapi.Spec) (connectorapi.Driver, error) {
	var cfg Config
	if err := spec.DecodeConfig(&cfg); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}

	parsed, err := cfg.parse()
	if err != nil {
		return nil, err
	}

	driver := newDriver(parsed)
	if spec.Logger != nil {
		driver.log = spec.Logger
	}

	return driver, nil
}

// parse checks a config block and fills in the defaults.
func (c Config) parse() (settings, error) {
	out := settings{timeout: defaultTimeout, gcodeTimeout: defaultGcodeTimeout, storage: defaultStorage}

	var err error

	if out.timeout, err = positiveDuration("timeout", c.Timeout, defaultTimeout); err != nil {
		return settings{}, err
	}

	if out.gcodeTimeout, err = positiveDuration("gcode_timeout", c.GcodeTimeout, defaultGcodeTimeout); err != nil {
		return settings{}, err
	}

	if name := strings.TrimSpace(c.StorageName); name != "" {
		if strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
			return settings{}, fmt.Errorf("%w: storage_name %q must be one path segment", ErrInvalidConfig, name)
		}

		out.storage = name
	}

	out.apiKey = strings.TrimSpace(c.APIKey)

	return out, nil
}

// defaultGcodeTimeout bounds a G-code command. Homing, a bed mesh or a heat
// soak macro can take minutes, and Moonraker answers only when it is done.
const defaultGcodeTimeout = 5 * time.Minute

// positiveDuration reads one duration option.
func positiveDuration(key, raw string, fallback time.Duration) (time.Duration, error) {
	if raw == "" {
		return fallback, nil
	}

	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%w: %s: %w", ErrInvalidConfig, key, err)
	}

	if parsed <= 0 {
		return 0, fmt.Errorf("%w: %s must be positive, and it is %s", ErrInvalidConfig, key, parsed)
	}

	return parsed, nil
}

// NewDriver returns a driver with the given call timeout and the defaults for
// everything else. Zero or less means the default timeout.
func NewDriver(timeout time.Duration) *Driver {
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	return newDriver(settings{timeout: timeout, gcodeTimeout: defaultGcodeTimeout, storage: defaultStorage})
}

// newDriver builds a driver from parsed settings.
func newDriver(s settings) *Driver {
	s.live = s.live.orShipping()

	return &Driver{
		settings: s,
		dialer: &websocket.Dialer{
			HandshakeTimeout: s.timeout,
			ReadBufferSize:   readBufferBytes,
			WriteBufferSize:  writeBufferBytes,
			Proxy:            http.ProxyFromEnvironment,
		},
		// No client timeout: an upload is bounded by the context it is sent
		// under, and a large file on a slow link takes as long as it takes.
		http: &http.Client{},
		log:  slog.New(slog.DiscardHandler),
	}
}

// UseLogger takes the logger this process writes through.
func (d *Driver) UseLogger(log *slog.Logger) {
	if log != nil {
		d.log = log
	}
}

// The websocket buffers of one connection.
const (
	readBufferBytes  = 16 << 10
	writeBufferBytes = 4 << 10
)

// Connect dials the machine, runs the identity exchange and the live probe,
// and returns a session. profile is what the host offers. The driver and the
// machine can only narrow it, so each merge is an intersection.
func (d *Driver) Connect(
	ctx context.Context, endpoint connectorapi.Endpoint, profile connectorapi.Set,
) (connectorapi.Session, error) {
	addr, err := parseAddress(endpoint.Address)
	if err != nil {
		return nil, err
	}

	key := d.settings.apiKey
	if option := strings.TrimSpace(endpoint.Options[optionAPIKey]); option != "" {
		key = option
	}

	header := http.Header{}
	if key != "" {
		header.Set(apiKeyHeader, key)
	}

	conn, response, err := d.dialer.DialContext(ctx, addr.websocket, header)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}

	if err != nil {
		if response != nil && (response.StatusCode == http.StatusUnauthorized ||
			response.StatusCode == http.StatusForbidden) {
			return nil, fmt.Errorf("%w: %s answered %d: set the api_key option to the Moonraker API key",
				ErrRefused, addr.websocket, response.StatusCode)
		}

		return nil, fmt.Errorf("%w: %s: %w", ErrUnreachable, addr.websocket, err)
	}

	conn.SetReadLimit(maxFrameBytes)

	session := newSession(conn, d.settings, d.log)
	session.files = newFiles(session, addr.http, key, d.http, d.settings.storage)

	resolved, err := session.handshake(ctx, d.Baseline().Intersect(profile))
	if err != nil {
		session.hangUp()

		return nil, err
	}

	session.begin(identityOf(endpoint, session.reported), resolved)

	return session, nil
}

// identityOf builds the identity from the endpoint options. A missing option
// falls back to what the machine or the address gives.
func identityOf(endpoint connectorapi.Endpoint, reported printerInfo) connectorapi.Identity {
	unit := endpoint.Options[optionUnitUID]
	if unit == "" {
		unit = unitPrefix + strings.ToLower(strings.TrimSpace(endpoint.Address))
	}

	model := endpoint.Options[optionModel]
	if model == "" {
		model = defaultModel
	}

	vendor := endpoint.Options[optionVendor]
	if vendor == "" {
		vendor = defaultVendor
	}

	return connectorapi.Identity{
		Vendor:   vendor,
		Model:    model,
		Firmware: connectorapi.NewFirmwareLabel(reported.SoftwareVersion),
		Serial:   endpoint.Options[optionSerial],
		UnitUID:  unit,
	}
}

// The endpoint options this driver reads, and what it reports when one is
// absent.
const (
	optionVendor  = "vendor"
	optionModel   = "model"
	optionSerial  = "serial"
	optionUnitUID = "unit_uid"
	optionAPIKey  = "api_key" //nolint:gosec // an option name, not a secret.

	defaultVendor = "Klipper"
	defaultModel  = "Klipper"

	// unitPrefix keeps a derived identity from colliding with one an operator
	// wrote.
	unitPrefix = "klipper:"
)

// address is one machine's two URLs: the websocket and the HTTP base that
// uploads and thumbnails go to.
type address struct {
	websocket string
	http      string
}

// parseAddress turns a bare host, a host and port, or a URL into the two URLs
// Moonraker serves. A URL keeps its path, for a machine behind a reverse proxy
// such as the one Mainsail and Fluidd install: http://voron.local/ reaches
// Moonraker through nginx on port 80.
func parseAddress(raw string) (address, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return address{}, fmt.Errorf("%w: the endpoint names no address", ErrInvalidConfig)
	}

	if !strings.Contains(trimmed, "://") {
		host := withPort(trimmed)

		return address{websocket: "ws://" + host + websocketPath, http: "http://" + host}, nil
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return address{}, fmt.Errorf("%w: %q is not an address: %w", ErrInvalidConfig, raw, err)
	}

	if parsed.Host == "" {
		return address{}, fmt.Errorf("%w: %q names no host", ErrInvalidConfig, raw)
	}

	if parsed.User != nil {
		return address{}, fmt.Errorf("%w: %q carries a user name; use the api_key option instead", ErrInvalidConfig, raw)
	}

	var wsScheme, httpScheme string

	switch parsed.Scheme {
	case "ws", "http":
		wsScheme, httpScheme = "ws", "http"
	case "wss", "https":
		wsScheme, httpScheme = "wss", "https"
	default:
		return address{}, fmt.Errorf("%w: %q names the scheme %q, and this driver speaks http, https, ws and wss",
			ErrInvalidConfig, raw, parsed.Scheme)
	}

	base := strings.TrimSuffix(parsed.Path, "/")
	base = strings.TrimSuffix(base, websocketPath)

	ws := url.URL{Scheme: wsScheme, Host: parsed.Host, Path: base + websocketPath}
	web := url.URL{Scheme: httpScheme, Host: parsed.Host, Path: base}

	return address{websocket: ws.String(), http: strings.TrimSuffix(web.String(), "/")}, nil
}

// withPort adds Moonraker's default port to a host that names none.
func withPort(host string) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}

	return net.JoinHostPort(strings.Trim(host, "[]"), defaultPort)
}

// decodeResult reads one JSON-RPC result. The error names the call.
func decodeResult(method string, result json.RawMessage, into any) error {
	if err := json.Unmarshal(result, into); err != nil {
		return fmt.Errorf("%w: the answer to %s is not readable: %w", ErrUnreachable, method, err)
	}

	return nil
}
