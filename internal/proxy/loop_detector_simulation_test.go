package proxy

import (
	"fmt"
	"testing"
)

// Simulate 4 identical tool calls (same name + same input) and show what the loop detector does.
func TestSimulateIdenticalLoop_Simulation(t *testing.T) {
	// 4x the SAME tool call — Read on /src/foo.go with same input
	msgs := buildMessages(
		userMsg("fix the bug"),
		assistantWithToolUse("Read", "/src/foo.go"),
		toolResult(false, "file content"),
		assistantWithToolUse("Read", "/src/foo.go"),
		toolResult(false, "file content"),
		assistantWithToolUse("Read", "/src/foo.go"),
		toolResult(false, "file content"),
		assistantWithToolUse("Read", "/src/foo.go"),
		toolResult(false, "file content"),
	)

	// 1) raw signal
	sig := DetectLoop(msgs)
	if sig == nil {
		fmt.Println(">>> DetectLoop: nil (no detection)")
	} else {
		fmt.Printf(">>> DetectLoop: type=%d description=%s\n",
			sig.Type, sig.Description)
	}

	// 2) formatted warning via CheckLoopAndFormat (with state, simulating real proxy path)
	state := &LoopState{}
	warning, level := CheckLoopAndFormat(msgs, state)
	if warning == "" {
		fmt.Println(">>> CheckLoopAndFormat: no warning (empty)")
	} else {
		fmt.Printf(">>> CheckLoopAndFormat: level=%d\n%s\n", level, warning)
	}
	state.RecordWarning()
	fmt.Printf(">>> After RecordWarning: cooldownLeft=%d inCooldown=%v\n",
		state.cooldownLeft, state.InCooldown())
}
