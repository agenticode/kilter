package main

import (
	"testing"

	"github.com/agenticode/kilter/pkg/plan"
)

func TestDropOrphanedNodeSteps(t *testing.T) {
	steps := []plan.Step{
		{Seq: 1, Type: plan.StepCordonNode, Node: "a"},
		{Seq: 2, Type: plan.StepEvictPod, Node: "a", Pod: "d/p1"},
		{Seq: 3, Type: plan.StepDeleteNode, Node: "a"},
		// node b's cordon was filtered by a cooldown → its whole sequence must drop
		{Seq: 5, Type: plan.StepEvictPod, Node: "b", Pod: "d/p2"},
		{Seq: 6, Type: plan.StepDeleteNode, Node: "b"},
		{Seq: 7, Type: plan.StepResizeWorkload},
	}
	out := dropOrphanedNodeSteps(steps)
	if len(out) != 4 {
		t.Fatalf("want 4 steps, got %d: %+v", len(out), out)
	}
	for _, s := range out {
		if s.Node == "b" {
			t.Fatalf("orphaned node-b step survived: %+v", s)
		}
	}
}

func TestDropOrphanedKeepsCompleteSequences(t *testing.T) {
	steps := []plan.Step{
		{Seq: 1, Type: plan.StepCordonNode, Node: "a"},
		{Seq: 2, Type: plan.StepDeleteNode, Node: "a"},
	}
	if got := dropOrphanedNodeSteps(steps); len(got) != 2 {
		t.Fatalf("complete sequence must survive, got %d", len(got))
	}
}
