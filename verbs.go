package klipper

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/prusa3d/prusa-connect/sdk/connectorapi"
	"github.com/prusa3d/prusa-connect/sdk/fault"
)

// The capabilities this driver can carry out. Every key is one the published
// capability vocabulary already names, so the web app and every extension
// drive a Klipper machine with the same words as a Prusa.
const (
	VerbSendStateInfo = connectorapi.Capability("core.command.send_state_info")

	VerbStartPrint  = connectorapi.Capability("core.command.start_print")
	VerbPausePrint  = connectorapi.Capability("core.command.pause_print")
	VerbResumePrint = connectorapi.Capability("core.command.resume_print")
	VerbStopPrint   = connectorapi.Capability("core.command.stop_print")
	// VerbSetIdle clears a finished or cancelled print, so the machine reads
	// idle again. Klipper otherwise keeps "complete" until the next print.
	VerbSetIdle = connectorapi.Capability("core.command.set_idle")

	VerbNozzleTarget  = connectorapi.Capability("core.command.set_nozzle_temperature")
	VerbBedTarget     = connectorapi.Capability("core.command.set_bed_temperature")
	VerbChamberTarget = connectorapi.Capability("core.command.set_chamber_target_temp")
	VerbSetSpeed      = connectorapi.Capability("core.command.set_speed")
	VerbSetFlow       = connectorapi.Capability("core.command.set_flow")

	VerbHome            = connectorapi.Capability("core.command.home")
	VerbMove            = connectorapi.Capability("core.command.move")
	VerbMoveZ           = connectorapi.Capability("core.command.move_z")
	VerbMoveE           = connectorapi.Capability("core.command.move_e")
	VerbDisableSteppers = connectorapi.Capability("core.command.disable_steppers")

	VerbSendGcode      = connectorapi.Capability("core.command.send_gcode")
	VerbCancelObject   = connectorapi.Capability("core.command.cancel_object")
	VerbMeshLeveling   = connectorapi.Capability("core.command.mesh_bed_leveling")
	VerbLoadFilament   = connectorapi.Capability("core.command.load_filament")
	VerbUnloadFilament = connectorapi.Capability("core.command.unload_filament")
	// VerbResetPrinter is "Restart printer", a software restart: Klipper's
	// RESTART, which reloads the configuration on the host.
	VerbResetPrinter = connectorapi.Capability("core.command.reset_printer")
	// VerbReset is "Reset printer", which restarts the control board:
	// FIRMWARE_RESTART, which also resets the MCU and clears a shutdown.
	VerbReset = connectorapi.Capability("core.command.reset")
	// VerbPower switches the Moonraker power device that feeds the printer,
	// such as a smart plug. It works while Klipper is shut down, which is
	// when it is needed: switch the board on, then restart the firmware.
	VerbPower = connectorapi.Capability("core.command.set_addon_power")

	// The file verbs. The host checks them before it calls Filesystem,
	// FileManager or Transfers on the session; they never reach Do.
	VerbSendInfo     = connectorapi.VerbSendInfo
	VerbSendFileInfo = connectorapi.VerbSendFileInfo
	VerbCreateFolder = connectorapi.VerbCreateFolder
	VerbDeleteFile   = connectorapi.VerbDeleteFile
	VerbDeleteFolder = connectorapi.VerbDeleteFolder
	FeatureUpload    = connectorapi.VerbFileUpload
)

// The argument names, as the vocabulary spells them.
const (
	argPath     = "path"
	argNozzle   = "nozzle_temperature"
	argTool     = "tool"
	argBed      = "bed_temperature"
	argChamber  = "chamber.target_temp"
	argSpeed    = "speed"
	argFlow     = "flow"
	argAxis     = "axis"
	argX        = "x"
	argY        = "y"
	argDistance = "distance"
	argLength   = "length"
	argFeedrate = "feedrate"
	argGcode    = "gcode"
	argObjectID = "id"
	argFilament = "filament"
	argPower    = "addon_power"
)

