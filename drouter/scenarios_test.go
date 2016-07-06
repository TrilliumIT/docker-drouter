package drouter

import (
	"testing"

	log "github.com/Sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

func TestDefault(t *testing.T) {
	require.NoError(t, cleanup(), "Failed to cleanup()")

	opts := defaultOpts()

	st, err := newSimulation(opts, t)
	require.NoError(t, err, "Failed to create simulation.")

	st.cb.assertInit = func() {
		st.assertAggressiveInit()
		st.assertSpecificRoutesOnInit()
	}

	st.cb.assertC2Start = st.assertSpecificRoutesOnC2Start
	st.cb.assertN3Add = func() {
		st.assertAggressiveN3Add()
		st.assertSpecificRoutesOnN3Add()
	}

	st.require.NoError(st.runV4(), "Scenario failed to run.")
}

func TestNoAggressive(t *testing.T) {
	require.NoError(t, cleanup(), "Failed to cleanup()")

	opts := defaultOpts()
	opts.Aggressive = false

	st, err := newSimulation(opts, t)
	require.NoError(t, err, "Failed to create simulation.")

	st.cb.assertInit = func() {
		warns := 0
		for _, e := range hook.Entries {
			if e.Level == log.WarnLevel {
				warns++
			}
			st.assert.True(e.Level >= log.WarnLevel, "All logs should be >= Warn, but observed log: ", e)
		}
		st.assert.Equal(warns, 1, "Should have recieved one warning message for running in Aggressive with no tranist net")
		hook.Reset()

		st.assertSpecificRoutesOnInit()
	}

	st.cb.assertC2Start = st.assertSpecificRoutesOnC2Start
	st.cb.assertN3Add = st.assertSpecificRoutesOnN3Add

	st.require.NoError(st.runV4(), "Scenario failed to run.")
}

func TestHostShortcut(t *testing.T) {
	require.NoError(t, cleanup(), "Failed to cleanup()")

	log.SetLevel(log.DebugLevel)

	opts := defaultOpts()
	opts.HostShortcut = true

	st, err := newSimulation(opts, t)
	require.NoError(t, err, "Failed to create simulation.")

	//TODO, actually, you know, test stuff, write the callbacks
	st.cb.assertInit = func() {
		st.assertAggressiveInit()
		st.assertSpecificRoutesOnInit()
	}

	st.cb.assertC2Start = st.assertSpecificRoutesOnC2Start
	st.cb.assertN3Add = func() {
		st.assertAggressiveN3Add()
		st.assertSpecificRoutesOnN3Add()
	}

	st.require.NoError(st.runV4(), "Scenario failed to run.")

	log.SetLevel(log.InfoLevel)
}

func (st *simulation) assertSpecificRoutesOnC2Start() {
	st.assert.False(st.handleContainsRoute(st.hns, testNets[0], nil), "host should not have route to n0.")
	st.assert.Equal(hostShortcut, st.handleContainsRoute(st.hns, testNets[1], nil), "host should have route to n1 with hostShortcut.")
	st.assert.Equal(hostShortcut, st.handleContainsRoute(st.hns, testNets[2], nil), "host should have route to n2 with hostShortcut.")

	st.assert.False(st.handleContainsRoute(st.c[1].handle, testNets[0], nil), "c1 should not have a route to n0.")
	st.assert.True(st.handleContainsRoute(st.c[1].handle, testNets[2], nil), "c1 should have a route to n2.")

	st.assert.False(st.handleContainsRoute(st.c[2].handle, testNets[0], nil), "c2 should not have a route to n0.")
	st.assert.True(st.handleContainsRoute(st.c[2].handle, testNets[1], nil), "c2 should have a route to n1.")
}

func (st *simulation) assertAggressiveInit() {
	_, ok := st.dr.getNetwork(st.n[0].ID)
	st.assert.True(ok, "should have learned n0 by now.")

	_, ok = st.dr.getNetwork(st.n[2].ID)
	st.assert.True(ok, "should have learned n2 by now.")
}

func (st *simulation) assertAggressiveN3Add() {
	_, ok := st.dr.getNetwork(st.n[3].ID)
	st.assert.True(ok, "should have learned n3 by now.")
}

func (st *simulation) assertSpecificRoutesOnInit() {
	st.assert.False(st.handleContainsRoute(st.hns, testNets[0], nil), "host should not have route to n0.")
	st.assert.Equal(hostShortcut, st.handleContainsRoute(st.hns, testNets[1], nil), "host should have route to n1 with hostShortcut.")
	st.assert.Equal(hostShortcut && aggressive, st.handleContainsRoute(st.hns, testNets[2], nil), "host should have a route to n2 in aggressive && hostShortcut")

	st.assert.Equal(aggressive, st.handleContainsRoute(st.c[1].handle, testNets[2], nil), "c1 should have a route to n2 in aggressive.")
}

func (st *simulation) assertSpecificRoutesOnN3Add() {
	st.assert.Equal(aggressive, st.handleContainsRoute(st.c[1].handle, testNets[3], nil), "c1 should have a route to n3 if in aggressive.")
	st.assert.Equal(aggressive, st.handleContainsRoute(st.c[2].handle, testNets[3], nil), "c2 should have a route to n3 if in aggressive.")
	st.assert.Equal(hostShortcut && aggressive, st.handleContainsRoute(st.hns, testNets[3], nil), "host should have a route to n3 in aggressive && hostShortcut")

	st.assertSpecificRoutesOnC2Start()
}
