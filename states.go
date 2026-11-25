package klipper

// The states this driver reports at /state, the moves between them, and the
// verbs each one accepts. The words are what stateOf in telemetry.go writes.
//
// The accepts lists are a hint for which controls a client offers. Klipper's
// own answer to the command still decides.

import (
	"slices"

	"github.com/prusa3d/prusa-connect/sdk/connectorapi"
)

// The state words.
const (
	stateIdle     connectorapi.StateName = "IDLE"
	statePrinting connectorapi.StateName = "PRINTING"
	statePaused   connectorapi.StateName = "PAUSED"
	stateFinished connectorapi.StateName = "FINISHED"
	stateStopped  connectorapi.StateName = "STOPPED"
	stateError    connectorapi.StateName = "ERROR"
	stateBusy     connectorapi.StateName = "BUSY"
)

// everyState is every word this driver writes.
func everyState() []connectorapi.StateName {
	return []connectorapi.StateName{
		stateIdle, statePrinting, statePaused, stateFinished, stateStopped, stateError, stateBusy,
	}
}

// quiet is the states with no print on the machine.
func quiet() []connectorapi.StateName {
	return []connectorapi.StateName{stateIdle, stateFinished, stateStopped}
}

// stateGuards lists the states a guarded verb is accepted in. A verb this map
// does not name is accepted in every state but ERROR: Klipper refuses almost
// every command while it is shut down.
//
//nolint:gochecknoglobals // a lookup table, read-only after init.
var stateGuards = map[connectorapi.Capability][]connectorapi.StateName{
	VerbStartPrint:      quiet(),
	VerbSetIdle:         {stateFinished, stateStopped},
	VerbPausePrint:      {statePrinting},
	VerbResumePrint:     {statePaused},
	VerbStopPrint:       {statePrinting, statePaused},
	VerbCancelObject:    {statePrinting, statePaused},
	VerbHome:            quiet(),
	VerbMove:            quiet(),
	VerbMoveZ:           append(quiet(), statePaused),
	VerbMoveE:           append(quiet(), statePaused),
	VerbDisableSteppers: quiet(),
	VerbMeshLeveling:    quiet(),
	VerbLoadFilament:    append(quiet(), statePaused),
	VerbUnloadFilament:  append(quiet(), statePaused),
	VerbSendStateInfo:   everyState(),
	VerbSendInfo:        everyState(),
	VerbSendFileInfo:    everyState(),
	VerbSendGcode:       everyState(),
	VerbResetPrinter:    everyState(),
	VerbReset:           everyState(),
	// Moonraker refuses to switch a device locked while printing, and this
	// keeps the control away from a running print as well.
	VerbPower: {stateIdle, stateFinished, stateStopped, stateError, stateBusy},
}

// stateNames is the words and what a person sees for each.
//
//nolint:gochecknoglobals // a fixed list, read-only after init.
var stateNames = []connectorapi.DeviceState{
	{Name: stateIdle, DisplayName: "Idle"},
	{Name: statePrinting, DisplayName: "Printing"},
	{Name: statePaused, DisplayName: "Paused"},
	{Name: stateFinished, DisplayName: "Finished"},
	{Name: stateStopped, DisplayName: "Stopped"},
	{Name: stateError, DisplayName: "Error"},
	{Name: stateBusy, DisplayName: "Busy"},
}

// stateTransitions is every move between two words. A move with no verb is one
// the machine makes on its own, such as a print started from Mainsail.
//
//nolint:gochecknoglobals // a fixed list, read-only after init.
var stateTransitions = []connectorapi.Transition{
	{From: stateIdle, To: statePrinting, Verb: VerbStartPrint},
	{From: stateFinished, To: statePrinting, Verb: VerbStartPrint},
	{From: stateStopped, To: statePrinting, Verb: VerbStartPrint},
	{From: stateIdle, To: statePrinting},
	{From: stateFinished, To: statePrinting},
	{From: stateStopped, To: statePrinting},
	{From: statePrinting, To: statePaused, Verb: VerbPausePrint},
	{From: statePaused, To: statePrinting, Verb: VerbResumePrint},
	{From: statePrinting, To: stateStopped, Verb: VerbStopPrint},
	{From: statePaused, To: stateStopped, Verb: VerbStopPrint},
	{From: statePrinting, To: stateFinished},
	{From: statePrinting, To: stateError},
	{From: statePaused, To: stateError},
	{From: stateIdle, To: stateError},
	{From: stateFinished, To: stateError},
	{From: stateStopped, To: stateError},
	{From: stateFinished, To: stateIdle, Verb: VerbSetIdle},
	{From: stateStopped, To: stateIdle, Verb: VerbSetIdle},
	{From: stateFinished, To: stateIdle},
	{From: stateStopped, To: stateIdle},
	{From: stateError, To: stateIdle, Verb: VerbResetPrinter},
	{From: stateError, To: stateIdle, Verb: VerbReset},
	{From: stateError, To: stateIdle},
	{From: stateIdle, To: stateBusy},
	{From: stateBusy, To: stateIdle},
	{From: stateBusy, To: stateError},
}

// states is this driver's declared state set.
func states() connectorapi.StateSet {
	verbs := baselineVerbs()

	declared := make([]connectorapi.DeviceState, 0, len(stateNames))

	for _, state := range stateNames {
		state.Accepts = make([]connectorapi.Capability, 0, len(verbs))

		for _, verb := range verbs {
			if acceptedIn(verb, state.Name) {
				state.Accepts = append(state.Accepts, verb)
			}
		}

		declared = append(declared, state)
	}

	return connectorapi.StateSet{States: declared, Transitions: slices.Clone(stateTransitions)}
}

// acceptedIn reports whether a state accepts a verb.
func acceptedIn(verb connectorapi.Capability, state connectorapi.StateName) bool {
	if guard, guarded := stateGuards[verb]; guarded {
		return slices.Contains(guard, state)
	}

	return state != stateError
}

// baselineVerbs is every verb of the baseline, in a fixed order, without its
// arguments.
func baselineVerbs() []connectorapi.Capability {
	var verbs []connectorapi.Capability

	for _, key := range (&Driver{}).Baseline().Keys() {
		if _, isArgument := key.Argument(); !isArgument {
			verbs = append(verbs, key)
		}
	}

	return verbs
}