// The argument keys.
//
//nolint:gochecknoglobals // derived constants; Go has no constant function calls.
var (
	ArgStartPrintPath   = connectorapi.ArgumentKey(VerbStartPrint, argPath)
	ArgNozzleTarget     = connectorapi.ArgumentKey(VerbNozzleTarget, argNozzle)
	ArgNozzleTool       = connectorapi.ArgumentKey(VerbNozzleTarget, argTool)
	ArgBedTarget        = connectorapi.ArgumentKey(VerbBedTarget, argBed)
	ArgChamberTarget    = connectorapi.ArgumentKey(VerbChamberTarget, argChamber)
	ArgSpeed            = connectorapi.ArgumentKey(VerbSetSpeed, argSpeed)
	ArgFlow             = connectorapi.ArgumentKey(VerbSetFlow, argFlow)
	ArgHomeAxis         = connectorapi.ArgumentKey(VerbHome, argAxis)
	ArgMoveX            = connectorapi.ArgumentKey(VerbMove, argX)
	ArgMoveY            = connectorapi.ArgumentKey(VerbMove, argY)
	ArgMoveFeedrate     = connectorapi.ArgumentKey(VerbMove, argFeedrate)
	ArgMoveZDistance    = connectorapi.ArgumentKey(VerbMoveZ, argDistance)
	ArgMoveZFeedrate    = connectorapi.ArgumentKey(VerbMoveZ, argFeedrate)
	ArgMoveELength      = connectorapi.ArgumentKey(VerbMoveE, argLength)
	ArgMoveEFeedrate    = connectorapi.ArgumentKey(VerbMoveE, argFeedrate)
	ArgGcode            = connectorapi.ArgumentKey(VerbSendGcode, argGcode)
	ArgCancelObjectID   = connectorapi.ArgumentKey(VerbCancelObject, argObjectID)
	ArgLoadFilament     = connectorapi.ArgumentKey(VerbLoadFilament, argFilament)
	ArgPower            = connectorapi.ArgumentKey(VerbPower, argPower)
	ArgFileInfoPath     = connectorapi.ArgumentKey(VerbSendFileInfo, argPath)
	ArgCreateFolderPath = connectorapi.ArgumentKey(VerbCreateFolder, argPath)
	ArgDeleteFilePath   = connectorapi.ArgumentKey(VerbDeleteFile, argPath)
	ArgDeleteFolderPath = connectorapi.ArgumentKey(VerbDeleteFolder, argPath)
)

// The highest values this driver's code will pass on. They are ceilings and
// not a machine's answer: the machine's own configuration narrows the heater
// bounds the moment it is read.
const (
	nozzleCeiling  = 500
	bedCeiling     = 200
	chamberCeiling = 100

	// The speed and flow overrides, in percent. The vocabulary's own range.
	minFactor = 20
	maxFactor = 999

	// A feedrate in mm/min. Zero would ask for a move that never ends.
	minFeedrate = 1
	maxFeedrate = 60000

	// The longest jog this driver passes on, in millimetres either way.
	maxJog = 1000

	// The longest G-code script one command may carry, in bytes.
	maxGcodeBytes = 8192

	// The longest path one command may carry, in bytes.
	maxPathBytes = 1024

	// The longest filament name, in bytes.
	maxFilamentBytes = 32
)

// The Klipper heater names the temperature verbs address.
const (
	heaterExtruder = "extruder"
	heaterBed      = "heater_bed"
	heaterChamber  = "chamber"
)

// flag is a verb or feature with no value.
func flag() connectorapi.Value { return connectorapi.Value{Flag: true} }

// integer is a whole-number argument between low and high.
func integer(low, high float64, required bool, unit connectorapi.Unit) connectorapi.Value {
	return connectorapi.Value{
		Flag: true, Kind: connectorapi.KindInteger, Required: required,
		Min: low, HasMin: true, Limit: high, HasLimit: true, Unit: unit,
	}
}

// number is a fractional argument within ±limit.
func number(limit float64, required bool, unit connectorapi.Unit) connectorapi.Value {
	return connectorapi.Value{
		Flag: true, Kind: connectorapi.KindNumber, Required: required,
		Limit: limit, HasLimit: true, Unit: unit,
	}
}

// text is a string argument at most limit bytes long.
func text(limit float64, required bool) connectorapi.Value {
	return connectorapi.Value{
		Flag: true, Kind: connectorapi.KindString, Required: required, Limit: limit, HasLimit: true,
	}
}

