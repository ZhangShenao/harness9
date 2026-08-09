// Package router provides the smart routing layer that decides whether
// a user task goes to the Fast Lane (existing engine.Run) or the Deep
// Lane (Mission Control with decomposition, parallel workers, and verification).
package router

import (
	"strings"
)

// Lane identifies which execution path a task should take.
type Lane string

const (
	Fast Lane = "fast"
	Deep Lane = "deep"
)

// Decision describes the routing outcome.
type Decision struct {
	Lane   Lane
	Reason string
	Goal   string
}

// complexitySignals are keywords that suggest a task needs decomposition.
var complexitySignals = []string{
	"重构", "refactor", "实现", "implement", "跨包", "cross-package",
	"并测试", "and test", "并文档", "and doc", "多文件", "multi-file",
	"迁移", "migrate", "重写", "rewrite", "集成", "integrate",
}

// Route evaluates user input and decides which lane to use.
// Heuristic-only for now; LLM triage is a future enhancement (fail-open to Fast).
func Route(input string) Decision {
	input = strings.TrimSpace(input)
	if input == "" {
		return Decision{Lane: Fast, Reason: "empty input"}
	}

	// Explicit /mission prefix forces Deep Lane
	if strings.HasPrefix(input, "/mission ") || input == "/mission" {
		goal := strings.TrimPrefix(input, "/mission ")
		return Decision{Lane: Deep, Reason: "explicit /mission prefix", Goal: goal}
	}

	// Heuristic: check for complexity signals
	lower := strings.ToLower(input)
	for _, signal := range complexitySignals {
		if strings.Contains(lower, strings.ToLower(signal)) {
			return Decision{Lane: Deep, Reason: "complexity signal: " + signal, Goal: input}
		}
	}

	// Default: Fast Lane (fail-open, never block user)
	return Decision{Lane: Fast, Reason: "default fast lane", Goal: input}
}
