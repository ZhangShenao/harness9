package router

import (
	"testing"
)

func TestRouteEmpty(t *testing.T) {
	d := Route("")
	if d.Lane != Fast {
		t.Fatalf("empty input should be Fast, got %s", d.Lane)
	}
}

func TestRouteExplicitMission(t *testing.T) {
	d := Route("/mission implement feature X with tests and docs")
	if d.Lane != Deep {
		t.Fatalf("explicit /mission should be Deep")
	}
	if d.Goal != "implement feature X with tests and docs" {
		t.Fatalf("goal = %q", d.Goal)
	}
}

func TestRouteComplexitySignals(t *testing.T) {
	cases := []string{
		"重构这个模块",
		"implement the feature and test it",
		"跨包迁移配置",
		"rewrite the scheduler with multi-file support",
	}
	for _, input := range cases {
		d := Route(input)
		if d.Lane != Deep {
			t.Fatalf("complexity signal %q should be Deep, got Fast (%s)", input, d.Reason)
		}
	}
}

func TestRouteSimpleTask(t *testing.T) {
	d := Route("fix the typo in README")
	if d.Lane != Fast {
		t.Fatalf("simple task should be Fast, got Deep (%s)", d.Reason)
	}
}

func TestRouteSimpleQuestion(t *testing.T) {
	d := Route("how does the scheduler work?")
	if d.Lane != Fast {
		t.Fatalf("question should be Fast, got Deep (%s)", d.Reason)
	}
}