// Baseline is what this driver can do, for any Klipper machine. It returns a
// fresh set each time. The probe in Connect narrows it to what this one
// machine has.
func (d *Driver) Baseline() connectorapi.Set {
	return connectorapi.Set{
		VerbSendStateInfo: flag(),

		VerbSendInfo:        flag(),
		VerbSendFileInfo:    flag(),
		ArgFileInfoPath:     text(maxPathBytes, true),
		VerbCreateFolder:    flag(),
		ArgCreateFolderPath: text(maxPathBytes, true),
		VerbDeleteFile:      flag(),
		ArgDeleteFilePath:   text(maxPathBytes, true),
		VerbDeleteFolder:    flag(),
		ArgDeleteFolderPath: text(maxPathBytes, true),
		FeatureUpload:       flag(),

		VerbStartPrint:    flag(),
		ArgStartPrintPath: text(maxPathBytes, true),
		VerbPausePrint:    flag(),
		VerbResumePrint:   flag(),
		VerbStopPrint:     flag(),
		VerbSetIdle:       flag(),
		VerbCancelObject:  flag(),
		ArgCancelObjectID: integer(0, math.MaxInt32, true, ""),

		VerbNozzleTarget:  flag(),
		ArgNozzleTarget:   integer(0, nozzleCeiling, true, connectorapi.UnitCelsius),
		ArgNozzleTool:     connectorapi.Value{Flag: true, Kind: connectorapi.KindInteger, Min: 0, HasMin: true},
		VerbBedTarget:     flag(),
		ArgBedTarget:      integer(0, bedCeiling, true, connectorapi.UnitCelsius),
		VerbChamberTarget: flag(),
		ArgChamberTarget:  integer(0, chamberCeiling, true, connectorapi.UnitCelsius),
		VerbSetSpeed:      flag(),
		ArgSpeed:          integer(minFactor, maxFactor, true, connectorapi.UnitPercent),
		VerbSetFlow:       flag(),
		ArgFlow:           integer(minFactor, maxFactor, true, connectorapi.UnitPercent),

		VerbHome:            flag(),
		ArgHomeAxis:         text(8, false),
		VerbMove:            flag(),
		ArgMoveX:            number(maxJog, false, connectorapi.UnitMillimeter),
		ArgMoveY:            number(maxJog, false, connectorapi.UnitMillimeter),
		ArgMoveFeedrate:     integer(minFeedrate, maxFeedrate, true, connectorapi.UnitMillimeterPerMinute),
		VerbMoveZ:           flag(),
		ArgMoveZDistance:    number(maxJog, true, connectorapi.UnitMillimeter),
		ArgMoveZFeedrate:    integer(minFeedrate, maxFeedrate, true, connectorapi.UnitMillimeterPerMinute),
		VerbMoveE:           flag(),
		ArgMoveELength:      number(maxJog, true, connectorapi.UnitMillimeter),
		ArgMoveEFeedrate:    integer(minFeedrate, maxFeedrate, true, connectorapi.UnitMillimeterPerMinute),
		VerbDisableSteppers: flag(),

		VerbSendGcode:      flag(),
		ArgGcode:           text(maxGcodeBytes, true),
		VerbMeshLeveling:   flag(),
		VerbLoadFilament:   flag(),
		ArgLoadFilament:    text(maxFilamentBytes, false),
		VerbUnloadFilament: flag(),
		VerbResetPrinter:   flag(),
		VerbReset:          flag(),
		VerbPower:          flag(),
		// Optional, and on when left out, so the button alone switches the
		// printer on.
		ArgPower: connectorapi.Value{Flag: true, Kind: connectorapi.KindBoolean, Default: "true"},
	}
}

// machine is what the handshake learned about one machine, which is what the
// probe reads.
type machine struct {
	objects   []string
	heaters   map[string]heaterConfig
	extruders int
	chamber   string
	power     string
}

// probe is what this machine says it can do, as a tier that narrows the two
// above it. It carries a key only when the machine reports the Klipper object
// that carries out the verb. A key it carries is copied from the resolved set,
// so that the intersection changes nothing but the heater bounds this function
// narrows on purpose.
func probe(m machine, resolved connectorapi.Set) connectorapi.Set {
	claimed := connectorapi.Set{}
	has := func(object string) bool { return slices.Contains(m.objects, object) }

	// Every Klipper machine answers a status query, runs a G-code script,
	// restarts its firmware and serves its gcodes folder.
	carry(claimed, resolved,
		VerbSendStateInfo, VerbSendGcode, ArgGcode, VerbResetPrinter, VerbReset,
		VerbSendInfo, VerbSendFileInfo, ArgFileInfoPath,
		VerbCreateFolder, ArgCreateFolderPath, VerbDeleteFile, ArgDeleteFilePath,
		VerbDeleteFolder, ArgDeleteFolderPath, FeatureUpload)

	// Moonraker prints a file through virtual_sdcard, so a machine without the
	// section cannot start one.
	if has(objectVirtualSD) {
		carry(claimed, resolved, VerbStartPrint, ArgStartPrintPath, VerbSetIdle)
	}

	// PAUSE, RESUME and CANCEL_PRINT are registered by pause_resume.
	if has(objectPauseResume) {
		carry(claimed, resolved, VerbPausePrint, VerbResumePrint, VerbStopPrint)
	}

	if has(objectExcludeObject) {
		carry(claimed, resolved, VerbCancelObject, ArgCancelObjectID)
	}

	if has(objectToolhead) {
		carry(claimed, resolved,
			VerbHome, ArgHomeAxis, VerbMove, ArgMoveX, ArgMoveY, ArgMoveFeedrate,
			VerbMoveZ, ArgMoveZDistance, ArgMoveZFeedrate, VerbDisableSteppers)
	}

	if has(objectGcodeMove) {
		carry(claimed, resolved, VerbSetSpeed, ArgSpeed, VerbSetFlow, ArgFlow)
	}

	if has(objectExtruder) {
		carryHeater(claimed, resolved, VerbNozzleTarget, ArgNozzleTarget, m.heaters[heaterExtruder])
		carryTool(claimed, resolved, m.extruders)
		carry(claimed, resolved, VerbMoveE, ArgMoveELength, ArgMoveEFeedrate)
	}

	if has(objectHeaterBed) {
		carryHeater(claimed, resolved, VerbBedTarget, ArgBedTarget, m.heaters[heaterBed])
	}

	if m.chamber == objectChamberHeater {
		carryHeater(claimed, resolved, VerbChamberTarget, ArgChamberTarget, m.heaters[objectChamberHeater])
	}

	if has(objectBedMesh) {
		carry(claimed, resolved, VerbMeshLeveling)
	}

	if has(macroLoadFilament) {
		carry(claimed, resolved, VerbLoadFilament, ArgLoadFilament)
	}

	if has(macroUnloadFilament) {
		carry(claimed, resolved, VerbUnloadFilament)
	}

	if m.power != "" {
		carry(claimed, resolved, VerbPower, ArgPower)
	}

	return claimed
}

// carry copies keys from the resolved set into the machine's tier unchanged.
func carry(into, from connectorapi.Set, keys ...connectorapi.Capability) {
	for _, key := range keys {
		if value, declared := from[key]; declared {
			into[key] = value
		}
	}
}

// carryHeater copies one temperature verb and narrows its argument to the range
// the machine's own configuration declares.
func carryHeater(into, from connectorapi.Set, verb, argument connectorapi.Capability, configured heaterConfig) {
	carry(into, from, verb)

	declared, present := from[argument]
	if !present {
		return
	}

	if configured.MaxTemp != nil && (!declared.HasLimit || *configured.MaxTemp < declared.Limit) {
		declared.Limit = math.Floor(*configured.MaxTemp)
		declared.HasLimit = true
	}

	into[argument] = declared
}

// carryTool copies the head argument of the nozzle verb and bounds it to the
// last head the machine lists.
func carryTool(into, from connectorapi.Set, extruders int) {
	declared, present := from[ArgNozzleTool]
	if !present || extruders < 1 {
		return
	}

	declared.Limit = float64(extruders - 1)
	declared.HasLimit = true
	into[ArgNozzleTool] = declared
}

// heaterBounds reads the heater sections out of the machine's configuration
// dump. A section that is not there, or that cannot be read, is absent: the
// bounds only narrow, so failing to read one leaves the ceiling standing.
func heaterBounds(dumped configDump, sections ...string) map[string]heaterConfig {
	out := make(map[string]heaterConfig, len(sections))

	for _, section := range sections {
		raw, present := dumped.Settings[section]
		if !present {
			continue
		}

		var configured heaterConfig
		if json.Unmarshal(raw, &configured) != nil {
			continue
		}

		out[section] = configured
	}

	return out
}

// methodFor is the Moonraker call one verb becomes, and ok is false for a verb
// this driver sends as G-code instead.
func methodFor(verb connectorapi.Capability) (method string, ok bool) {
	switch verb {
	case VerbPausePrint:
		return methodPrintPause, true
	case VerbResumePrint:
		return methodPrintResume, true
	case VerbStopPrint:
		return methodPrintCancel, true
	case VerbResetPrinter:
		return methodRestart, true
	case VerbReset:
		return methodFirmwareRestart, true
	default:
		return "", false
	}
}

// args reads the validated arguments of one command. The host checked each
// against its descriptor, so a value that does not parse is this driver being
// reached in a way it was not declared for.
type args struct {
	verb connectorapi.Capability
	raw  map[string]string
}

func (a args) has(name string) bool {
	_, ok := a.raw[name]

	return ok
}

func (a args) number(name string) (float64, error) {
	value, err := strconv.ParseFloat(strings.TrimSpace(a.raw[name]), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, malformed(a.verb, name, a.raw[name])
	}

	return value, nil
}

func (a args) whole(name string) (int, error) {
	value, err := strconv.Atoi(strings.TrimSpace(a.raw[name]))
	if err != nil {
		return 0, malformed(a.verb, name, a.raw[name])
	}

	return value, nil
}

// malformed is the refusal for a value the descriptor should have stopped.
func malformed(verb connectorapi.Capability, name, value string) error {
	return connectorapi.ArgumentMalformed.New(fmt.Sprintf("the value %q is not one this driver can send", value)).
		With(fault.StringParam("verb", verb.String()),
			fault.StringParam("argument", name))
}

// gcodeFor is the G-code script one command becomes, for the verbs Moonraker
// has no call of its own for. ignored names the arguments the machine cannot
// act on and drops.
func gcodeFor(cmd connectorapi.Command, objectNames func(int) (string, bool)) (script string, ignored []string, err error) {
	a := args{verb: cmd.Verb, raw: cmd.Args}

	switch cmd.Verb {
	case VerbNozzleTarget:
		script, err = nozzleScript(a)
	case VerbBedTarget:
		script, err = heaterScript(a, heaterBed, argBed)
	case VerbChamberTarget:
		script, err = heaterScript(a, heaterChamber, argChamber)
	case VerbSetSpeed:
		script, err = factorScript(a, "M220", argSpeed)
	case VerbSetFlow:
		script, err = factorScript(a, "M221", argFlow)
	case VerbHome:
		script, err = homeScript(a)
	case VerbMove:
		script, err = moveScript(a)
	case VerbMoveZ:
		script, err = relativeScript(a, "Z", argDistance)
	case VerbMoveE:
		script, err = extrudeScript(a)
	case VerbDisableSteppers:
		script = "M84"
	case VerbSetIdle:
		script = "SDCARD_RESET_FILE"
	case VerbSendGcode:
		script, err = rawScript(a)
	case VerbCancelObject:
		script, err = cancelObjectScript(a, objectNames)
	case VerbMeshLeveling:
		script = "BED_MESH_CALIBRATE"
	case VerbLoadFilament:
		script = "LOAD_FILAMENT"
		if a.has(argFilament) {
			ignored = []string{argFilament}
		}
	case VerbUnloadFilament:
		script = "UNLOAD_FILAMENT"
	default:
		err = connectorapi.CapabilityNotSupported.New("this driver has no command for that verb").
			With(fault.StringParam("verb", cmd.Verb.String()))
	}

	return script, ignored, err
}

// heaterScript sets one heater's target with Klipper's own command, which
// returns as soon as the target is set.
func heaterScript(a args, heater, name string) (string, error) {
	degrees, err := a.whole(name)
	if err != nil {
		return "", err
	}

	return "SET_HEATER_TEMPERATURE HEATER=" + heater + " TARGET=" + strconv.Itoa(degrees), nil
}

// nozzleScript is a nozzle target. With a head named, it sets that extruder's
// heater. Without one, it is M104, which Klipper applies to the active
// extruder.
func nozzleScript(a args) (string, error) {
	if !a.has(argTool) {
		degrees, err := a.whole(argNozzle)
		if err != nil {
			return "", err
		}

		return "M104 S" + strconv.Itoa(degrees), nil
	}

	tool, err := a.whole(argTool)
	if err != nil || tool < 0 {
		return "", malformed(a.verb, argTool, a.raw[argTool])
	}

	return heaterScript(a, extruderName(tool), argNozzle)
}

// factorScript sets the speed or flow override, in percent.
func factorScript(a args, code, name string) (string, error) {
	percent, err := a.whole(name)
	if err != nil {
		return "", err
	}

	return code + " S" + strconv.Itoa(percent), nil
}

// homeScript homes the axes named, or every axis. The caller may write "XY",
// "X Y" or "x,y"; Klipper reads only "X Y", where "XY" would home X alone.
func homeScript(a args) (string, error) {
	named := strings.ToUpper(a.raw[argAxis])

	var axes []string

	for _, letter := range named {
		switch letter {
		case 'X', 'Y', 'Z':
			if !slices.Contains(axes, string(letter)) {
				axes = append(axes, string(letter))
			}
		case ' ', ',', ';':
		default:
			return "", malformed(a.verb, argAxis, a.raw[argAxis])
		}
	}

	if len(axes) == 0 {
		return "G28", nil
	}

	return "G28 " + strings.Join(axes, " "), nil
}

// moveScript is a relative XY move. The previous positioning mode is restored
// afterwards, so a jog never leaves the machine in relative mode.
func moveScript(a args) (string, error) {
	feedrate, err := a.whole(argFeedrate)
	if err != nil {
		return "", err
	}

	var axes strings.Builder

	for _, axis := range []struct{ name, letter string }{{argX, "X"}, {argY, "Y"}} {
		if !a.has(axis.name) {
			continue
		}

		distance, numErr := a.number(axis.name)
		if numErr != nil {
			return "", numErr
		}

		axes.WriteString(" " + axis.letter + formatMM(distance))
	}

	if axes.Len() == 0 {
		return "", connectorapi.ArgumentMalformed.New("a move needs x, y or both").
			With(fault.StringParam("verb", a.verb.String()), fault.StringParam("argument", argX))
	}

	return relativeMove(axes.String(), feedrate), nil
}

// relativeScript is a relative move along one axis.
func relativeScript(a args, letter, name string) (string, error) {
	feedrate, err := a.whole(argFeedrate)
	if err != nil {
		return "", err
	}

	distance, err := a.number(name)
	if err != nil {
		return "", err
	}

	return relativeMove(" "+letter+formatMM(distance), feedrate), nil
}

// relativeMove wraps one G1 in Klipper's saved G-code state, so the
// positioning mode the machine was in before is the one it is in after.
func relativeMove(axes string, feedrate int) string {
	return "SAVE_GCODE_STATE NAME=prusa_connect_jog\nG91\nG1" + axes + " F" + strconv.Itoa(feedrate) +
		"\nRESTORE_GCODE_STATE NAME=prusa_connect_jog"
}

// extrudeScript extrudes or retracts. M83 makes the move relative, and the
// saved state puts the extruder mode back afterwards.
func extrudeScript(a args) (string, error) {
	feedrate, err := a.whole(argFeedrate)
	if err != nil {
		return "", err
	}

	length, err := a.number(argLength)
	if err != nil {
		return "", err
	}

	return "SAVE_GCODE_STATE NAME=prusa_connect_extrude\nM83\nG1 E" + formatMM(length) + " F" +
		strconv.Itoa(feedrate) + "\nRESTORE_GCODE_STATE NAME=prusa_connect_extrude", nil
}

// formatMM writes a distance to the micrometre, with no trailing zeros.
func formatMM(distance float64) string {
	return strconv.FormatFloat(math.Round(distance*1000)/1000, 'f', -1, 64)
}

// rawScript passes the caller's G-code on. Klipper reads a newline as the end
// of one command, which is how a console sends several.
func rawScript(a args) (string, error) {
	script := strings.ReplaceAll(a.raw[argGcode], "\r\n", "\n")
	script = strings.TrimSpace(strings.ReplaceAll(script, "\r", "\n"))

	if script == "" {
		return "", malformed(a.verb, argGcode, a.raw[argGcode])
	}

	return script, nil
}

// cancelObjectScript excludes one object of the running print. Klipper
// addresses an object by its name, and the id is its place in the list
// exclude_object reports.
func cancelObjectScript(a args, objectNames func(int) (string, bool)) (string, error) {
	id, err := a.whole(argObjectID)
	if err != nil {
		return "", err
	}

	name, known := objectNames(id)
	if !known {
		return "", connectorapi.ArgumentMalformed.New("the running print has no object with that id").
			With(fault.StringParam("verb", a.verb.String()), fault.StringParam("argument", argObjectID))
	}

	if strings.ContainsAny(name, "\n\r") {
		return "", fmt.Errorf("klipper: the object name %q cannot be sent as G-code", name)
	}

	// Klipper reads an extended parameter up to the next KEY=, so a name with
	// a space in it is passed as it is, without quotes.
	return "EXCLUDE_OBJECT NAME=" + name, nil
}
